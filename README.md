# awflex

A small replacement for AWX on Kubernetes, for one job: nodes of a large cluster
call in when they boot and get the main playbook run on them.

One Go binary in one pod. No database, no Redis, no UI, no operator, no pod per job.
Agents and people drive it through REST or the CLI; both speak JSON.

```
node boots ──POST /api/v1/hook?host=me&branch=X──▶ awflex ──ssh──▶ ansible-playbook on that node
agent      ──awflex run / get / log / status─────▶   │
                                                     └─ /data: queue + last run per host
```

## How it works

1. **A call is accepted and written to disk.** The hook returns `202` at once. The
   host's record (`/data/runs/<host>.json`) is written as `queued` and fsynced
   before the reply.
2. **Callers share ansible runs.** Calls for the same branch and playbook are
   collected for a fixed 10 seconds from the first caller, then run as one
   `ansible-playbook` process over all of them (up to 100 hosts; a full batch
   starts early). 1000 nodes booting together cost about ten ansible runs, not
   a thousand.
3. **A fork budget caps memory.** `AWFLEX_MAX_FORKS` is the total number of
   ansible forks across all running batches. When it is used up, new callers
   keep collecting and start when a run ends.
4. **Each host gets its own result.** A callback plugin (embedded in the binary)
   records every task result per host, so a host in a 100-host batch still has
   its own status, failed task, and log.
5. **Only the last run per host is kept.** A new run on a host replaces its
   record and log, whatever playbook it was. Raw ansible output is deleted once
   no host's last run points at it.

### Restart and crash behavior

The host record is the queue and the finish marker. It moves `queued` →
`running` → a final status, and the final status is written only when ansible
reports its summary for that host.

| What happens | Result |
|---|---|
| awflex or its node dies | On start, every record still `queued` or `running` is re-queued and run again. |
| Pod is deleted or rolled (SIGTERM) | Running ansible is stopped, unfinished hosts go back to `queued` on disk, the next pod resumes them. The attempt is not counted. |
| ansible is killed mid-run (for example out of memory) | Hosts without a finish are re-queued automatically. |
| A request keeps killing ansible or the pod | After `AWFLEX_MAX_ATTEMPTS` (3) starts it becomes `error`, so one bad request cannot loop forever. |

Playbooks must be safe to run twice, since a host interrupted mid-run starts over.
Run one replica: the volume has a single writer.

### Statuses

`queued`, `running`, then one of `successful`, `failed` (a task failed),
`unreachable` (ssh could not reach the host), `error` (awflex, git or ansible
could not run the play: unknown branch, missing playbook, host not in inventory),
`canceled`, `timeout`.

## Deploy

```sh
docker build -t registry.example.com/awflex:0.1.0 . && docker push registry.example.com/awflex:0.1.0

kubectl create namespace awflex
kubectl -n awflex create secret generic awflex-auth \
  --from-literal=AWFLEX_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=AWFLEX_SECRET_KEY="$(openssl rand -hex 32)"

# set AWFLEX_REPO and the image in deploy/k8s/awflex.yaml, then
kubectl apply -f deploy/k8s/awflex.yaml
```

The manifest is a Deployment (1 replica, `Recreate`), a 2Gi volume, a ConfigMap and
a Service. The pod runs as a non-root user with a read-only root filesystem and no
Kubernetes API access. Nodes outside the cluster need the Service exposed
(NodePort, LoadBalancer or Ingress).

Collections are baked into the image:
`docker build --build-arg ANSIBLE_PACKAGES="ansible-core netaddr" .`, or add an
`ansible-galaxy collection install` layer.

### Configuration

There is one playbook repo and one set of run settings, fixed at deploy time.

| Variable | Default | Meaning |
|---|---|---|
| `AWFLEX_REPO` | required | Git URL of the playbook repo. |
| `AWFLEX_BRANCH` | `main` | Branch when a caller names none. |
| `AWFLEX_PLAYBOOK` | `main.yml` | Playbook when a caller names none. |
| `AWFLEX_INVENTORY` | unset | Comma list of inventories in the repo; runs use `--limit` on the calling hosts. Unset: an inventory is built from the calling hosts. |
| `AWFLEX_GROUPS` | unset | Groups to put hosts in when the inventory is built. |
| `AWFLEX_EXTRA_VARS` | unset | JSON object applied to every run. |
| `AWFLEX_BATCH_WINDOW` | `10` | Seconds callers are collected into one ansible run. |
| `AWFLEX_MAX_BATCH` | `100` | Hosts per ansible run. `1` gives one run per host. |
| `AWFLEX_FORKS` | `25` | Forks per ansible run. |
| `AWFLEX_MAX_FORKS` | `50` | Forks across all runs. The memory knob. |
| `AWFLEX_MAX_ATTEMPTS` | `3` | Starts before a never-finishing request is given up. |
| `AWFLEX_TIMEOUT` | `2h` | Limit for one ansible run. |
| `AWFLEX_FETCH_TTL` | `10` | Seconds a git fetch is reused. |
| `AWFLEX_ADMIN_TOKEN` | required | Bearer token for everything. |
| `AWFLEX_READ_TOKEN` | unset | Bearer token for read-only routes. |
| `AWFLEX_HOOK_TOKEN` | unset | If set, nodes must send it; otherwise the hook is open. |
| `AWFLEX_SECRET_KEY` | unset | Encrypts API-managed credentials on disk (AES-256-GCM). |
| `AWFLEX_CREDENTIALS_DIR` | unset | Directory of `<name>.json` credentials from a mounted Secret. |
| `AWFLEX_DATA_DIR` | `/data` | Queue, last runs, credentials, git mirror. |
| `AWFLEX_TMP_DIR` | `<data>/tmp` | Per-run key files; tmpfs in the manifest. |
| `AWFLEX_LISTEN`, `AWFLEX_TLS_CERT`, `AWFLEX_TLS_KEY`, `AWFLEX_TRUST_PROXY` | `:8080` | Listener, optional TLS, trust `X-Forwarded-For`. |

Any variable also reads from `NAME_FILE`. `ANSIBLE_*` variables set on the pod are
passed to ansible. Playbooks get a minimal environment; awflex's tokens never reach
them. Host key checking is off, as in AWX, because reinstalled nodes change keys.

## Nodes: the boot hook

Install `deploy/node/awflex-callback` to `/usr/local/sbin/`, the unit to
`/etc/systemd/system/`, and write `/etc/awflex/callback.env`:

```sh
AWFLEX_URL=http://awflex.example.com
AWFLEX_BRANCH=main        # optional
AWFLEX_PLAYBOOK=main.yml  # optional
```

At boot the node sends `host`, `branch` and `playbook` and awflex runs that
playbook from that branch on that node only. A kernel parameter overrides the
file, so a PXE entry can pin a node to a branch: `awflex.branch=my-fix`.
The call retries for five minutes and is safe to repeat.

By hand: `curl -X POST "$AWFLEX_URL/api/v1/hook?host=$(hostname -f)&branch=main"`.
Without `host`, awflex uses the reverse DNS name of the caller.

## CLI and API

`awflex help` lists the commands; `GET /api/v1` (or `awflex api`) returns every
route with its auth level and parameters, for agents to read. Set `AWFLEX_URL` and
`AWFLEX_TOKEN`.

```sh
awflex cred set fleet --ssh-user root --ssh-key-file ~/.ssh/fleet --vault-password-file vault.txt
awflex run node001 node002 --branch fix-nfs --wait   # exit 3 unless all succeed
awflex runs --status failed                          # which nodes are broken
awflex get node001 --wait 600                        # block until final
awflex log node001                                   # that host's tasks only
awflex log node001 --raw                             # whole ansible output of its batch
awflex status                                        # queue, batches, fork budget
```

| Route | Auth | |
|---|---|---|
| `POST /api/v1/hook` | hook | Node callback. `host`, `branch`, `playbook`, `extra_vars` by query, form or JSON. |
| `POST /api/v1/runs` | admin | `{hosts, branch, playbook, extra_vars, tags, skip_tags, check}` |
| `GET /api/v1/runs[?status=]` | read | Last run of every host. |
| `GET /api/v1/runs/{host}[?wait=N]` | read | Last run of one host. |
| `GET /api/v1/runs/{host}/log[?raw=1]` | read | Text log. |
| `POST /api/v1/runs/{host}/cancel` | admin | Drop a queued host. |
| `DELETE /api/v1/runs/{host}` | admin | Forget a host. |
| `POST /api/v1/batches/{id}/cancel` | admin | Kill a running ansible process. |
| `GET /api/v1/status`, `/api/v1/config` | read | Queue and settings. |
| `GET/PUT/DELETE /api/v1/credentials[/{name}]` | read/admin | Secrets are write-only. |
| `GET /healthz`, `/metrics` | none | Liveness, Prometheus (`awflex_hosts{status="failed"}` is the alert). |

A call for a host that is already queued with the same request returns the same
run. A different request replaces the queued one. A call while the host is running
is parked on the record (`next`) and starts when the run ends.

### Credentials

Fields: `ssh_user`, `ssh_private_key`, `become_password`, `vault_password`,
`git_user`, `git_token`, `git_ssh_key`, `env`. Every stored credential applies to
every run; fields merge in name order. The API returns only which fields are set.
Per run they are written to tmpfs with mode 0600 and removed when ansible exits.
Authentication to nodes is by SSH key; password login is not supported.

## Measured

Kubernetes numbers are from the pod's cgroup on a local k3s cluster, with a
trivial playbook over the local connection (no SSH, no fact gathering), so they
show awflex's own overhead and a lower bound for ansible's.

| | |
|---|---|
| Idle pod memory | 4 to 14 MB |
| 301 nodes calling at once, `MAX_FORKS=50` | 21 s, 4 ansible runs, peak 1.2 GB |
| 1000 nodes calling at once (macOS host, 10 s window) | 78 s, all successful |
| Image | 181 MB |
| Pod deleted with 3 hosts mid-run | all 3 resumed and finished in the new pod |

Memory under load is ansible's: about 25 MB per fork here, more with fact
gathering and large inventories. Size the limit as `AWFLEX_MAX_FORKS` × per-fork
memory, and lower `AWFLEX_MAX_FORKS` to trade speed for memory.

## Tests

```sh
go test ./...   # engine, recovery, auth, sealing; uses a fake ansible
test/e2e.sh     # real ansible, 200 simulated nodes; NODES=1000 for more
```

Not exercised by these tests: SSH to real remote nodes, and git authentication
to a private repo (token or SSH key). Both paths are implemented and pass their
arguments to ssh and git; verify them against the real fleet and git server.

## Limits

- Hosts in one batch move through the playbook together under ansible's default
  `linear` strategy, so a slow host delays its batch. Use `strategy: free` in the
  play, or lower `AWFLEX_MAX_BATCH`.
- The hook is trusted: any caller can name any host, branch and playbook in the
  repo. Set `AWFLEX_HOOK_TOKEN` and keep the Service off untrusted networks.
- A git outage or an unknown branch marks the hosts `error`; nothing retries
  them. `awflex runs --status error` finds them.
- No schedules, RBAC, workflows, surveys or notifications.
- `ANSIBLE_CALLBACKS_ENABLED` from the pod environment is kept, but the same
  setting in the repo's `ansible.cfg` is overridden.
