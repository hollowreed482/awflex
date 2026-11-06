package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAnsible stands in for ansible-playbook: it reports every inventory host
// as successful, or misbehaves as told by the "mode" file beside it.
const fakeAnsible = `#!/bin/sh
here=$(dirname "$0")
inv=
while [ $# -gt 0 ]; do case "$1" in -i) inv=$2; shift;; esac; shift; done
echo run >> "$here/calls"
mode=$(cat "$here/mode" 2>/dev/null)
case "$mode" in
  crash) kill -9 $$ ;;
  sleep*) sleep "${mode#sleep }" ;;
esac
grep -v '^\[' "$inv" | sort -u | while read -r h; do
  printf '{"e":"task","host":"%s","task":"step","status":"ok"}\n' "$h" >> "$AWFLEX_EVENTS"
  printf '{"e":"stats","host":"%s","ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":0,"rescued":0,"ignored":0}\n' "$h" >> "$AWFLEX_EVENTS"
done
`

type rig struct {
	t   *testing.T
	dir string
	cfg Config
	st  *Store
	eng *Engine
}

func newRig(t *testing.T, tune func(*Config)) *rig {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "main.yml"), []byte("- hosts: all\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "other.yml"), []byte("- hosts: all\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	bin := filepath.Join(dir, "fake", "ansible-playbook")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte(fakeAnsible), 0o755)

	cfg := Config{DataDir: filepath.Join(dir, "data"), Repo: repo, Ref: "main", Playbook: "main.yml",
		AdminToken: "admin", ReadToken: "read", SecretKey: "k", MaxForks: 50, Forks: 25, MaxBatch: 100,
		MaxAttempts: 3, BatchWindow: 150 * time.Millisecond, Timeout: time.Minute, AnsibleBin: bin, GitBin: "git"}
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, dir: dir, cfg: cfg}
	r.open()
	t.Cleanup(func() { r.eng.Stop() })
	return r
}

func (r *rig) open() {
	var err error
	if r.st, err = OpenStore(r.cfg.DataDir, r.cfg.SecretKey); err != nil {
		r.t.Fatal(err)
	}
	git, err := NewGit(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	if r.eng, err = NewEngine(r.cfg, r.st, git); err != nil {
		r.t.Fatal(err)
	}
	r.eng.Start()
}

func (r *rig) mode(m string) {
	os.WriteFile(filepath.Join(filepath.Dir(r.cfg.AnsibleBin), "mode"), []byte(m), 0o644)
}

func (r *rig) calls() int {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(r.cfg.AnsibleBin), "calls"))
	return strings.Count(string(b), "run")
}

func (r *rig) waitFinal(host string) *Run {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		if run := r.st.GetRun(host); run != nil && isFinal(run.Status) {
			return run
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.t.Fatalf("%s never finished: %+v", host, r.st.GetRun(host))
	return nil
}

func hosts(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + string(rune('a'+i/26/26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
	}
	return out
}

func TestCallersShareAnsibleRuns(t *testing.T) {
	// Each accepted call is fsynced, so the window must outlast 50 of them.
	r := newRig(t, func(c *Config) { c.MaxBatch, c.BatchWindow = 20, 3*time.Second })
	hs := hosts("n", 50)
	for _, h := range hs { // one hook call per node
		r.eng.Submit([]string{h}, Request{Ref: "main", Playbook: "main.yml", Source: "hook"})
	}
	for _, h := range hs {
		if run := r.waitFinal(h); run.Status != StSuccessful || run.Commit == "" || run.Attempts != 1 {
			t.Fatalf("%s: %+v", h, run)
		}
	}
	if c := r.calls(); c != 3 {
		t.Fatalf("50 hosts with max_batch 20 should take 3 ansible runs, took %d", c)
	}
	log, _ := r.st.ReadLog(hs[0])
	if !strings.Contains(string(log), "step") || strings.Contains(string(log), hs[1]) {
		t.Fatalf("host log should hold only its own tasks:\n%s", log)
	}
	if s := r.eng.Status(); s.ForksInUse != 0 || s.Queued != 0 || s.HookCalls != 50 {
		t.Fatalf("status after drain: %+v", s)
	}
}

func TestRepeatCallsAndNewestWins(t *testing.T) {
	r := newRig(t, func(c *Config) { c.BatchWindow = 300 * time.Millisecond })
	a := r.eng.Submit([]string{"h1"}, Request{Ref: "main", Playbook: "main.yml"})[0]
	b := r.eng.Submit([]string{"h1"}, Request{Ref: "main", Playbook: "main.yml"})[0]
	if a.ID != b.ID {
		t.Fatal("identical request while queued should not create a second run")
	}
	c := r.eng.Submit([]string{"h1"}, Request{Ref: "main", Playbook: "other.yml"})[0]
	if c.ID == a.ID {
		t.Fatal("a different request while queued should replace the waiting one")
	}
	if run := r.waitFinal("h1"); run.Playbook != "other.yml" || r.calls() != 1 {
		t.Fatalf("newest request should be the only one run: %+v calls=%d", run, r.calls())
	}

	// A request arriving while the host is running waits, then runs.
	r.mode("sleep 0.6")
	r.eng.Submit([]string{"h2"}, Request{Ref: "main", Playbook: "main.yml"})
	for r.st.GetRun("h2").Status != StRunning {
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.eng.Submit([]string{"h2"}, Request{Ref: "main", Playbook: "other.yml"})[0]; got.Status != StRunning || got.Next == nil {
		t.Fatalf("request during a run should be parked on the record: %+v", got)
	}
	r.mode("")
	for i := 0; i < 400; i++ {
		if run := r.st.GetRun("h2"); run.Status == StSuccessful && run.Playbook == "other.yml" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("parked request never ran: %+v", r.st.GetRun("h2"))
}

func TestForkBudgetHoldsBatchesBack(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxForks, c.Forks = 4, 4 })
	r.mode("sleep 0.3")
	r.eng.Submit(hosts("a", 4), Request{Ref: "main", Playbook: "main.yml"})
	r.eng.Submit(hosts("b", 4), Request{Ref: "main", Playbook: "other.yml"})
	peak := 0
	for i := 0; i < 400; i++ {
		s := r.eng.Status()
		peak = max(peak, len(s.Batches))
		if s.ForksInUse > 4 {
			t.Fatalf("fork budget exceeded: %d", s.ForksInUse)
		}
		if s.Queued+s.Running == 0 && r.calls() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peak != 1 || r.calls() != 2 {
		t.Fatalf("batches should run one at a time: peak=%d calls=%d", peak, r.calls())
	}
}

func TestCrashedAnsibleIsRetriedThenGivenUp(t *testing.T) {
	r := newRig(t, nil)
	r.mode("crash")
	r.eng.Submit([]string{"h1"}, Request{Ref: "main", Playbook: "main.yml"})
	run := r.waitFinal("h1")
	if run.Status != StError || run.Attempts != 3 || !strings.Contains(run.Error, "killed") || r.calls() != 3 {
		t.Fatalf("want error after 3 attempts, got %+v calls=%d", run, r.calls())
	}
}

func TestUnfinishedRunsResumeAfterRestart(t *testing.T) {
	r := newRig(t, nil)
	r.eng.Stop()
	now := time.Now()
	req := Request{Ref: "main", Playbook: "main.yml"}
	req.Group = req.groupKey()
	// What the disk looks like after awflex died: one host mid-run, one still
	// queued, one that has already taken down ansible three times.
	r.st.SaveRun(&Run{ID: "a", Host: "midrun", Status: StRunning, Attempts: 1, Request: req, QueuedAt: now, StartedAt: &now, BatchID: "gone"})
	r.st.SaveRun(&Run{ID: "b", Host: "waiting", Status: StQueued, Request: req, QueuedAt: now})
	r.st.SaveRun(&Run{ID: "c", Host: "poison", Status: StRunning, Attempts: 3, Request: req, QueuedAt: now, StartedAt: &now, BatchID: "gone"})
	r.open()

	if run := r.waitFinal("midrun"); run.Status != StSuccessful || run.Attempts != 2 || run.ID != "a" {
		t.Fatalf("midrun: %+v", run)
	}
	if run := r.waitFinal("waiting"); run.Status != StSuccessful || run.Attempts != 1 {
		t.Fatalf("waiting: %+v", run)
	}
	if run := r.waitFinal("poison"); run.Status != StError || !strings.Contains(run.Error, "gave up") {
		t.Fatalf("poison: %+v", run)
	}
	if r.calls() != 1 {
		t.Fatalf("recovered hosts should share one ansible run, took %d", r.calls())
	}
}

func TestCancelAndForget(t *testing.T) {
	r := newRig(t, func(c *Config) { c.BatchWindow = time.Hour })
	r.eng.Submit([]string{"h1", "h2"}, Request{Ref: "main", Playbook: "main.yml"})
	if run, err := r.eng.Cancel("h1"); err != nil || run.Status != StCanceled {
		t.Fatalf("cancel: %v %+v", err, run)
	}
	if err := r.eng.Forget("h2"); err != nil || r.st.GetRun("h2") != nil {
		t.Fatalf("forget: %v", err)
	}
	if s := r.eng.Status(); s.Queued != 0 {
		t.Fatalf("queue should be empty: %+v", s)
	}
}

func TestCredentialsAreSealedAndNeverReturned(t *testing.T) {
	r := newRig(t, nil)
	srv := NewServer(r.cfg, r.st, r.eng)
	do := func(method, path, token, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	secret := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"
	body, _ := json.Marshal(map[string]any{"ssh_user": "root", "ssh_private_key": secret, "env": map[string]string{"TOKEN": "hunter2"}})

	if code, _ := do("PUT", "/api/v1/credentials/fleet", "read", string(body)); code != http.StatusUnauthorized {
		t.Fatalf("read token must not write credentials: %d", code)
	}
	if code, out := do("PUT", "/api/v1/credentials/fleet", "admin", string(body)); code != 200 || strings.Contains(out, "abc") {
		t.Fatalf("put: %d %s", code, out)
	}
	code, out := do("GET", "/api/v1/credentials", "read", "")
	if code != 200 || strings.Contains(out, "abc") || strings.Contains(out, "hunter2") || !strings.Contains(out, "ssh_private_key") {
		t.Fatalf("list must name fields without values: %d %s", code, out)
	}
	disk, _ := os.ReadFile(filepath.Join(r.cfg.DataDir, "credentials", "fleet.json"))
	if bytes.Contains(disk, []byte("OPENSSH")) || bytes.Contains(disk, []byte("hunter2")) {
		t.Fatalf("credential is not sealed on disk: %s", disk)
	}
	if st, err := OpenStore(r.cfg.DataDir, "k"); err != nil || mergeCreds(st.ListCredentials()).SSHPrivateKey != secret {
		t.Fatalf("reopen with the right key: %v", err)
	}
	if _, err := OpenStore(r.cfg.DataDir, "wrong"); err == nil {
		t.Fatal("reopen with the wrong key must fail")
	}
}

func TestHookAndLaunchValidation(t *testing.T) {
	r := newRig(t, func(c *Config) { c.BatchWindow, c.HookToken = time.Hour, "node" })
	srv := NewServer(r.cfg, r.st, r.eng)
	do := func(method, path, token, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if strings.HasPrefix(body, "{") {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"hook needs its token", "POST", "/api/v1/hook?host=n1", "nope", "", 401},
		{"hook token cannot read", "GET", "/api/v1/runs", "node", "", 401},
		{"hook by query", "POST", "/api/v1/hook?host=n1&branch=main&playbook=other.yml", "node", "", 202},
		{"hook by json", "POST", "/api/v1/hook", "node", `{"host":"n2","branch":"main","extra_vars":{"a":1}}`, 202},
		{"flag as branch", "POST", "/api/v1/hook?host=n3&branch=--upload-pack=x", "node", "", 400},
		{"playbook leaves repo", "POST", "/api/v1/hook?host=n3&playbook=../../etc/passwd", "node", "", 400},
		{"flag as playbook", "POST", "/api/v1/hook?host=n3&playbook=-e", "node", "", 400},
		{"host with shell", "POST", "/api/v1/hook?host=n3%24(id)", "node", "", 400},
		{"launch needs hosts", "POST", "/api/v1/runs", "admin", `{}`, 400},
		{"launch", "POST", "/api/v1/runs", "admin", `{"hosts":["n4","n5","n4"],"tags":["a"]}`, 202},
		{"tag injection", "POST", "/api/v1/runs", "admin", `{"hosts":["n6"],"tags":["a,--flag"]}`, 400},
		{"unknown host", "GET", "/api/v1/runs/zz", "read", "", 404},
		{"index is open", "GET", "/api/v1", "", "", 200},
	} {
		if code, out := do(c.method, c.path, c.token, c.body); code != c.want {
			t.Errorf("%s: got %d want %d: %s", c.name, code, c.want, out)
		}
	}
	if run := r.st.GetRun("n1"); run == nil || run.Playbook != "other.yml" || run.Source != "hook" {
		t.Fatalf("hook run: %+v", run)
	}
	if s := r.eng.Status(); s.Queued != 4 {
		t.Fatalf("want n1 n2 n4 n5 queued, got %d", s.Queued)
	}
}
