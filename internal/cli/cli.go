// Package cli is the awflex command line client. It prints the server's JSON
// verbatim so that agents and jq can consume it.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"awflex/internal/core"
)

const usage = `awflex ` + core.Version + `: minimal ansible runner for Kubernetes

Server:
  awflex serve                       run the server (configured by AWFLEX_* env, see README)

Client (AWFLEX_URL, AWFLEX_TOKEN; every command prints JSON unless noted):
  awflex doctor [--host H]           START HERE when anything does not work: checks the
                                     deployment step by step and prints the fix for each
                                     failure. --host also connects to H the way a run would.
                                     Exits 3 if a check failed. Inside the pod:
                                     kubectl exec deploy/awflex -- awflex doctor --host H
  awflex run HOST... [--hosts-file F] [--branch B] [--playbook P] [-e k=v | -e @vars.json]
             [--tags a,b] [--skip-tags a,b] [--check] [--wait]
                                     launch on hosts; --wait blocks and exits 3 unless all succeed
  awflex hook [--host H] [--branch B] [--playbook P] [-e k=v]
                                     what a booting node sends: run on me. -e needs
                                     AWFLEX_HOOK_EXTRA_VARS=1 on the server.
  awflex runs [--status S]           last run of every host
  awflex get HOST [--wait SECONDS]   last run of one host
  awflex log HOST [--raw]            that host's task log (text); --raw = whole ansible output
  awflex cancel HOST                 remove a queued host from the queue
  awflex cancel-batch ID             kill a running ansible process
  awflex forget HOST                 delete a host's record
  awflex status                      queue, running batches, fork budget
  awflex config                      effective server configuration
  awflex api                         machine readable list of REST routes
  awflex cred ls
  awflex cred set NAME [--ssh-user U] [--ssh-key-file F] [--become-password-file F]
             [--vault-password-file F] [--git-user U] [--git-token-file F] [--git-ssh-key-file F]
             [--env K=V]...          secrets are read from files, never from arguments
  awflex cred rm NAME
  awflex version

Exit codes: 0 ok, 1 server or network error, 2 usage, 3 a waited run or a doctor check did not succeed.
`

type client struct {
	base, token string
	http        *http.Client
}

// do sends a request and returns the body and status.
func (c *client) do(method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// show prints the response and maps HTTP errors to exit code 1.
func (c *client) show(method, path string, body any) int {
	b, code, err := c.do(method, path, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "awflex: %v\n", err)
		fmt.Fprintf(os.Stderr, "awflex: cannot reach the server at %s. Set AWFLEX_URL, or run inside the pod: kubectl exec deploy/awflex -- awflex doctor. If the pod is not running, its last log line names the problem: kubectl logs deploy/awflex\n", c.base)
		return 1
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		fmt.Fprintf(os.Stderr, "awflex: the server refused the token. Set AWFLEX_TOKEN to the admin token from the awflex-auth Secret\n")
	}
	os.Stdout.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Println()
	}
	if code >= 400 {
		return 1
	}
	return 0
}

// parse splits args into positionals and flags. Flags in bools take no value.
func parse(args []string, bools ...string) (pos []string, flags map[string][]string, err error) {
	flags = map[string][]string{}
	isBool := map[string]bool{}
	for _, b := range bools {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-e" {
			a = "--extra-var"
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if isBool[name] {
			flags[name] = append(flags[name], "true")
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		flags[name] = append(flags[name], val)
	}
	return pos, flags, nil
}

func first(flags map[string][]string, name string) string {
	if v := flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// extraVars reads -e k=v (string values) and -e @file.json (any JSON object).
func extraVars(vals []string) (map[string]any, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, v := range vals {
		if strings.HasPrefix(v, "@") {
			b, err := os.ReadFile(v[1:])
			if err != nil {
				return nil, err
			}
			m := map[string]any{}
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, fmt.Errorf("%s: want a JSON object: %v", v[1:], err)
			}
			for k, x := range m {
				out[k] = x
			}
			continue
		}
		k, x, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("-e wants key=value or @file.json, got %q", v)
		}
		out[k] = x
	}
	return out, nil
}

func usageErr(f string, a ...any) int {
	fmt.Fprintf(os.Stderr, "awflex: "+f+"\nrun `awflex help` for usage\n", a...)
	return 2
}

func Main(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}
	if args[0] == "version" {
		fmt.Println(core.Version)
		return 0
	}
	c := &client{
		base:  strings.TrimRight(envOr("AWFLEX_URL", "http://127.0.0.1:8080"), "/"),
		token: envOr("AWFLEX_TOKEN", os.Getenv("AWFLEX_ADMIN_TOKEN")), // the latter is set inside the pod
		http:  &http.Client{Timeout: 330 * time.Second},
	}
	cmd, rest := args[0], args[1:]
	pos, flags, err := parse(rest, "check", "wait", "raw")
	if cmd == "get" {
		pos, flags, err = parse(rest)
	}
	if err != nil {
		return usageErr("%v", err)
	}
	need := func(n int, what string) bool {
		if len(pos) != n {
			usageErr("%s wants %s", cmd, what)
			return false
		}
		return true
	}

	switch cmd {
	case "status":
		return c.show("GET", "/api/v1/status", nil)
	case "config":
		return c.show("GET", "/api/v1/config", nil)
	case "doctor":
		q := ""
		if h := first(flags, "host"); h != "" {
			q = "?host=" + url.QueryEscape(h)
		}
		b, code, err := c.do("GET", "/api/v1/doctor"+q, nil)
		if err != nil || code != http.StatusOK {
			return c.show("GET", "/api/v1/doctor"+q, nil)
		}
		os.Stdout.Write(b)
		var rep struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(b, &rep) == nil && !rep.OK {
			return 3
		}
		return 0
	case "api":
		return c.show("GET", "/api/v1", nil)
	case "runs":
		q := ""
		if s := first(flags, "status"); s != "" {
			q = "?status=" + url.QueryEscape(s)
		}
		return c.show("GET", "/api/v1/runs"+q, nil)
	case "get":
		if !need(1, "HOST") {
			return 2
		}
		q := ""
		if w := first(flags, "wait"); w != "" {
			q = "?wait=" + url.QueryEscape(w)
		}
		return c.show("GET", "/api/v1/runs/"+url.PathEscape(pos[0])+q, nil)
	case "log":
		if !need(1, "HOST") {
			return 2
		}
		q := ""
		if first(flags, "raw") != "" {
			q = "?raw=1"
		}
		return c.show("GET", "/api/v1/runs/"+url.PathEscape(pos[0])+"/log"+q, nil)
	case "cancel":
		if !need(1, "HOST") {
			return 2
		}
		return c.show("POST", "/api/v1/runs/"+url.PathEscape(pos[0])+"/cancel", nil)
	case "cancel-batch":
		if !need(1, "a batch ID") {
			return 2
		}
		return c.show("POST", "/api/v1/batches/"+url.PathEscape(pos[0])+"/cancel", nil)
	case "forget":
		if !need(1, "HOST") {
			return 2
		}
		return c.show("DELETE", "/api/v1/runs/"+url.PathEscape(pos[0]), nil)
	case "run", "hook":
		return c.launch(cmd, pos, flags)
	case "cred":
		return c.cred(pos, flags)
	}
	return usageErr("unknown command %q", cmd)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func (c *client) launch(cmd string, pos []string, flags map[string][]string) int {
	vars, err := extraVars(flags["extra-var"])
	if err != nil {
		return usageErr("%v", err)
	}
	body := map[string]any{}
	if v := first(flags, "branch"); v != "" {
		body["branch"] = v
	}
	if v := first(flags, "playbook"); v != "" {
		body["playbook"] = v
	}
	if vars != nil {
		body["extra_vars"] = vars
	}
	if cmd == "hook" {
		if v := first(flags, "host"); v != "" {
			body["host"] = v
		}
		return c.show("POST", "/api/v1/hook", body)
	}

	hosts := pos
	if f := first(flags, "hosts-file"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return usageErr("%v", err)
		}
		hosts = append(hosts, strings.Fields(string(b))...)
	}
	if len(hosts) == 0 {
		return usageErr("run wants at least one HOST or --hosts-file")
	}
	body["hosts"] = hosts
	if v := first(flags, "tags"); v != "" {
		body["tags"] = csv(v)
	}
	if v := first(flags, "skip-tags"); v != "" {
		body["skip_tags"] = csv(v)
	}
	if first(flags, "check") != "" {
		body["check"] = true
	}
	if first(flags, "wait") == "" {
		return c.show("POST", "/api/v1/runs", body)
	}

	// --wait: launch, then poll each host until final and print the final records.
	b, code, err := c.do("POST", "/api/v1/runs", body)
	if err != nil || code >= 400 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "awflex: %v\n", err)
		}
		os.Stdout.Write(b)
		return 1
	}
	type run struct {
		ID     string `json:"id"`
		Host   string `json:"host"`
		Status string `json:"status"`
	}
	final := []json.RawMessage{}
	ok := true
	for _, h := range hosts {
		for {
			b, code, err := c.do("GET", "/api/v1/runs/"+url.PathEscape(h)+"?wait=60", nil)
			if err != nil || code >= 400 {
				fmt.Fprintf(os.Stderr, "awflex: waiting for %s: %v %s\n", h, err, b)
				return 1
			}
			var r run
			json.Unmarshal(b, &r)
			if r.Status != core.StQueued && r.Status != core.StRunning {
				ok = ok && r.Status == core.StSuccessful
				final = append(final, json.RawMessage(bytes.TrimSpace(b)))
				break
			}
		}
	}
	out, _ := json.Marshal(map[string]any{"ok": ok, "runs": final})
	fmt.Println(string(out))
	if !ok {
		return 3
	}
	return 0
}

func (c *client) cred(pos []string, flags map[string][]string) int {
	if len(pos) == 0 {
		return usageErr("cred wants ls, set NAME or rm NAME")
	}
	switch {
	case pos[0] == "ls" && len(pos) == 1:
		return c.show("GET", "/api/v1/credentials", nil)
	case pos[0] == "rm" && len(pos) == 2:
		return c.show("DELETE", "/api/v1/credentials/"+url.PathEscape(pos[1]), nil)
	case pos[0] == "set" && len(pos) == 2:
		body := map[string]any{}
		for flag, field := range map[string]string{"ssh-user": "ssh_user", "git-user": "git_user"} {
			if v := first(flags, flag); v != "" {
				body[field] = v
			}
		}
		for flag, field := range map[string]string{
			"ssh-key-file": "ssh_private_key", "become-password-file": "become_password",
			"vault-password-file": "vault_password", "git-token-file": "git_token", "git-ssh-key-file": "git_ssh_key",
		} {
			f := first(flags, flag)
			if f == "" {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				return usageErr("%v", err)
			}
			v := string(b)
			if !strings.HasSuffix(field, "_key") {
				v = strings.TrimSpace(v)
			}
			body[field] = v
		}
		if len(flags["env"]) > 0 {
			env := map[string]string{}
			for _, kv := range flags["env"] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return usageErr("--env wants K=V, got %q", kv)
				}
				env[k] = v
			}
			body["env"] = env
		}
		if len(body) == 0 {
			return usageErr("cred set wants at least one field")
		}
		return c.show("PUT", "/api/v1/credentials/"+url.PathEscape(pos[1]), body)
	}
	return usageErr("cred wants ls, set NAME or rm NAME")
}
