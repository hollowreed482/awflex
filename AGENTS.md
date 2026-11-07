# Operating awflex: guide for agents

Read this before deploying awflex or changing anything when it does not work.

## Rules

1. **Run `awflex doctor` first and follow its `fix` lines.** It checks the real
   deployment step by step and names the exact fix for each failed check.
2. **Never edit awflex's source code to fix a deployment.** Every setting is an
   environment variable, a manifest field, or the image you build. If you think
   the code must change, stop and report what doctor printed.
3. **Never run the pod as root** (`runAsUser: 0`). If ssh fails for the user, the
   image is missing a passwd entry; see the table below.
4. **Keep your site's files out of this repo.** Put your manifest copy, image
   build, scripts and notes in your own repository. Do not commit them to a
   clone of awflex, and do not push to it.
5. **Change one thing, then run doctor again.** Do not stack guesses.

## Deploy, in order

```sh
# 1. Image. Use the ansible image your playbooks already run in as the base.
#    Use a new tag for every build.
docker build -f deploy/Dockerfile.base --build-arg BASE_IMAGE=<your ansible image> \
  -t registry.example.com/awflex:0.4.0 .
docker push registry.example.com/awflex:0.4.0

# 2. Namespace. Use one that exists; the manifest creates none.
NS=<namespace>

# 3. Pull secret, only for a private registry. Then uncomment imagePullSecrets
#    in deploy/k8s/awflex.yaml.
kubectl -n $NS create secret docker-registry registry-pull \
  --docker-server=registry.example.com --docker-username=USER --docker-password=TOKEN

# 4. Tokens.
kubectl -n $NS create secret generic awflex-auth \
  --from-literal=AWFLEX_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=AWFLEX_HOOK_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=AWFLEX_SECRET_KEY="$(openssl rand -hex 32)"

# 5. In your copy of deploy/k8s/awflex.yaml set: AWFLEX_REPO, AWFLEX_INVENTORY,
#    the image, and storage (no volume claims allowed? follow the comment at the
#    data volume to use NFS). Then:
kubectl -n $NS apply -f awflex.yaml
kubectl -n $NS rollout status deploy/awflex

# 6. The node SSH key (read from a file, never an argument).
kubectl -n $NS exec -i deploy/awflex -- sh -c 'cat > /run/awflex/scratch/k' < ~/.ssh/fleet_key
kubectl -n $NS exec deploy/awflex -- awflex cred set fleet --ssh-user root --ssh-key-file /run/awflex/scratch/k
kubectl -n $NS exec deploy/awflex -- rm /run/awflex/scratch/k

# 7. Check everything, including a real connection to one node.
kubectl -n $NS exec deploy/awflex -- awflex doctor --host <node>

# 8. One real run.
kubectl -n $NS exec deploy/awflex -- awflex run <node> --wait
kubectl -n $NS exec deploy/awflex -- awflex log <node>
```

Inside the pod the CLI needs no URL or token: it uses the pod's own settings.
From outside, set `AWFLEX_URL` and `AWFLEX_TOKEN` (the admin token).

## When a run does not succeed

`awflex get <host>` returns the record. Read these fields in order:

1. `hint`: the likely cause and the exact fix, for known failures.
2. `error`: what awflex or ansible reported.
3. `failed_tasks`: the task that failed and its message.

Then `awflex log <host>` for that host's tasks, and `awflex log <host> --raw` for
the whole ansible output of its batch.

## Symptoms

| Symptom | Cause | Fix |
|---|---|---|
| Pod `ImagePullBackOff` | The registry needs credentials, or the image name or tag is wrong. | Deploy step 3. Check the image name with `kubectl describe pod`. |
| Pod stuck `ContainerCreating`, mount error in `kubectl describe pod` | The volume cannot be mounted: wrong NFS server or path, or claims are not allowed. | Fix the `nfs:` server and path. The export must be writable by uid 1000. |
| Pod `CrashLoopBackOff` | A setting is wrong. | `kubectl logs deploy/awflex`: the last line names the setting. |
| New build, but the pod still runs the old code | The tag was reused and the node kept its cached image. | Use a new tag per build, or set `imagePullPolicy: Always`. Check with `kubectl exec deploy/awflex -- awflex version`. |
| `No user exists for uid 1000` | The image has no passwd entry for the pod's user, so ssh exits. | Build with `deploy/Dockerfile.base`, which adds one. Keep `runAsUser: 1000`. Do not run as root. |
| Run `error` with `Could not load ... callback plugin` | The pod env names a callback the image does not have. | Remove `ANSIBLE_STDOUT_CALLBACK` and unknown names in `ANSIBLE_CALLBACKS_ENABLED` from the pod env. awflex uses the default stdout callback itself; a repo's `stdout_callback` cannot stop a run. |
| Run `error`: `couldn't resolve module/action`, `role ... was not found` | The image lacks a collection or role the playbook uses. | Build with `deploy/Dockerfile.base` on the image your playbooks already run in. |
| Host `error`: `no tasks ran for this host` or `no hosts to target` | The host is not in the inventory, or the play's `hosts:` pattern excludes it. | `awflex doctor --host <host>` shows which. With a generated inventory, set `AWFLEX_GROUPS` to the group the play targets. |
| Host `unreachable`: `Permission denied (publickey)` | The node refused the key. | Deploy step 6. Check the user and that the public key is on the node and any jump host. |
| Host `unreachable`: `Host key verification failed` | The nested ssh inside a `ProxyCommand` checked a host key. | Add `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null` to that inner ssh command. |
| Host `unreachable`: timed out, no route, could not resolve | The pod cannot reach or resolve the node. | Check DNS and network from the pod, or route through a jump host (`ProxyJump` in `ansible_ssh_common_args`). |
| Repo `error`: `Authentication failed`, `could not read Username` | A private repo without a git credential. | `awflex cred set git --git-user U --git-token-file F` |
| `kubectl port-forward` says `lost connection to pod` after a rollout | The forward still points at the old pod. | Stop it and start a new one. The run itself was not affected. |

Anything not in this table: run `awflex doctor --host <host>`, then report its
full JSON output and the host's `awflex get` record.
