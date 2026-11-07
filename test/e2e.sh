#!/usr/bin/env bash
# End-to-end test against real ansible: a local git repo, a real awflex server
# and hundreds of simulated nodes (local connection) calling the hook at once.
#
#   test/e2e.sh            needs: go, git, curl, jq, ssh-agent and ansible (playbook, inventory, vault) on PATH
#   NODES=1000 test/e2e.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d "${TMPDIR:-/tmp}/awflex-e2e.XXXXXX")
NODES=${NODES:-200}
PORT=${PORT:-18080}
export AWFLEX_URL=http://127.0.0.1:$PORT AWFLEX_TOKEN=admin-secret
BIN=$W/awflex
PASS=0 FAIL=0
SRV=

ok()   { PASS=$((PASS+1)); echo "  ok   $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL $*"; }
check(){ local name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }
eq()   { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }

cleanup() {
  [ -n "$SRV" ] && kill "$SRV" 2>/dev/null || true
  pkill -f "$W/data" 2>/dev/null || true
  if [ "$FAIL" = 0 ] && [ -z "${KEEP:-}" ]; then rm -rf "$W"; else echo "kept $W"; fi
}
trap cleanup EXIT

start() { # extra arguments are more NAME=value settings
  env AWFLEX_LISTEN=127.0.0.1:$PORT AWFLEX_DATA_DIR=$W/data AWFLEX_REPO=$W/repo \
  AWFLEX_ADMIN_TOKEN=admin-secret AWFLEX_READ_TOKEN=read-secret AWFLEX_SECRET_KEY=seal-key \
  AWFLEX_BATCH_WINDOW=${WINDOW:-2} AWFLEX_MAX_FORKS=50 AWFLEX_FORKS=25 \
  AWFLEX_EXTRA_VARS="{\"marker_dir\":\"$W/markers\"}" "$@" \
    "$BIN" serve >>"$W/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 50); do curl -fsS "$AWFLEX_URL/healthz" >/dev/null 2>&1 && return; sleep 0.1; done
  echo "server did not start"; cat "$W/server.log"; exit 1
}

wait_idle() { # wait until nothing is queued or running
  for _ in $(seq "${1:-600}"); do
    s=$("$BIN" status)
    [ "$(jq '.queued + .running' <<<"$s")" = 0 ] && return 0
    sleep 0.5
  done
  echo "timed out waiting for idle"; "$BIN" status; return 1
}

rss_mb() { ps -o rss= -p "$1" 2>/dev/null | awk '{printf "%.0f", $1/1024}'; }

echo "== build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/awflex)

echo "== playbook repo with two branches"
mkdir -p "$W/repo" "$W/markers" && cd "$W/repo" && git init -q -b main
mkdir group_vars
cat > group_vars/all.yml <<YML
ansible_connection: local
ansible_python_interpreter: $(command -v python3)
YML
cat > main.yml <<'YML'
- hosts: all
  gather_facts: false
  vars_files: [secrets.yml]
  tasks:
    - name: show branch
      ansible.builtin.debug:
        msg: "branch=main env={{ lookup('env', 'CRED_ENV') | default('unset', true) }}"
    - name: vault password opened the secrets file
      ansible.builtin.assert:
        that: vaulted == 'opened'
    - name: write marker
      ansible.builtin.copy:
        content: "{{ inventory_hostname }} {{ awflex_commit }}\n"
        dest: "{{ marker_dir }}/{{ inventory_hostname }}"
    - name: the run's ssh-agent holds the fleet key
      ansible.builtin.command: ssh-add -l
      changed_when: false
    - name: fail on bad hosts
      ansible.builtin.command: sh -c 'echo broken disk >&2; exit 1'
      when: inventory_hostname.startswith('bad')
YML
printf '[compute]\ninv01\ninv02\n' > hosts.ini
cat > slow.yml <<'YML'
- hosts: all
  gather_facts: false
  tasks:
    - name: slow step
      ansible.builtin.command: sleep 6
YML
echo 'vault-pw' > "$W/vault-pw"
printf 'vaulted: opened\n' > secrets.yml
ansible-vault encrypt --vault-password-file "$W/vault-pw" secrets.yml >/dev/null
git add -A && git -c user.name=t -c user.email=t@t commit -qm main
git checkout -q -b feature
sed -i.bak 's/branch=main/branch=feature/' main.yml && rm main.yml.bak
git -c user.name=t -c user.email=t@t commit -qam feature
git checkout -q main
MAIN=$(git rev-parse main) FEATURE=$(git rev-parse feature)
cd "$ROOT"

echo "== start"
start
IDLE=$(rss_mb "$SRV")
echo "  idle server RSS: ${IDLE} MB"

echo "== auth"
eq "no token is rejected" "$(curl -s -o /dev/null -w '%{http_code}' "$AWFLEX_URL/api/v1/runs")" 401
eq "read token can read" "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer read-secret' "$AWFLEX_URL/api/v1/runs")" 200
eq "read token cannot launch" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer read-secret' -d '{"hosts":["x"]}' "$AWFLEX_URL/api/v1/runs")" 401
eq "a node cannot pass variables" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$AWFLEX_URL/api/v1/hook?host=n1" --data-urlencode 'extra_vars={"a":1}')" 400
eq "bad host name is rejected" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$AWFLEX_URL/api/v1/hook?host=a%3Brm")" 400
eq "unparseable query is rejected" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$AWFLEX_URL/api/v1/hook?host=a;rm")" 400

echo "== credentials"
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
"$BIN" cred set fleet --ssh-user root --ssh-key-file "$W/key" --vault-password-file "$W/vault-pw" --env CRED_ENV=from-cred >/dev/null
eq "credential lists its fields" "$("$BIN" cred ls | jq -c '.credentials[0].fields')" '["ssh_private_key","ssh_user","vault_password"]'
check "API never returns the secret" bash -c "! '$BIN' cred ls | grep -q 'vault-pw\|OPENSSH'"
check "credential is sealed on disk" bash -c "! grep -rq 'vault-pw\|OPENSSH' '$W/data/credentials'"

echo "== $NODES nodes call the hook at once"
T0=$(date +%s)
seq -f 'node%04g' "$NODES" | xargs -P 64 -I{} curl -fsS -o /dev/null -X POST "$AWFLEX_URL/api/v1/hook?host={}&branch=main"
curl -fsS -o /dev/null -X POST "$AWFLEX_URL/api/v1/hook?host=bad0001"
FIRST=$(curl -fsS -X POST "$AWFLEX_URL/api/v1/hook?host=node0001" | jq -r .id)
AGAIN=$(curl -fsS -X POST "$AWFLEX_URL/api/v1/hook?host=node0001" | jq -r .id)
eq "a repeat call while queued returns the same run" "$AGAIN" "$FIRST"
PEAK=0
while [ "$("$BIN" status | jq '.queued + .running')" != 0 ]; do
  cur=$(ps -axo rss=,command= | awk -v w="$W" 'index($0, w) {s+=$1} END {printf "%.0f", s/1024}')
  [ "$cur" -gt "$PEAK" ] && PEAK=$cur
  sleep 0.5
done
T1=$(date +%s)
echo "  $NODES nodes converged in $((T1-T0))s; peak RSS of awflex + ansible: ${PEAK} MB"
eq "every node succeeded" "$("$BIN" runs --status successful | jq .count)" "$NODES"
eq "every node wrote its marker" "$(ls "$W/markers" | grep -c '^node')" "$NODES"
BATCHES=$(grep -c 'batch .* start' "$W/server.log")
WANT=$(( (NODES + 1 + 99) / 100 ))
if [ "$BATCHES" -le $((WANT + 2)) ]; then ok "$((NODES+1)) hosts shared $BATCHES ansible runs"; else bad "too many ansible runs: $BATCHES"; fi
eq "run used the main commit" "$("$BIN" get node0001 | jq -r .commit)" "$MAIN"
check "the key reached ansible through an agent" bash -c "! '$BIN' log node0001 --raw | grep -q 'no ssh-agent'"
check "credential reached ansible (vault + env)" bash -c "'$BIN' log node0001 --raw | grep -q 'env=from-cred'"

echo "== a failing host"
R=$("$BIN" get bad0001)
eq "status is failed" "$(jq -r .status <<<"$R")" failed
eq "failed task is named" "$(jq -r '.failed_tasks[0].task' <<<"$R")" "fail on bad hosts"
check "host log carries stderr" bash -c "'$BIN' log bad0001 | grep -q 'broken disk'"
check "host log holds only that host" bash -c "! '$BIN' log bad0001 | grep -q node0"

echo "== the node picks the branch; only the last run is kept"
"$BIN" hook --host node0001 --branch feature >/dev/null
R=$("$BIN" get node0001 --wait 60)
eq "feature branch commit ran" "$(jq -r .commit <<<"$R")" "$FEATURE"
check "feature playbook ran" bash -c "'$BIN' log node0001 --raw | grep -q 'branch=feature'"
if [ "$(jq -r .id <<<"$R")" != "$FIRST" ]; then ok "record was replaced by the new run"; else bad "record not replaced"; fi
eq "one record per host on disk" "$(ls "$W/data/runs" | grep -c '^node0001\.')" 2
eq "unknown branch is an error" "$("$BIN" run node0002 --branch nope --wait | jq -r '.runs[0].status')" error
wait_idle
STALE=$(comm -23 <(ls "$W/data/batches" | sed 's/\.log$//' | sort) <("$BIN" runs | jq -r '.runs[].batch_id' | sort -u) | wc -l | tr -d ' ')
eq "raw logs no host points at are deleted" "$STALE" 0

echo "== ansible is killed mid-run (out of memory): hosts re-queue themselves"
"$BIN" run crash01 crash02 --playbook slow.yml >/dev/null
for _ in $(seq 100); do [ "$("$BIN" get crash01 | jq -r .status)" = running ] && break; sleep 0.2; done
sleep 1
pkill -9 -f "ansible-playbook slow.yml" || true
R=$("$BIN" get crash01 --wait 60); R=$("$BIN" get crash01 --wait 60)
eq "host finished on the second attempt" "$(jq -c '[.status, .attempts]' <<<"$R")" '["successful",2]'

echo "== awflex itself is killed mid-run: the queue comes back from disk"
"$BIN" run dead01 dead02 --playbook slow.yml >/dev/null
for _ in $(seq 100); do [ "$("$BIN" get dead01 | jq -r .status)" = running ] && break; sleep 0.2; done
"$BIN" run late01 --playbook slow.yml --branch feature >/dev/null
AGENT_PID=$(pgrep -n -f 'ssh-agent -D -a' || true) # a dying pod takes the run's agent with it
{ kill -9 "$SRV"; wait "$SRV"; } 2>/dev/null || true; [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null; pkill -9 -f "ansible-playbook slow.yml" || true; wait "$SRV" 2>/dev/null || true
eq "disk shows the run never finished" "$(jq -r .status "$W/data/runs/dead01.json")" running
eq "disk holds the queued host" "$(jq -r .status "$W/data/runs/late01.json")" queued
start
check "restart reports the recovery" grep -q 'unfinished runs re-queued' "$W/server.log"
wait_idle
eq "interrupted host finished after restart" "$("$BIN" get dead01 | jq -c '[.status, .attempts]')" '["successful",2]'
eq "queued host finished after restart" "$("$BIN" get late01 | jq -c '[.status, .attempts]')" '["successful",1]'

echo "== graceful shutdown keeps unfinished hosts queued"
"$BIN" run term01 --playbook slow.yml >/dev/null
for _ in $(seq 100); do [ "$("$BIN" get term01 | jq -r .status)" = running ] && break; sleep 0.2; done
kill -TERM "$SRV"; wait "$SRV" 2>/dev/null || true
eq "host is back to queued on disk" "$(jq -c '[.status, .attempts]' "$W/data/runs/term01.json")" '["queued",0]'
start; wait_idle
eq "host finished after restart" "$("$BIN" get term01 | jq -r .status)" successful

echo "== with an inventory, ansible itself leaves unknown hosts alone"
kill -TERM "$SRV"; wait "$SRV" 2>/dev/null || true
start AWFLEX_INVENTORY=hosts.ini
curl -fsS -o /dev/null -X POST "$AWFLEX_URL/api/v1/hook?host=inv01"
curl -fsS -o /dev/null -X POST "$AWFLEX_URL/api/v1/hook?host=stranger01"
eq "inventory host ran" "$("$BIN" get inv01 --wait 60 | jq -r .status)" successful
R=$("$BIN" get stranger01 --wait 60)
eq "unknown host ends as an error" "$(jq -r .status <<<"$R")" error
case "$(jq -r .error <<<"$R")" in *"not in the inventory"*) ok "the error says why";; *) bad "the error does not say why";; esac
check "nothing ran on the unknown host" bash -c "! ls '$W/markers' | grep -q stranger01"
case "$(jq -r .hint <<<"$R")" in *"doctor --host"*) ok "the record carries a hint with the next command";; *) bad "no hint on the unknown host";; esac

echo "== doctor and the stdout callback"
rc=0; D=$("$BIN" doctor --host inv01) || rc=$?
eq "doctor passes for an inventory host" "$rc/$(jq -r .ok <<<"$D")" "0/true"
eq "doctor connected to it" "$(jq -r '.checks[] | select(.name=="connect") | .status' <<<"$D")" ok
rc=0; D=$("$BIN" doctor --host stranger01) || rc=$?
eq "doctor fails for a host outside the inventory" "$rc/$(jq -r '.checks[] | select(.name=="targets") | .status' <<<"$D")" "3/fail"
(cd "$W/repo" && git checkout -q main && printf '[defaults]\nstdout_callback = no.such.callback\n' > ansible.cfg && git add ansible.cfg && git -c user.name=t -c user.email=t@t commit -qm cfg)
"$BIN" run inv01 >/dev/null
eq "a repo naming a missing stdout callback still runs" "$("$BIN" get inv01 --wait 60 | jq -r .status)" successful

echo
echo "passed $PASS, failed $FAIL   (idle ${IDLE} MB, peak ${PEAK} MB for $NODES nodes)"
[ "$FAIL" = 0 ]
