package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type event struct {
	E            string `json:"e"` // task | stats
	Host         string `json:"host"`
	Task         string `json:"task"`
	Status       string `json:"status"`
	Msg          string `json:"msg"`
	Stderr       string `json:"stderr"`
	Stdout       string `json:"stdout"`
	IgnoreErrors bool   `json:"ignore_errors"`
	OK           int    `json:"ok"`
	Changed      int    `json:"changed"`
	Failures     int    `json:"failures"`
	Unreachable  int    `json:"unreachable"`
	Skipped      int    `json:"skipped"`
	Rescued      int    `json:"rescued"`
	Ignored      int    `json:"ignored"`
}

// hostResult is what ansible reported for one host. stats is set only when
// ansible printed its final summary for the host, which is the finish marker.
type hostResult struct {
	events []event
	stats  *Stats
}

func (h *hostResult) failedTasks() []FailedTask {
	var out []FailedTask
	for _, ev := range h.events {
		if (ev.Status == "failed" && !ev.IgnoreErrors) || ev.Status == "unreachable" {
			if len(out) == 20 {
				break
			}
			out = append(out, FailedTask{Task: ev.Task, Msg: ev.Msg})
		}
	}
	return out
}

type execResult struct {
	commit   string
	infraErr string // awflex could not start ansible at all
	exitErr  error
	crashed  bool // ansible was killed by a signal awflex did not send
	timedOut bool
	rawTail  string
	hosts    map[string]*hostResult
	hostErr  map[string]string // hosts awflex refused to run, with the reason
}

func parseEvents(path string) map[string]*hostResult {
	out := map[string]*hostResult{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Host == "" {
			continue
		}
		hr := out[ev.Host]
		if hr == nil {
			hr = &hostResult{}
			out[ev.Host] = hr
		}
		if ev.E == "stats" {
			hr.stats = &Stats{OK: ev.OK, Changed: ev.Changed, Failed: ev.Failures, Unreachable: ev.Unreachable,
				Skipped: ev.Skipped, Rescued: ev.Rescued, Ignored: ev.Ignored}
		} else {
			hr.events = append(hr.events, ev)
		}
	}
	return out
}

// mergeCreds folds every credential into one; later names override earlier ones.
func mergeCreds(cs []*Credential) Credential {
	m := Credential{Env: map[string]string{}}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	for _, c := range cs {
		set(&m.SSHUser, c.SSHUser)
		set(&m.SSHPrivateKey, c.SSHPrivateKey)
		set(&m.BecomePassword, c.BecomePassword)
		set(&m.VaultPassword, c.VaultPassword)
		set(&m.GitUser, c.GitUser)
		set(&m.GitToken, c.GitToken)
		set(&m.GitSSHKey, c.GitSSHKey)
		for k, v := range c.Env {
			m.Env[k] = v
		}
	}
	return m
}

func withNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// limitWriter stops writing after n bytes so a runaway playbook cannot fill the disk.
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if int64(len(q)) > l.n {
		q = q[:l.n]
	}
	l.n -= int64(len(q))
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	if l.n <= 0 {
		l.w.Write([]byte("\n[awflex: output truncated]\n"))
	}
	return len(p), nil
}

// execute runs one ansible-playbook process for the batch.
func (e *Engine) execute(ctx context.Context, b *batch) (res execResult) {
	res.hosts, res.hostErr = map[string]*hostResult{}, map[string]string{}
	fail := func(f string, a ...any) execResult {
		res.infraErr = fmt.Sprintf(f, a...)
		return res
	}

	tmp := filepath.Dir(b.events)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fail("scratch dir: %v", err)
	}
	defer os.RemoveAll(tmp)
	var werr error
	write := func(name, data string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil && werr == nil {
			werr = err
		}
		return p
	}

	cred := mergeCreds(e.st.ListCredentials())

	var gitAuth []string
	if cred.GitSSHKey != "" {
		key := write("git_key", withNL(cred.GitSSHKey))
		known := filepath.Join(e.cfg.DataDir, "git", "known_hosts")
		gitAuth = append(gitAuth, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i '%s' -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile='%s'", key, known))
	}
	if cred.GitToken != "" {
		user := cred.GitUser
		if user == "" {
			user = "git"
		}
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + cred.GitToken))
		gitAuth = append(gitAuth, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	sha, dir, release, err := e.git.Checkout(ctx, b.req.Ref, gitAuth)
	if err != nil {
		return fail("%v", err)
	}
	defer release()
	res.commit = sha

	pb, err := cleanRel(b.req.Playbook)
	if err != nil {
		return fail("%v", err)
	}
	if st, err := os.Stat(filepath.Join(dir, pb)); err != nil || st.IsDir() {
		return fail("playbook %q not found in %s at %s (%s)", pb, e.cfg.Repo, b.req.Ref, sha[:12])
	}

	rawPath := e.st.batchLogPath(b.id)
	raw, err := os.OpenFile(rawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fail("batch log: %v", err)
	}
	defer raw.Close()
	out := &limitWriter{w: raw, n: 32 << 20}

	// The playbook gets a minimal environment: awflex's own tokens never reach it.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
		"PYTHONUNBUFFERED=1",
		"ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_NOCOLOR=1",
		"ANSIBLE_RETRY_FILES_ENABLED=False",
	}
	if t := os.Getenv("TMPDIR"); t != "" {
		env = append(env, "TMPDIR="+t)
	}
	callbacks := "awflex"
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANSIBLE_CALLBACKS_ENABLED=") {
			callbacks = strings.TrimPrefix(kv, "ANSIBLE_CALLBACKS_ENABLED=") + ",awflex"
		} else if strings.HasPrefix(kv, "ANSIBLE_") {
			env = append(env, kv)
		}
	}
	for k, v := range cred.Env {
		env = append(env, k+"="+v)
	}

	var secretArgs []string
	if cred.SSHPrivateKey != "" {
		key := write("ssh_key", withNL(cred.SSHPrivateKey))
		secretArgs = append(secretArgs, "--private-key", key)
		if e.cfg.SSHAgent && werr == nil {
			// The key is also held by an agent that lives only for this run, so
			// jump hosts and agent forwarding work, not just a direct connection.
			sock, stop, err := startAgent(tmp, key, e.cfg.Timeout+time.Minute)
			if err != nil {
				fmt.Fprintf(out, "[awflex] no ssh-agent for this run (%v); the key file is still passed to ansible\n", err)
			} else {
				defer stop()
				env = append(env, "SSH_AUTH_SOCK="+sock)
			}
		}
	}
	if cred.SSHUser != "" {
		secretArgs = append(secretArgs, "-u", cred.SSHUser)
	}
	var vaultArgs []string
	if cred.VaultPassword != "" {
		vaultArgs = []string{"--vault-password-file", write("vault_pass", cred.VaultPassword)}
	}

	// With an inventory configured, only hosts that are in it are run.
	hosts := b.hosts
	var invArgs []string
	for _, inv := range e.cfg.Inventory {
		invArgs = append(invArgs, "-i", inv)
	}
	if len(invArgs) > 0 && e.cfg.InventoryCheck {
		known, err := e.inventoryHosts(ctx, sha, dir, env, append(append([]string{}, invArgs...), vaultArgs...))
		if err != nil {
			return fail("%v", err)
		}
		hosts = nil
		for _, h := range b.hosts {
			if known[h] {
				hosts = append(hosts, h)
			} else {
				res.hostErr[h] = "host is not in the configured inventory; nothing was run"
			}
		}
		if len(hosts) == 0 {
			return res
		}
	}

	args := []string{pb, "-f", strconv.Itoa(b.forks)}
	hostLines := strings.Join(hosts, "\n") + "\n"
	if len(invArgs) > 0 {
		args = append(args, invArgs...)
		args = append(args, "--limit", "@"+write("limit", hostLines))
	} else {
		ini := "[awflex]\n" + hostLines
		for _, g := range e.cfg.Groups {
			ini += "[" + g + "]\n" + hostLines
		}
		args = append(args, "-i", write("inventory.ini", ini))
	}

	vars := map[string]any{}
	for k, v := range e.cfg.ExtraVars {
		vars[k] = v
	}
	for k, v := range b.req.ExtraVars {
		vars[k] = v
	}
	vars["awflex_ref"], vars["awflex_commit"], vars["awflex_batch_id"] = b.req.Ref, sha, b.id
	vj, _ := json.Marshal(vars)
	args = append(args, "-e", "@"+write("vars.json", string(vj)))
	if cred.BecomePassword != "" {
		bj, _ := json.Marshal(map[string]string{"ansible_become_password": cred.BecomePassword})
		args = append(args, "-e", "@"+write("become.json", string(bj)))
	}
	args = append(args, secretArgs...)
	args = append(args, vaultArgs...)
	if len(b.req.Tags) > 0 {
		args = append(args, "--tags", strings.Join(b.req.Tags, ","))
	}
	if len(b.req.SkipTags) > 0 {
		args = append(args, "--skip-tags", strings.Join(b.req.SkipTags, ","))
	}
	if b.req.Check {
		args = append(args, "--check")
	}
	if werr != nil {
		return fail("scratch dir: %v", werr)
	}

	env = append(env,
		"ANSIBLE_CALLBACK_PLUGINS="+e.pluginDir,
		"ANSIBLE_CALLBACKS_ENABLED="+callbacks,
		"AWFLEX_EVENTS="+b.events,
	)

	cmd := exec.Command(e.cfg.AnsibleBin, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so ssh children die with it
	if err := cmd.Start(); err != nil {
		return fail("start %s: %v", e.cfg.AnsibleBin, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	killed := false
	select {
	case res.exitErr = <-done:
	case <-ctx.Done():
		killed = true
		res.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case res.exitErr = <-done:
		case <-time.After(10 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			res.exitErr = <-done
		}
	}
	var xe *exec.ExitError
	if !killed && errors.As(res.exitErr, &xe) {
		if ws, ok := xe.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.crashed = true
		}
	}

	res.hosts = parseEvents(b.events)
	if rb, err := os.ReadFile(rawPath); err == nil {
		res.rawTail = strings.TrimSpace(tail(rb, 1500))
	}
	return res
}

// classify turns what ansible reported into the host's final status.
func classify(host string, hr *hostResult, res execResult, userCanceled bool) (status, msg string) {
	if m := res.hostErr[host]; m != "" {
		return StError, m
	}
	if hr != nil && hr.stats != nil {
		switch {
		case hr.stats.Unreachable > 0:
			return StUnreachable, ""
		case hr.stats.Failed > 0:
			return StFailed, ""
		}
		return StSuccessful, ""
	}
	switch {
	case res.infraErr != "":
		return StError, res.infraErr
	case userCanceled:
		return StCanceled, "batch canceled before ansible finished this host"
	case res.timedOut:
		return StTimeout, "ansible run exceeded the timeout before finishing this host"
	}
	if hr != nil {
		for _, ev := range hr.events {
			if ev.Status == "unreachable" {
				return StUnreachable, ""
			}
		}
		for _, ev := range hr.events {
			if ev.Status == "failed" && !ev.IgnoreErrors {
				return StFailed, ""
			}
		}
	}
	if res.crashed {
		return StError, fmt.Sprintf("ansible was killed before finishing this host (%v), likely out of memory", res.exitErr)
	}
	if res.exitErr != nil {
		return StError, fmt.Sprintf("ansible-playbook failed (%v): %s", res.exitErr, res.rawTail)
	}
	return StError, "no tasks ran for this host: it is not in the inventory or the play's hosts pattern"
}

// renderLog is the per-host log: only this host's tasks, readable without the rest of the batch.
func renderLog(r *Run, hr *hostResult) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "awflex run %s host=%s ref=%s commit=%s playbook=%s batch=%s attempt=%d\n",
		r.ID, r.Host, r.Ref, r.Commit, r.Playbook, r.BatchID, r.Attempts)
	if hr != nil {
		for _, ev := range hr.events {
			label := ev.Status
			switch {
			case ev.Status == "failed" && ev.IgnoreErrors:
				label = "failed (ignored)"
			case ev.Status == "failed" || ev.Status == "unreachable":
				label = strings.ToUpper(ev.Status)
			}
			fmt.Fprintf(&b, "%-12s | %s", label, ev.Task)
			if ev.Msg != "" {
				fmt.Fprintf(&b, ": %s", ev.Msg)
			}
			b.WriteByte('\n')
			for _, kv := range [][2]string{{"stderr", ev.Stderr}, {"stdout", ev.Stdout}} {
				if kv[1] != "" {
					fmt.Fprintf(&b, "             | %s: %s\n", kv[0], strings.ReplaceAll(strings.TrimSpace(kv[1]), "\n", "\n             |   "))
				}
			}
		}
		if s := hr.stats; s != nil {
			fmt.Fprintf(&b, "stats: ok=%d changed=%d failed=%d unreachable=%d skipped=%d rescued=%d ignored=%d\n",
				s.OK, s.Changed, s.Failed, s.Unreachable, s.Skipped, s.Rescued, s.Ignored)
		}
	}
	fmt.Fprintf(&b, "status: %s\n", r.Status)
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", r.Error)
	}
	return b.Bytes()
}

// startAgent runs an ssh-agent that holds one key for the length of one
// ansible run. Ansible can use the key file directly, but a connection that
// goes through a jump host with agent forwarding needs the key in an agent.
func startAgent(dir, keyPath string, life time.Duration) (sock string, stop func(), err error) {
	sockDir := dir
	// Unix socket paths are limited to about 100 bytes.
	if len(filepath.Join(dir, "agent.sock")) > 100 {
		if sockDir, err = os.MkdirTemp("/tmp", "awflex-agent-"); err != nil {
			return "", nil, err
		}
	}
	sock = filepath.Join(sockDir, "agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	agent.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if err = agent.Start(); err != nil {
		return "", nil, err
	}
	// Lets the next start find and stop this agent if awflex is killed first.
	os.WriteFile(filepath.Join(dir, agentPidFile), []byte(strconv.Itoa(agent.Process.Pid)), 0o600)
	stop = func() {
		agent.Process.Kill()
		agent.Wait()
		os.Remove(sock)
		if sockDir != dir {
			os.RemoveAll(sockDir)
		}
	}
	for i := 0; ; i++ {
		if _, serr := os.Stat(sock); serr == nil {
			break
		}
		if i == 100 {
			stop()
			return "", nil, errors.New("ssh-agent did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The key expires on its own shortly after the longest possible run.
	add := exec.Command("ssh-add", "-t", strconv.Itoa(int(life.Seconds())), keyPath)
	add.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "SSH_AUTH_SOCK=" + sock, "SSH_ASKPASS_REQUIRE=never"}
	if msg, aerr := add.CombinedOutput(); aerr != nil {
		stop()
		return "", nil, fmt.Errorf("ssh-add: %v: %s", aerr, strings.TrimSpace(tail(msg, 200)))
	}
	return sock, stop, nil
}

const agentPidFile = "agent.pid"

// stopStaleAgents ends agents left by a previous process that was killed
// before it could clean up. It only touches pids that are still an ssh-agent.
func stopStaleAgents(tmpDir string) {
	files, _ := filepath.Glob(filepath.Join(tmpDir, "run-*", agentPidFile))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 1 {
			continue
		}
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err == nil && strings.Contains(string(out), "ssh-agent") {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
}

const inventoryTTL = 30 * time.Second

type inventoryCache struct {
	at    time.Time
	hosts map[string]bool
}

// inventoryHosts asks ansible which hosts the configured inventory holds. The
// answer is reused briefly, so a burst of batches costs one lookup.
func (e *Engine) inventoryHosts(ctx context.Context, sha, dir string, env, args []string) (map[string]bool, error) {
	e.invMu.Lock()
	defer e.invMu.Unlock()
	if c, ok := e.inv[sha]; ok && time.Since(c.at) < inventoryTTL {
		return c.hosts, nil
	}
	cmd := exec.CommandContext(ctx, e.cfg.InventoryBin, append(args, "--list")...)
	cmd.Dir, cmd.Env = dir, env
	var errb bytes.Buffer
	cmd.Stderr = &errb
	outb, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading the inventory failed (%v): %s", err, strings.TrimSpace(tail(errb.Bytes(), 600)))
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(outb, &doc); err != nil {
		return nil, fmt.Errorf("reading the inventory failed: unexpected output from %s", e.cfg.InventoryBin)
	}
	hosts := map[string]bool{}
	for name, rawGroup := range doc {
		if name == "_meta" {
			var meta struct {
				Hostvars map[string]json.RawMessage `json:"hostvars"`
			}
			json.Unmarshal(rawGroup, &meta)
			for h := range meta.Hostvars {
				hosts[h] = true
			}
			continue
		}
		var group struct {
			Hosts []string `json:"hosts"`
		}
		json.Unmarshal(rawGroup, &group)
		for _, h := range group.Hosts {
			hosts[h] = true
		}
	}
	for k, c := range e.inv { // drop stale commits
		if time.Since(c.at) >= inventoryTTL {
			delete(e.inv, k)
		}
	}
	e.inv[sha] = inventoryCache{at: time.Now(), hosts: hosts}
	return hosts, nil
}
