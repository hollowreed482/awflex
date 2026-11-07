package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Check is one doctor finding. Status is ok, warn, fail or skip. Fix is the
// exact action that resolves a warn or fail.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// DoctorReport is what awflex doctor returns. OK is false when any check failed.
type DoctorReport struct {
	Version string  `json:"version"`
	OK      bool    `json:"ok"`
	Host    string  `json:"host,omitempty"`
	Checks  []Check `json:"checks"`
}

// lookupUID is replaced in tests.
var lookupUID = func(uid int) error {
	_, err := user.LookupId(strconv.Itoa(uid))
	return err
}

// PasswdProblem says why ssh will fail for the current user, or "" if it will not.
// OpenSSH refuses to start when the user id has no entry in /etc/passwd, which
// is common when an image runs as a uid it never created.
func PasswdProblem() string {
	uid := os.Getuid()
	if err := lookupUID(uid); err != nil {
		return fmt.Sprintf("uid %d has no entry in /etc/passwd, so ssh will exit with \"No user exists for uid %d\" before connecting", uid, uid)
	}
	return ""
}

const passwdFix = "build the image with deploy/Containerfile.base (it adds a passwd entry for uid 1000) and keep securityContext runAsUser: 1000, or set runAsUser to a uid the image's /etc/passwd lists. Do not run as root to work around it."

const pingPlaybook = `- name: awflex doctor connection test
  hosts: all
  gather_facts: false
  tasks:
    - name: ping
      ansible.builtin.ping:
`

var listHostsRe = regexp.MustCompile(`hosts \((\d+)\):`)

// Doctor checks the real environment a run uses, step by step, and says how to
// fix each problem. With a host it also connects to that host the way a run
// would, through the same inventory, credentials, agent and ansible.cfg.
func (e *Engine) Doctor(ctx context.Context, host string) DoctorReport {
	rep := DoctorReport{Version: Version, OK: true, Host: host}
	add := func(name, status, detail, fix string) {
		if (status == "fail" || status == "warn") && fix == "" {
			fix = hintFor(detail)
		}
		if status == "fail" {
			rep.OK = false
		}
		rep.Checks = append(rep.Checks, Check{name, status, strings.TrimSpace(detail), fix})
	}
	skip := func(name, why string) { add(name, "skip", why, "") }

	add("version", "ok", "awflex "+Version, "")

	if p := PasswdProblem(); p != "" {
		add("user", "fail", p, passwdFix)
	} else {
		add("user", "ok", fmt.Sprintf("uid %d has a passwd entry", os.Getuid()), "")
	}

	for _, d := range []struct{ name, path, fix string }{
		{"home", os.Getenv("HOME"), "set HOME to a writable directory; the images use the /run/awflex tmpfs"},
		{"tmpdir", os.Getenv("TMPDIR"), "set TMPDIR to a writable directory; the images use the /run/awflex tmpfs"},
		{"data_dir", e.cfg.DataDir, "mount a writable volume at AWFLEX_DATA_DIR (/data); see the storage options in deploy/k8s/awflex.yaml"},
		{"scratch_dir", e.cfg.TmpDir, "AWFLEX_TMP_DIR must be writable; the manifest mounts a memory tmpfs at /run/awflex"},
	} {
		if d.path == "" {
			if d.name == "tmpdir" {
				continue // falls back to /tmp, which is fine when writable
			}
			add("writable:"+d.name, "fail", "not set", d.fix)
			continue
		}
		if err := writable(d.path); err != nil {
			add("writable:"+d.name, "fail", fmt.Sprintf("%s: %v", d.path, err), d.fix)
		} else {
			add("writable:"+d.name, "ok", d.path, "")
		}
	}

	if out, err := exec.CommandContext(ctx, e.cfg.AnsibleBin, "--version").CombinedOutput(); err != nil {
		add("tool:ansible-playbook", "fail", fmt.Sprintf("%s --version: %v %s", e.cfg.AnsibleBin, err, tail(out, 300)),
			"the image needs ansible-playbook on PATH (or set AWFLEX_ANSIBLE_PLAYBOOK). Build with deploy/Containerfile.base on an ansible image.")
	} else {
		add("tool:ansible-playbook", "ok", strings.SplitN(string(out), "\n", 2)[0], "")
	}
	for _, t := range []struct {
		bin      string
		required bool
	}{{e.cfg.GitBin, true}, {"ssh", true}, {"ssh-agent", e.cfg.SSHAgent}, {"ssh-add", e.cfg.SSHAgent}} {
		if p, err := exec.LookPath(t.bin); err == nil {
			add("tool:"+filepath.Base(t.bin), "ok", p, "")
		} else if t.required {
			add("tool:"+filepath.Base(t.bin), "fail", t.bin+" not found on PATH", "install git and an OpenSSH client in the image")
		} else {
			add("tool:"+filepath.Base(t.bin), "warn", t.bin+" not found on PATH", "install the OpenSSH client tools in the image, or set AWFLEX_SSH_AGENT=0 when no jump host needs agent forwarding")
		}
	}

	creds := e.st.ListCredentials()
	cred := mergeCreds(creds)
	var names []string
	for _, c := range creds {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	switch {
	case cred.SSHPrivateKey == "":
		add("credentials", "warn", fmt.Sprintf("no ssh_private_key in any credential (%d stored)", len(creds)),
			"store the node SSH key: awflex cred set fleet --ssh-user USER --ssh-key-file KEY")
	default:
		sshUser := cred.SSHUser
		if sshUser == "" {
			sshUser = "(ansible default)"
		}
		add("credentials", "ok", fmt.Sprintf("ssh key set, ssh user %s, credentials: %s", sshUser, strings.Join(names, ", ")), "")
	}

	tmp, err := os.MkdirTemp(e.cfg.TmpDir, "doctor-")
	if err != nil {
		add("repo", "fail", "scratch dir: "+err.Error(), "")
		return rep
	}
	defer os.RemoveAll(tmp)
	write := func(name, data string) string {
		p := filepath.Join(tmp, name)
		os.WriteFile(p, []byte(data), 0o600)
		return p
	}

	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	sha, dir, release, err := e.git.Checkout(cctx, e.cfg.Ref, e.gitAuth(cred, write))
	if err != nil {
		add("repo", "fail", err.Error(), "")
		for _, n := range []string{"playbook", "inventory", "syntax", "targets", "connect"} {
			skip(n, "needs the repo")
		}
		return rep
	}
	defer release()
	add("repo", "ok", fmt.Sprintf("%s branch %s at %s", e.cfg.Repo, e.cfg.Ref, sha[:12]), "")

	pbOK := false
	if st, err := os.Stat(filepath.Join(dir, e.cfg.Playbook)); err != nil || st.IsDir() {
		add("playbook", "fail", fmt.Sprintf("playbook %q not found in %s at %s", e.cfg.Playbook, e.cfg.Repo, e.cfg.Ref), "")
	} else {
		pbOK = true
		add("playbook", "ok", e.cfg.Playbook, "")
	}

	env, _ := ansibleEnv(cred)
	e.callbackChecks(cctx, dir, env, add)

	invOK := true
	if len(e.cfg.Inventory) == 0 {
		groups := append([]string{"awflex"}, e.cfg.Groups...)
		add("inventory", "ok", "generated from the calling hosts, groups: "+strings.Join(groups, ", "), "")
	} else {
		for _, inv := range e.cfg.Inventory {
			p := inv
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, inv)
			}
			if _, err := os.Stat(p); err != nil {
				invOK = false
				add("inventory", "fail", fmt.Sprintf("%s not found (repo relative paths are read from branch %s)", inv, e.cfg.Ref),
					"fix AWFLEX_INVENTORY: a path inside the repo, or an absolute path to a mounted file or directory")
			} else {
				add("inventory", "ok", inv, "")
			}
		}
	}

	if !pbOK || !invOK {
		for _, n := range []string{"syntax", "targets", "connect"} {
			skip(n, "needs the playbook and inventory")
		}
		return rep
	}

	var vault []string
	if cred.VaultPassword != "" {
		vault = []string{"--vault-password-file", write("vault_pass", cred.VaultPassword)}
	}
	sample := []string{host}
	if host == "" {
		sample = nil
		if len(e.cfg.Inventory) == 0 {
			sample = []string{"doctor-check"}
		}
	}
	ansible := func(hosts []string, extra ...string) (string, error) {
		args := append([]string{e.cfg.Playbook}, e.inventoryArgs(hosts, write)...)
		args = append(args, vault...)
		args = append(args, extra...)
		cmd := exec.CommandContext(cctx, e.cfg.AnsibleBin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	syntaxHosts := sample // the syntax check needs an inventory, not the host
	if len(e.cfg.Inventory) > 0 {
		syntaxHosts = nil
	}
	if out, err := ansible(syntaxHosts, "--syntax-check"); err != nil {
		add("syntax", "fail", fmt.Sprintf("ansible-playbook --syntax-check: %v\n%s", err, tail([]byte(out), 1200)), "")
	} else {
		add("syntax", "ok", "playbook parses with this image's collections and roles", "")
	}

	out, err := ansible(sample, "--list-hosts")
	if err != nil && strings.Contains(out, "no hosts to target") {
		err = nil // ansible's way of saying zero hosts
	}
	n := 0
	for _, m := range listHostsRe.FindAllStringSubmatch(out, -1) {
		k, _ := strconv.Atoi(m[1])
		n += k
	}
	targeted := err == nil && n > 0
	switch {
	case err != nil:
		add("targets", "fail", fmt.Sprintf("ansible-playbook --list-hosts: %v\n%s", err, tail([]byte(out), 800)), "")
	case n == 0 && host != "":
		add("targets", "fail", fmt.Sprintf("the play would not run on %s: no tasks ran for this host", host), "")
	case n == 0:
		add("targets", "fail", "the play matches no hosts in the inventory: no tasks ran for this host", "")
	case host != "":
		add("targets", "ok", fmt.Sprintf("the play runs on %s", host), "")
	default:
		add("targets", "ok", fmt.Sprintf("the play matches %d hosts", n), "")
	}

	if host == "" {
		skip("connect", "pass a host to test a real connection: awflex doctor --host HOST")
		return rep
	}
	if !targeted {
		skip("connect", "the play does not target this host")
		return rep
	}
	if !hostRe.MatchString(host) {
		add("connect", "fail", "invalid host name "+host, "")
		return rep
	}
	ctmp, err := os.MkdirTemp(e.cfg.TmpDir, "doctor-run-")
	if err != nil {
		add("connect", "fail", "scratch dir: "+err.Error(), "")
		return rep
	}
	pb := filepath.Join(ctmp, "ping.yml")
	os.WriteFile(pb, []byte(pingPlaybook), 0o600)
	b := &batch{id: "doctor-" + newID(), hosts: []string{host}, forks: 1, started: time.Now(),
		req: Request{Ref: e.cfg.Ref, Playbook: e.cfg.Playbook}, events: filepath.Join(ctmp, "events.jsonl"), playbookPath: pb}
	res := e.execute(cctx, b)
	os.Remove(e.st.batchLogPath(b.id))
	hr := res.hosts[host]
	status, msg := classify(hr, res, false)
	if status == StSuccessful {
		add("connect", "ok", fmt.Sprintf("connected to %s and ran a task, through the same inventory, credentials and ansible.cfg as a run", host), "")
		return rep
	}
	detail := []string{fmt.Sprintf("%s: %s", status, msg)}
	if hr != nil {
		for _, ev := range hr.events {
			if ev.Status == "failed" || ev.Status == "unreachable" {
				detail = append(detail, ev.Msg)
			}
		}
	}
	if msg == "" && res.rawTail != "" {
		detail = append(detail, res.rawTail)
	}
	add("connect", "fail", strings.Join(detail, "\n"), runHint(msg, hr))
	return rep
}

// callbackChecks reports the stdout callback a run will use, and whether every
// callback the pod names exists in this image. A missing one stops every run
// before its first task.
func (e *Engine) callbackChecks(ctx context.Context, repoDir string, env []string, add func(name, status, detail, fix string)) {
	pod := os.Getenv("ANSIBLE_STDOUT_CALLBACK")
	detail := "runs use ansible's default stdout callback"
	if pod != "" && pod != "default" {
		detail = "the pod sets ANSIBLE_STDOUT_CALLBACK=" + pod + ", which overrides awflex's default"
	} else if b, err := os.ReadFile(filepath.Join(repoDir, "ansible.cfg")); err == nil {
		if m := regexp.MustCompile(`(?m)^\s*stdout_callback\s*=\s*(\S+)`).FindSubmatch(b); m != nil {
			detail += fmt.Sprintf("; the repo's ansible.cfg sets stdout_callback = %s, which awflex overrides, so it cannot stop a run", m[1])
		}
	}
	var names []string
	if pod != "" && pod != "default" {
		names = append(names, pod)
	}
	for _, n := range strings.Split(os.Getenv("ANSIBLE_CALLBACKS_ENABLED"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	doc := ""
	if p, err := exec.LookPath(e.cfg.AnsibleBin); err == nil {
		if d := filepath.Join(filepath.Dir(p), "ansible-doc"); fileExists(d) {
			doc = d
		}
	}
	var missing []string
	for _, n := range names {
		if doc == "" {
			break
		}
		cmd := exec.CommandContext(ctx, doc, "-t", "callback", "-j", n)
		cmd.Dir, cmd.Env = repoDir, env
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) == "{}" {
			missing = append(missing, n)
		}
	}
	switch {
	case len(missing) > 0:
		add("callbacks", "fail", "callback plugins named in the pod env are not in this image: "+strings.Join(missing, ", ")+". Every run stops before its first task.",
			"remove them from ANSIBLE_STDOUT_CALLBACK / ANSIBLE_CALLBACKS_ENABLED in the pod env, or install the collection that provides them in the image. awflex does not need either variable.")
	case pod != "" && pod != "default":
		add("callbacks", "warn", detail, "awflex does not need ANSIBLE_STDOUT_CALLBACK; remove it from the pod env unless you want that output format in raw logs")
	default:
		add("callbacks", "ok", detail, "")
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".awflex-doctor-")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// Text renders the report one line per check, with the fix under each problem.
func (r DoctorReport) Text() string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-4s %s: %s\n", strings.ToUpper(c.Status), c.Name, strings.ReplaceAll(c.Detail, "\n", "\n       "))
		if c.Fix != "" {
			fmt.Fprintf(&b, "       fix: %s\n", c.Fix)
		}
	}
	verdict := "all checks passed"
	if !r.OK {
		verdict = "some checks FAILED: apply each fix above, then run awflex doctor again"
	}
	fmt.Fprintf(&b, "awflex %s: %s\n", r.Version, verdict)
	return b.String()
}
