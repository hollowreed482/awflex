package core

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (r *rig) commit(name, content string) {
	r.t.Helper()
	repo := filepath.Join(r.dir, "repo")
	os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
	os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644)
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "add " + name}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			r.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func check(t *testing.T, rep DoctorReport, name string) Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, rep.Checks)
	return Check{}
}

func TestDoctorHealthyDeployment(t *testing.T) {
	r := newRig(t, nil)
	r.commit("ansible.cfg", "[defaults]\nstdout_callback = community.general.yaml\n")
	rep := r.eng.Doctor(context.Background(), "node001")
	if !rep.OK {
		t.Fatalf("healthy rig should pass: %+v", rep.Checks)
	}
	for _, name := range []string{"version", "user", "writable:data_dir", "tool:ansible-playbook", "repo", "playbook", "syntax", "targets", "connect"} {
		if c := check(t, rep, name); c.Status != "ok" {
			t.Fatalf("%s: %+v", name, c)
		}
	}
	if c := check(t, rep, "credentials"); c.Status != "warn" || !strings.Contains(c.Fix, "awflex cred set") {
		t.Fatalf("no ssh key should warn with the command: %+v", c)
	}
	if c := check(t, rep, "callbacks"); c.Status != "ok" || !strings.Contains(c.Detail, "community.general.yaml") || !strings.Contains(c.Detail, "overrides") {
		t.Fatalf("repo stdout_callback should be reported as overridden: %+v", c)
	}
	if r.st.GetRun("node001") != nil {
		t.Fatal("doctor must not leave a run record behind")
	}
	if m, _ := filepath.Glob(filepath.Join(r.cfg.TmpDir, "doctor-*")); len(m) != 0 {
		t.Fatalf("doctor left scratch dirs: %v", m)
	}
}

func TestDoctorNamesTheFix(t *testing.T) {
	t.Run("user without passwd entry", func(t *testing.T) {
		r := newRig(t, nil)
		old := lookupUID
		lookupUID = func(int) error { return errors.New("unknown userid") }
		defer func() { lookupUID = old }()
		rep := r.eng.Doctor(context.Background(), "")
		if c := check(t, rep, "user"); rep.OK || c.Status != "fail" || !strings.Contains(c.Fix, "Dockerfile.base") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("missing collection", func(t *testing.T) {
		r := newRig(t, nil)
		r.mode("syntaxfail")
		rep := r.eng.Doctor(context.Background(), "")
		if c := check(t, rep, "syntax"); rep.OK || c.Status != "fail" || !strings.Contains(c.Fix, "collection") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("play targets nothing", func(t *testing.T) {
		r := newRig(t, nil)
		r.mode("nohosts")
		rep := r.eng.Doctor(context.Background(), "node001")
		if c := check(t, rep, "targets"); c.Status != "fail" || !strings.Contains(c.Fix, "AWFLEX_GROUPS") {
			t.Fatalf("%+v", c)
		}
		if c := check(t, rep, "connect"); c.Status != "skip" {
			t.Fatalf("connect should be skipped: %+v", c)
		}
	})
	t.Run("ssh fails on the node", func(t *testing.T) {
		r := newRig(t, nil)
		r.mode("unreach")
		rep := r.eng.Doctor(context.Background(), "node001")
		if c := check(t, rep, "connect"); c.Status != "fail" || !strings.Contains(c.Detail, "No user exists") || !strings.Contains(c.Fix, "Dockerfile.base") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("bad playbook skips the rest", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Playbook = "site.yml" })
		rep := r.eng.Doctor(context.Background(), "node001")
		if c := check(t, rep, "playbook"); c.Status != "fail" || !strings.Contains(c.Fix, "AWFLEX_PLAYBOOK") {
			t.Fatalf("%+v", c)
		}
		if c := check(t, rep, "syntax"); c.Status != "skip" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("missing inventory file", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Inventory = []string{"inventory/hosts.ini"} })
		rep := r.eng.Doctor(context.Background(), "")
		if c := check(t, rep, "inventory"); c.Status != "fail" || !strings.Contains(c.Fix, "AWFLEX_INVENTORY") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("configured inventory", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Inventory = []string{"inventory/hosts.ini"} })
		r.commit("inventory/hosts.ini", "[compute]\nnode001\nnode002\n")
		rep := r.eng.Doctor(context.Background(), "")
		if c := check(t, rep, "targets"); c.Status != "ok" || !strings.Contains(c.Detail, "2 hosts") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("pod names a callback the image lacks", func(t *testing.T) {
		r := newRig(t, nil)
		doc := "#!/bin/sh\ncase \"$4\" in *missing*) echo '{}';; *) echo '{\"x\":{}}';; esac\n"
		os.WriteFile(filepath.Join(filepath.Dir(r.cfg.AnsibleBin), "ansible-doc"), []byte(doc), 0o755)
		t.Setenv("ANSIBLE_STDOUT_CALLBACK", "example.missing.yaml")
		t.Setenv("ANSIBLE_CALLBACKS_ENABLED", "timer")
		rep := r.eng.Doctor(context.Background(), "")
		if c := check(t, rep, "callbacks"); rep.OK || c.Status != "fail" || !strings.Contains(c.Detail, "example.missing.yaml") || strings.Contains(c.Detail, "timer") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("pod overrides stdout callback", func(t *testing.T) {
		r := newRig(t, nil)
		t.Setenv("ANSIBLE_STDOUT_CALLBACK", "yaml")
		if c := check(t, r.eng.Doctor(context.Background(), ""), "callbacks"); c.Status != "warn" {
			t.Fatalf("%+v", c)
		}
	})
}

func TestDoctorNeedsAdmin(t *testing.T) {
	r := newRig(t, nil)
	srv := NewServer(r.cfg, r.st, r.eng)
	for token, want := range map[string]int{"read": 401, "admin": 200} {
		req := httptest.NewRequest("GET", "/api/v1/doctor", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s token: %d %s", token, w.Code, w.Body)
		}
	}
}

func TestStdoutCallbackDefaultsAndPodWins(t *testing.T) {
	r := newRig(t, nil)
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	r.waitFinal("n1")
	if got := strings.TrimSpace(r.fake("stdoutcb")); got != "default" {
		t.Fatalf("runs should use the default stdout callback, got %q", got)
	}
	t.Setenv("ANSIBLE_STDOUT_CALLBACK", "minimal")
	r.eng.Submit([]string{"n2"}, Request{Ref: "main", Playbook: "main.yml"})
	r.waitFinal("n2")
	if got := strings.TrimSpace(r.fake("stdoutcb")); got != "minimal" {
		t.Fatalf("a value set on the pod should win, got %q", got)
	}
}

func TestFailedRunCarriesHint(t *testing.T) {
	r := newRig(t, nil)
	r.mode("unreach")
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	run := r.waitFinal("n1")
	if run.Status != StUnreachable || !strings.Contains(run.Hint, "Dockerfile.base") {
		t.Fatalf("%+v", run)
	}
	var log []byte
	for i := 0; i < 100 && !strings.Contains(string(log), "hint:"); i++ {
		log, _ = r.st.ReadLog("n1") // the log is written just after the record turns final
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(log), "hint: ssh needs a passwd entry") {
		t.Fatalf("log should carry the hint:\n%s", log)
	}
	r.mode("")
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	if run := r.waitFinal("n1"); run.Status != StSuccessful || run.Hint != "" {
		t.Fatalf("a successful run has no hint: %+v", run)
	}
}

func TestHintsMatchKnownFailures(t *testing.T) {
	for text, want := range map[string]string{
		"Failed to connect to the host via ssh: No user exists for uid 1000":                           "passwd entry",
		"ERROR! Invalid callback for stdout specified: community.general.yaml":                         "ANSIBLE_STDOUT_CALLBACK",
		"ERROR! couldn't resolve module/action 'community.general.thing'":                              "collection",
		"ERROR! the role 'common' was not found in /x/roles":                                           "collection, role",
		"git fetch: exit status 128: fatal: Authentication failed for 'https://g/x.git'":               "git_token",
		"git fetch: exit status 128: remote: x\nfatal: could not read Username for 'https:'":           "git_token",
		"SSL certificate problem: unable to get local issuer certificate":                              "CA",
		"git fetch: exit status 128: fatal: '/repo' does not appear to be a git repository":            "git ls-remote",
		"branch/ref \"fix\" not found in https://g/x.git":                                              "Push it",
		"playbook \"site.yml\" not found in https://g/x.git at main (abc)":                             "AWFLEX_PLAYBOOK",
		"no tasks ran for this host: it is not in the inventory or the play's hosts pattern":           "doctor --host",
		"node001 | UNREACHABLE! => Permission denied (publickey,password).":                            "ssh_private_key",
		"[ERROR]: Specified inventory, host pattern and/or --limit leaves us with no hosts to target.": "doctor --host",
		"Host key verification failed.":                                                                "StrictHostKeyChecking",
		"ssh: Could not resolve hostname node001: Name or service not known":                           "ansible_host",
		"ssh: connect to host 10.0.0.1 port 22: Connection timed out":                                  "ProxyJump",
		"Missing sudo password":                            "become-password",
		"Attempting to decrypt but no vault secrets found": "vault-password",
		"ansible was killed before finishing this host (signal: killed), likely out of memory": "AWFLEX_MAX_FORKS",
		"[Errno 30] Read-only file system: '/home/x'":                                          "tmpfs",
	} {
		if got := hintFor(text); !strings.Contains(got, want) {
			t.Errorf("%q: want a hint containing %q, got %q", text, want, got)
		}
	}
	if got := hintFor("everything is fine"); got != "" {
		t.Errorf("unrelated text got a hint: %q", got)
	}
}
