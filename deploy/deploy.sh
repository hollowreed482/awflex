#!/usr/bin/env bash
# deploy/deploy.sh: build the awflex image, test it, push it and deploy it.
# Docker is not needed. On macOS it uses Apple's `container` tool; on Linux,
# podman or nerdctl.
#
#   deploy/deploy.sh --config ~/my-site/awflex-deploy.env           # every step
#   deploy/deploy.sh --config ~/my-site/awflex-deploy.env build smoke  # some steps
#   deploy/deploy.sh --help
#
# Steps, in this order: build smoke push secrets apply doctor
# With no step named, all of them run. Settings come from the config file (copy
# deploy/deploy.env.example) or the environment. Keep the config file in your
# own repository, not in this one.
#
# Every build gets a new tag (version, commit, time), so a node can never keep
# running an old image under a reused tag, and a registry never has to replace
# a tag it already holds.

set -euo pipefail

usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; }

SRC=$(cd "$(dirname "$0")/.." && pwd)
CONFIG=""
STEPS=""
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --config) CONFIG=${2:?--config needs a file}; shift ;;
    --config=*) CONFIG=${1#--config=} ;;
    build|smoke|push|secrets|apply|doctor) STEPS="$STEPS $1" ;;
    all) STEPS="" ;;
    *) echo "deploy.sh: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done
[ -n "$STEPS" ] || STEPS="build smoke push secrets apply doctor"

if [ -z "$CONFIG" ] && [ -f ./awflex-deploy.env ]; then CONFIG=./awflex-deploy.env; fi
if [ -n "$CONFIG" ]; then
  [ -f "$CONFIG" ] || { echo "deploy.sh: config file $CONFIG not found" >&2; exit 2; }
  set -a; . "$CONFIG"; set +a
fi

IMAGE=${IMAGE:-}                     # registry.example.com/team/awflex, without a tag
TAG=${TAG:-}                         # default: <version>-<commit>-<utc time>
BUILDER=${BUILDER:-auto}             # auto | container | podman | nerdctl
BASE_IMAGE=${BASE_IMAGE:-}           # the ansible image your playbooks run in
BINARY=${BINARY:-auto}               # auto | local | image: where the awflex binary is compiled
PLATFORM=${PLATFORM:-}               # default: the cluster's node architecture
GO_IMAGE=${GO_IMAGE:-}               # Go image for BINARY=image, e.g. your mirror's golang:1.26-alpine
BUILD_ARGS=${BUILD_ARGS:-}           # extra build args, KEY=VALUE separated by ;
SMOKE=${SMOKE:-1}                    # 0 skips the local test of the image
PUSH=${PUSH:-1}                      # 0 keeps the image local (a cluster that shares this runtime)
REGISTRY_INSECURE=${REGISTRY_INSECURE:-0} # 1 pushes over plain http / without TLS checks
KUBE_CONTEXT=${KUBE_CONTEXT:-}       # default: kubectl's current context
NAMESPACE=${NAMESPACE:-}             # must exist already; this script creates none
MANIFEST=${MANIFEST:-$SRC/deploy/k8s/awflex.yaml}  # point at your own copy
EXTRA_MANIFESTS=${EXTRA_MANIFESTS:-} # space separated, e.g. your ingress.yaml
AUTH_SECRET=${AUTH_SECRET:-awflex-auth}
PULL_SECRET=${PULL_SECRET:-}         # name of the image pull secret; empty for a public registry
REGISTRY_USER=${REGISTRY_USER:-}
REGISTRY_PASSWORD_FILE=${REGISTRY_PASSWORD_FILE:-}
REGISTRY_AUTH_FILE=${REGISTRY_AUTH_FILE:-} # or an existing registry auth file for the pull secret
DOCTOR_HOST=${DOCTOR_HOST:-}         # a node for doctor's real connection test
ROLLOUT_TIMEOUT=${ROLLOUT_TIMEOUT:-180s}
STATE_DIR=${STATE_DIR:-$HOME/.cache/awflex-deploy}

step() { printf '\n== %s\n' "$*"; }
say()  { printf '   %s\n' "$*"; }
die()  { # die "what went wrong" "fix" ["more fix lines"...]
  printf '\nFAILED: %s\n' "$1" >&2; shift
  for f in "$@"; do printf '   fix: %s\n' "$f" >&2; done
  exit 1
}
need() { [ -n "${!1:-}" ] || die "$1 is not set" "set $1 in the config file; see deploy/deploy.env.example"; }
have() { command -v "$1" >/dev/null 2>&1; }

VERSION=$(sed -n 's/^const Version = "\(.*\)"/\1/p' "$SRC/internal/core/types.go")
mkdir -p "$STATE_DIR"
LAST="$STATE_DIR/last-image"

k() {
  if [ -n "$KUBE_CONTEXT" ]; then kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"
  else kubectl -n "$NAMESPACE" "$@"; fi
}

builder() {
  [ -n "${B:-}" ] && return
  B=$BUILDER
  if [ "$B" = auto ]; then
    if [ "$(uname -s)" = Darwin ]; then B=container
    elif have podman; then B=podman
    elif have nerdctl; then B=nerdctl
    else die "no container tool found" "install podman (or nerdctl), or set BUILDER"; fi
  fi
  case "$B" in
    container)
      have container || die "Apple's container tool is not installed" "brew install container, then: container system start"
      container system status >/dev/null 2>&1 || {
        say "starting the container system service"
        container system start >/dev/null || die "container system start failed" "run: container system start, and follow what it prints"
      } ;;
    podman|nerdctl) have "$B" || die "$B is not installed" "install $B, or set BUILDER to a tool you have" ;;
    docker) die "BUILDER=docker is not supported" "use BUILDER=container on macOS, or podman or nerdctl" ;;
    *) die "unknown BUILDER $B" "use container, podman or nerdctl" ;;
  esac
}

platform() {
  [ -n "$PLATFORM" ] && return
  local arch=""
  if [ -n "$NAMESPACE" ] && have kubectl; then
    arch=$(kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || true)
  fi
  PLATFORM=linux/${arch:-amd64}
  if [ -n "$arch" ]; then say "platform $PLATFORM (from the cluster nodes)"
  else say "platform $PLATFORM (default; set PLATFORM to change it)"; fi
}

ref() { # the image built last, or IMAGE:TAG when TAG is given
  if [ -n "$TAG" ]; then need IMAGE; echo "$IMAGE:$TAG"; return; fi
  [ -f "$LAST" ] || die "no image has been built yet" "run the build step first, or set TAG to an image that exists"
  cat "$LAST"
}

do_build() {
  step "build"
  need IMAGE; builder; platform
  if [ -z "$TAG" ]; then
    local commit
    commit=$(git -C "$SRC" rev-parse --short=8 HEAD 2>/dev/null || echo nogit)
    git -C "$SRC" diff --quiet HEAD 2>/dev/null || commit="$commit-dirty"
    TAG="$VERSION-$commit-$(date -u +%Y%m%d%H%M%S)"
  fi
  local r="$IMAGE:$TAG" ctx file a
  local args=()
  local goarch=${PLATFORM#linux/}; goarch=${goarch%%/*}
  if [ -n "$BASE_IMAGE" ]; then
    local mode=$BINARY
    if [ "$mode" = auto ]; then if have go; then mode=local; else mode=image; fi; fi
    if [ "$mode" = local ]; then
      have go || die "BINARY=local needs Go on this machine" "install Go (brew install go), or set BINARY=image"
      ctx="$STATE_DIR/build"; rm -rf "$ctx"; mkdir -p "$ctx"
      say "cross-compiling awflex for linux/$goarch on this machine (make dist/awflex-$VERSION-linux-$goarch)"
      if have make; then
        make -s -C "$SRC" "dist/awflex-$VERSION-linux-$goarch" && cp "$SRC/dist/awflex-$VERSION-linux-$goarch" "$ctx/awflex"
      else
        (cd "$SRC" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$ctx/awflex" ./cmd/awflex)
      fi || die "the Go build failed" "install the Go version named in go.mod, or set BINARY=image"
      file="$SRC/deploy/Containerfile.prebuilt"
    else
      ctx=$SRC; file="$SRC/deploy/Containerfile.base"
    fi
    args=(--build-arg "BASE_IMAGE=$BASE_IMAGE")
  else
    say "BASE_IMAGE is not set: building the small default image (ansible-core only)"
    ctx=$SRC; file="$SRC/Containerfile"
  fi
  [ -z "$GO_IMAGE" ] || args+=(--build-arg "GO_IMAGE=$GO_IMAGE")
  if [ -n "$BUILD_ARGS" ]; then
    local oldifs=$IFS; IFS=';'
    for a in $BUILD_ARGS; do args+=(--build-arg "$a"); done
    IFS=$oldifs
  fi
  say "building $r with $B from $(basename "$file")"
  # ${args[@]+...} keeps bash 3.2 from failing on an empty array under set -u.
  if ! $B build --platform "$PLATFORM" -f "$file" -t "$r" ${args[@]+"${args[@]}"} "$ctx"; then
    die "the image build failed (see the output above)" \
      "\"exec format error\": this machine cannot run $PLATFORM steps; on Apple silicon install Rosetta (softwareupdate --install-rosetta)" \
      "a base image pull failed: log in to its registry first ($(login_cmd "${BASE_IMAGE%%/*}"))" \
      "a Go image pull failed: set GO_IMAGE to your mirror, or install Go so the binary is built here (BINARY=local)"
  fi
  echo "$r" > "$LAST"
  say "built $r"
}

login_cmd() {
  case "${B:-container}" in
    container) echo "container registry login $1" ;;
    *) echo "$B login $1" ;;
  esac
}

do_smoke() {
  step "smoke: run the image here under the pod's restrictions"
  if [ "$SMOKE" = 0 ]; then say "skipped (SMOKE=0)"; return; fi
  builder; platform
  local r; r=$(ref)
  local d="$STATE_DIR/smoke" name="awflex-smoke-$$"
  rm -rf "$d"; mkdir -p "$d/repo" "$d/run" "$d/data"
  chmod 0777 "$d/run" "$d/data"
  printf -- '- hosts: all\n  gather_facts: false\n  tasks:\n    - ansible.builtin.ping:\n' > "$d/repo/site.yml"
  (cd "$d/repo" && git init -q -b main && git add -A && git -c user.name=smoke -c user.email=smoke@example.com commit -qm smoke)
  chmod -R a+rX "$d/repo"
  trap '$B rm -f "$name" >/dev/null 2>&1 || true' EXIT
  if ! $B run -d --name "$name" --platform "$PLATFORM" --user 1000 --read-only \
    -v "$d/run:/run/awflex" -v "$d/data:/data" -v "$d/repo:/repo:ro" \
    -e AWFLEX_REPO=/repo -e AWFLEX_PLAYBOOK=site.yml -e AWFLEX_ADMIN_TOKEN=smoke \
    -e 'AWFLEX_EXTRA_VARS={"ansible_connection":"local"}' "$r" > "$d/run.log" 2>&1; then
    if grep -q -i -e 'rosetta is not installed' -e 'exec format error' "$d/run.log"; then
      say "WARNING: smoke SKIPPED. This machine cannot run $PLATFORM images: it has no emulation for that architecture."
      say "to test $PLATFORM images here, install Rosetta once: softwareupdate --install-rosetta --agree-to-license"
      say "or set PLATFORM to this machine's architecture for a local test build. The image itself is fine to push."
      trap - EXIT
      return 0
    fi
    tail -5 "$d/run.log" | sed 's/^/   /' >&2
    die "the image would not start here (the error is above)" "read the error, fix it, and run the smoke step again; SMOKE=0 skips this test"
  fi
  local up=""
  for _ in $(seq 40); do
    if $B exec "$name" awflex status >/dev/null 2>&1; then up=1; break; fi
    sleep 0.5
  done
  if [ -z "$up" ]; then
    $B logs "$name" 2>&1 | tail -5 >&2
    die "awflex did not start inside the image (its log is above)" "the last log line names the problem"
  fi
  # The test repo belongs to this machine's user, not uid 1000.
  $B exec "$name" git config --global --add safe.directory '*' >/dev/null 2>&1 || true
  local rc=0
  $B exec "$name" awflex doctor --host localhost --text | sed 's/^/   /' || rc=$?
  $B rm -f "$name" >/dev/null 2>&1 || true
  trap - EXIT
  [ "$rc" = 0 ] || die "the image failed its local test (see the checks above)" "apply the fix printed under each FAIL, rebuild, and run the smoke step again"
  say "the image runs as uid 1000 with a read-only root, and ansible works in it"
  say "not tested here: your repo, inventory, credentials and network; the doctor step checks those in the cluster"
}

do_push() {
  step "push"
  local r; r=$(ref)
  if [ "$PUSH" = 0 ]; then say "skipped (PUSH=0): $r stays in this machine's image store"; return; fi
  builder; platform
  local ok=0 insecure=()
  if [ "$REGISTRY_INSECURE" = 1 ]; then
    case "$B" in
      container) insecure=(--scheme http) ;;
      nerdctl)   insecure=(--insecure-registry) ;;
      podman)    insecure=(--tls-verify=false) ;;
    esac
  fi
  case "$B" in
    container) container image push ${insecure[@]+"${insecure[@]}"} --platform "$PLATFORM" "$r" && ok=1 ;;
    nerdctl)   nerdctl push ${insecure[@]+"${insecure[@]}"} --platform "$PLATFORM" "$r" && ok=1 ;;
    podman)    podman push ${insecure[@]+"${insecure[@]}"} "$r" && ok=1 ;;
  esac
  [ "$ok" = 1 ] || die "push of $r failed (see the output above)" \
    "unauthorized or denied: log in first: $(login_cmd "${IMAGE%%/*}")" \
    "TLS or \"server gave HTTP response\" errors on a registry without TLS: set REGISTRY_INSECURE=1" \
    "manifest or media type errors: push to a new tag (the default; do not set TAG to one that exists)"
  say "pushed $r"
}

do_secrets() {
  step "secrets in namespace $NAMESPACE"
  need NAMESPACE
  have kubectl || die "kubectl is not installed" "install kubectl"
  k get serviceaccount default >/dev/null 2>&1 || die "cannot use namespace $NAMESPACE" \
    "check the context (KUBE_CONTEXT=${KUBE_CONTEXT:-current}) and your access to it" \
    "this script creates no namespace: create it, or ask whoever manages the cluster"
  local tmp; tmp=$(mktemp -d); chmod 0700 "$tmp"
  if k get secret "$AUTH_SECRET" >/dev/null 2>&1; then
    say "$AUTH_SECRET exists, kept as is"
  else
    { echo "AWFLEX_ADMIN_TOKEN=$(openssl rand -hex 32)"
      echo "AWFLEX_HOOK_TOKEN=$(openssl rand -hex 32)"
      echo "AWFLEX_SECRET_KEY=$(openssl rand -hex 32)"; } > "$tmp/auth.env"
    k create secret generic "$AUTH_SECRET" --from-env-file="$tmp/auth.env" >/dev/null
    say "$AUTH_SECRET created with new random tokens"
  fi
  if [ -z "$PULL_SECRET" ]; then
    say "PULL_SECRET not set: the image must be pullable without credentials"
  elif k get secret "$PULL_SECRET" >/dev/null 2>&1; then
    say "$PULL_SECRET exists, kept as is"
  else
    local auth="$tmp/auth.json" host=${IMAGE%%/*}
    if [ -n "$REGISTRY_AUTH_FILE" ]; then
      cp "$REGISTRY_AUTH_FILE" "$auth"
    elif [ -n "$REGISTRY_USER" ] && [ -n "$REGISTRY_PASSWORD_FILE" ]; then
      local b64
      b64=$(printf '%s:%s' "$REGISTRY_USER" "$(cat "$REGISTRY_PASSWORD_FILE")" | base64 | tr -d '\n')
      printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$host" "$b64" > "$auth"
    else
      rm -rf "$tmp"
      die "pull secret $PULL_SECRET does not exist and nothing to make it from" \
        "set REGISTRY_USER and REGISTRY_PASSWORD_FILE (a file holding the password or token)" \
        "or set REGISTRY_AUTH_FILE to a registry auth file, or create the secret yourself and run again"
    fi
    k create secret generic "$PULL_SECRET" --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson="$auth" >/dev/null
    say "$PULL_SECRET created for $host"
  fi
  rm -rf "$tmp"
}

diagnose() {
  echo >&2
  echo "The rollout did not finish. What the cluster says:" >&2
  k get pods -l app=awflex -o wide >&2 || true
  local pod reason
  pod=$(k get pods -l app=awflex --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)
  [ -n "$pod" ] || die "no awflex pod exists" "k describe deployment awflex: look for quota or admission errors in its events"
  reason=$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  [ -n "$reason" ] || reason=$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  k get events --field-selector involvedObject.name="$pod" --sort-by=.lastTimestamp 2>/dev/null | tail -8 >&2 || true
  k logs "$pod" --tail=10 >&2 2>/dev/null || true
  case "$reason" in
    *ImagePull*|*ErrImage*) die "the node cannot pull the image ($reason); the event above says why" \
      "unauthorized or denied: a private registry needs PULL_SECRET (see the secrets step)" \
      "connection refused, timeout or no such host: the nodes cannot reach the registry under that name; use a registry name the nodes can reach" \
      "not found: the push did not finish; run the push step again" \
      "\"no match for platform\": set PLATFORM=linux/<the node architecture> and build again" ;;
    *CrashLoop*|*Error*) die "awflex exits at start ($reason)" "the last log line above names the setting to fix" ;;
    *CreateContainerConfigError*) die "a Secret or ConfigMap the pod needs is missing" "run the secrets step, and check the names in your manifest" ;;
    *ContainerCreating*|*Pending*) die "the pod cannot start ($reason)" \
      "a mount error in the events: fix the data volume (nfs server and path, or a volume claim)" \
      "a quota or scheduling error: lower the resources in your manifest or ask for quota" ;;
    *) die "the pod is not ready ($reason)" "read the events above, then run: deploy/deploy.sh doctor" ;;
  esac
}

do_apply() {
  step "apply to namespace $NAMESPACE"
  need NAMESPACE
  [ -f "$MANIFEST" ] || die "manifest $MANIFEST not found" "set MANIFEST to your copy of deploy/k8s/awflex.yaml"
  local r rendered before
  r=$(ref)
  rendered=$(mktemp)
  # Put the image just built into the manifest's one image: line.
  awk -v img="$r" '!done && /^[[:space:]]*image:[[:space:]]/ { sub(/image:.*/, "image: " img); done=1 } { print }' "$MANIFEST" > "$rendered"
  grep -q "image: $r" "$rendered" || die "no image: line found in $MANIFEST" "the manifest must have the awflex container's image: line"
  before=$(k get deployment awflex -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
  k apply -f "$rendered" | sed 's/^/   /'
  rm -f "$rendered"
  for m in $EXTRA_MANIFESTS; do k apply -f "$m" | sed 's/^/   /'; done
  if [ -n "$PULL_SECRET" ]; then
    k patch deployment awflex --type merge \
      -p "{\"spec\":{\"template\":{\"spec\":{\"imagePullSecrets\":[{\"name\":\"$PULL_SECRET\"}]}}}}" >/dev/null
  fi
  if [ "$before" = "$r" ]; then
    say "the image did not change; restarting so the pod starts fresh"
    k rollout restart deployment/awflex >/dev/null
  fi
  k rollout status deployment/awflex --timeout="$ROLLOUT_TIMEOUT" | sed 's/^/   /' || diagnose
  local running
  running=$(k exec deploy/awflex -- awflex version 2>/dev/null || true)
  say "running awflex $running from $r"
}

do_doctor() {
  step "doctor in the cluster${DOCTOR_HOST:+ (connecting to $DOCTOR_HOST)}"
  need NAMESPACE
  local rc=0
  k exec deploy/awflex -- awflex doctor ${DOCTOR_HOST:+--host "$DOCTOR_HOST"} --text | sed 's/^/   /' || rc=$?
  [ -n "$DOCTOR_HOST" ] || say "set DOCTOR_HOST to one node to also test a real connection"
  [ "$rc" = 0 ] || die "doctor found problems (see the checks above)" "apply the fix printed under each FAIL, then run: deploy/deploy.sh --config ${CONFIG:-FILE} doctor"
}

for s in $STEPS; do
  case "$s" in
    build) do_build ;; smoke) do_smoke ;; push) do_push ;;
    secrets) do_secrets ;; apply) do_apply ;; doctor) do_doctor ;;
  esac
done
printf '\ndone:%s\n' "$STEPS"
