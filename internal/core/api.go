package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Server struct {
	cfg    Config
	st     *Store
	eng    *Engine
	routes []route
	mux    *http.ServeMux
}

// route doubles as the registration and the self-description served at /api/v1.
type route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"` // none | hook | read | admin
	Desc   string `json:"description"`
	h      http.HandlerFunc
}

func NewServer(cfg Config, st *Store, eng *Engine) *Server {
	s := &Server{cfg: cfg, st: st, eng: eng, mux: http.NewServeMux()}
	s.routes = []route{
		{"GET", "/healthz", "none", "Liveness.", s.health},
		{"GET", "/metrics", "none", "Prometheus metrics.", s.metrics},
		{"GET", "/api/v1", "none", "This list of routes.", s.index},
		{"POST", "/api/v1/hook", "hook", "Node callback: run on me. Params (query, form or JSON): host (default: reverse DNS of the caller), branch, playbook, extra_vars (JSON object). Returns 202 and the queued run.", s.hook},
		{"POST", "/api/v1/runs", "admin", "Launch on hosts. JSON: {hosts:[...], branch, playbook, extra_vars:{}, tags:[], skip_tags:[], check:false}. Returns 202 and one run per host.", s.launch},
		{"GET", "/api/v1/runs", "read", "Last run of every host. Filter with ?status=failed etc.", s.listRuns},
		{"GET", "/api/v1/runs/{host}", "read", "Last run of one host. ?wait=SECONDS blocks until it is final.", s.getRun},
		{"GET", "/api/v1/runs/{host}/log", "read", "That host's task log as text. ?raw=1 returns the whole ansible output of its batch.", s.getLog},
		{"POST", "/api/v1/runs/{host}/cancel", "admin", "Remove a queued host from the queue.", s.cancelRun},
		{"DELETE", "/api/v1/runs/{host}", "admin", "Forget a host (decommissioned node).", s.forget},
		{"POST", "/api/v1/batches/{id}/cancel", "admin", "Kill a running ansible process; its unfinished hosts become canceled.", s.cancelBatch},
		{"GET", "/api/v1/status", "read", "Queue, running batches, fork budget and host counts by status.", s.status},
		{"GET", "/api/v1/config", "read", "Effective non-secret configuration.", s.config},
		{"GET", "/api/v1/credentials", "read", "Credential names and which fields are set. Secrets are never returned.", s.listCreds},
		{"PUT", "/api/v1/credentials/{name}", "admin", "Create or replace a credential. JSON fields: ssh_user, ssh_private_key, become_password, vault_password, git_user, git_token, git_ssh_key, env:{}.", s.putCred},
		{"DELETE", "/api/v1/credentials/{name}", "admin", "Delete a credential.", s.deleteCred},
	}
	for _, r := range s.routes {
		s.mux.HandleFunc(r.Method+" "+r.Path, s.guard(r.Auth, r.h))
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	s.mux.ServeHTTP(w, r)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, f string, a ...any) {
	reply(w, code, map[string]string{"error": fmt.Sprintf(f, a...)})
}

func tokenEq(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) guard(level string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if level == "none" || s.cfg.NoAuth {
			h(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ok := tokenEq(tok, s.cfg.AdminToken)
		switch level {
		case "read":
			ok = ok || tokenEq(tok, s.cfg.ReadToken)
		case "hook":
			ok = ok || s.cfg.HookToken == "" || tokenEq(tok, s.cfg.HookToken)
		}
		if !ok {
			fail(w, http.StatusUnauthorized, "missing or wrong bearer token for a route that needs %s access", level)
			return
		}
		h(w, r)
	}
}

func (s *Server) callerIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- launching

type launchBody struct {
	Host      string         `json:"host"`
	Hosts     []string       `json:"hosts"`
	Ref       string         `json:"ref"`
	Branch    string         `json:"branch"`
	Playbook  string         `json:"playbook"`
	ExtraVars map[string]any `json:"extra_vars"`
	Tags      []string       `json:"tags"`
	SkipTags  []string       `json:"skip_tags"`
	Check     bool           `json:"check"`
}

// request validates a body and fills the configured defaults.
func (s *Server) request(b launchBody) (Request, []string, error) {
	req := Request{Ref: b.Ref, Playbook: b.Playbook, ExtraVars: b.ExtraVars, Tags: b.Tags, SkipTags: b.SkipTags, Check: b.Check}
	if req.Ref == "" {
		req.Ref = b.Branch
	}
	if req.Ref == "" {
		req.Ref = s.cfg.Ref
	}
	if req.Playbook == "" {
		req.Playbook = s.cfg.Playbook
	}
	if len(req.ExtraVars) == 0 {
		req.ExtraVars = nil
	}
	if err := validRef(req.Ref); err != nil {
		return req, nil, err
	}
	pb, err := cleanRel(req.Playbook)
	if err != nil {
		return req, nil, fmt.Errorf("playbook: %w", err)
	}
	req.Playbook = pb
	if err := validWords("tag", append(append([]string{}, req.Tags...), req.SkipTags...)); err != nil {
		return req, nil, err
	}
	hosts := b.Hosts
	if b.Host != "" {
		hosts = append(hosts, b.Host)
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		if !hostRe.MatchString(h) {
			return req, nil, fmt.Errorf("invalid host name %q", h)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return req, out, nil
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	var b launchBody
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
			fail(w, http.StatusBadRequest, "invalid JSON body: %v", err)
			return
		}
	}
	if err := r.ParseForm(); err != nil { // query string and form bodies
		fail(w, http.StatusBadRequest, "invalid query or form: %v", err)
		return
	}
	get := func(dst *string, keys ...string) {
		for _, k := range keys {
			if v := r.Form.Get(k); v != "" && *dst == "" {
				*dst = v
			}
		}
	}
	get(&b.Host, "host")
	get(&b.Ref, "branch", "ref")
	get(&b.Playbook, "playbook")
	if v := r.Form.Get("extra_vars"); v != "" && b.ExtraVars == nil {
		if err := json.Unmarshal([]byte(v), &b.ExtraVars); err != nil {
			fail(w, http.StatusBadRequest, "extra_vars: want a JSON object: %v", err)
			return
		}
	}
	b.Hosts, b.Tags, b.SkipTags, b.Check = nil, nil, nil, false // a node speaks only for itself

	ip := s.callerIP(r)
	if b.Host == "" {
		names, _ := net.LookupAddr(ip)
		if len(names) == 0 {
			fail(w, http.StatusBadRequest, "no host given and %s has no reverse DNS name; pass ?host=", ip)
			return
		}
		b.Host = strings.TrimSuffix(names[0], ".")
	}
	req, hosts, err := s.request(b)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	req.Source, req.From = "hook", ip
	reply(w, http.StatusAccepted, s.eng.Submit(hosts, req)[0])
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	var b launchBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}
	req, hosts, err := s.request(b)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(hosts) == 0 {
		fail(w, http.StatusBadRequest, "hosts is required")
		return
	}
	req.Source, req.From = "api", s.callerIP(r)
	reply(w, http.StatusAccepted, map[string]any{"runs": s.eng.Submit(hosts, req)})
}

// ---- reading

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.st.ListRuns(r.URL.Query().Get("status"))
	reply(w, http.StatusOK, map[string]any{"count": len(runs), "runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	wait, _ := strconv.ParseFloat(r.URL.Query().Get("wait"), 64)
	deadline := time.Now().Add(time.Duration(min(wait, 300) * float64(time.Second)))
	for {
		run := s.st.GetRun(host)
		if run == nil {
			fail(w, http.StatusNotFound, "no run recorded for host %q", host)
			return
		}
		if isFinal(run.Status) || !time.Now().Before(deadline) {
			reply(w, http.StatusOK, run)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	run := s.st.GetRun(host)
	if run == nil {
		fail(w, http.StatusNotFound, "no run recorded for host %q", host)
		return
	}
	var data []byte
	var err error
	switch {
	case r.URL.Query().Get("raw") != "":
		if run.BatchID == "" {
			err = os.ErrNotExist
		} else {
			data, err = os.ReadFile(s.st.batchLogPath(run.BatchID))
		}
	case run.Status == StRunning:
		data = s.eng.LiveLog(run)
	case run.Status == StQueued:
		data = []byte(fmt.Sprintf("awflex run %s host=%s is queued\n", run.ID, host))
	default:
		data, err = s.st.ReadLog(host)
	}
	if err != nil {
		fail(w, http.StatusNotFound, "no log available for host %q yet", host)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.eng.Cancel(r.PathValue("host"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		fail(w, http.StatusNotFound, "no run recorded for host %q", r.PathValue("host"))
	case err != nil:
		fail(w, http.StatusConflict, "%v", err)
	default:
		reply(w, http.StatusOK, run)
	}
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Forget(r.PathValue("host")); err != nil {
		fail(w, http.StatusNotFound, "no run recorded for host %q", r.PathValue("host"))
		return
	}
	reply(w, http.StatusOK, map[string]string{"forgotten": r.PathValue("host")})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.CancelBatch(r.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, "batch %q is not running", r.PathValue("id"))
		return
	}
	reply(w, http.StatusAccepted, map[string]string{"canceling": r.PathValue("id")})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]any{
		"name": "awflex", "version": Version,
		"auth":     "Authorization: Bearer <token>. admin = AWFLEX_ADMIN_TOKEN, read = AWFLEX_READ_TOKEN or admin, hook = AWFLEX_HOOK_TOKEN if set (open otherwise).",
		"statuses": []string{StQueued, StRunning, StSuccessful, StFailed, StUnreachable, StError, StCanceled, StTimeout},
		"routes":   s.routes,
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.eng.Status())
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	c := s.cfg
	reply(w, http.StatusOK, map[string]any{
		"repo": c.Repo, "branch": c.Ref, "playbook": c.Playbook, "inventory": c.Inventory, "groups": c.Groups,
		"extra_vars": c.ExtraVars, "max_forks": c.MaxForks, "forks": c.Forks, "max_batch": c.MaxBatch,
		"batch_window_seconds": c.BatchWindow.Seconds(), "timeout_seconds": c.Timeout.Seconds(),
		"max_attempts": c.MaxAttempts, "fetch_ttl_seconds": c.FetchTTL.Seconds(),
		"hook_token_required": c.HookToken != "", "credentials_sealed": c.SecretKey != "",
		"data_dir": c.DataDir,
	})
}

// ---- credentials

func credView(c *Credential) map[string]any {
	var set []string
	for name, v := range map[string]string{"ssh_user": c.SSHUser, "ssh_private_key": c.SSHPrivateKey,
		"become_password": c.BecomePassword, "vault_password": c.VaultPassword, "git_user": c.GitUser,
		"git_token": c.GitToken, "git_ssh_key": c.GitSSHKey} {
		if v != "" {
			set = append(set, name)
		}
	}
	sort.Strings(set)
	env := []string{}
	for k := range c.Env {
		env = append(env, k)
	}
	sort.Strings(env)
	v := map[string]any{"name": c.Name, "fields": set, "env": env}
	if c.Managed != "" {
		v["managed"] = c.Managed
	}
	return v
}

func (s *Server) listCreds(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, c := range s.st.ListCredentials() {
		out = append(out, credView(c))
	}
	reply(w, http.StatusOK, map[string]any{"credentials": out})
}

func (s *Server) putCred(w http.ResponseWriter, r *http.Request) {
	var c Credential
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, "invalid credential JSON: %v", err)
		return
	}
	c.Name = r.PathValue("name")
	if err := s.st.PutCredential(&c); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errManaged) {
			code = http.StatusConflict
		}
		fail(w, code, "%v", err)
		return
	}
	reply(w, http.StatusOK, credView(&c))
}

func (s *Server) deleteCred(w http.ResponseWriter, r *http.Request) {
	err := s.st.DeleteCredential(r.PathValue("name"))
	switch {
	case errors.Is(err, errManaged):
		fail(w, http.StatusConflict, "%v", err)
	case err != nil:
		fail(w, http.StatusNotFound, "no credential %q", r.PathValue("name"))
	default:
		reply(w, http.StatusOK, map[string]string{"deleted": r.PathValue("name")})
	}
}

// ---- metrics

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	st := s.eng.Status()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	g := func(name, help string, v int) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}
	g("awflex_queued_hosts", "Hosts waiting for an ansible run.", st.Queued)
	g("awflex_running_hosts", "Hosts in a running ansible process.", st.Running)
	g("awflex_running_batches", "Running ansible processes.", len(st.Batches))
	g("awflex_forks_in_use", "Ansible forks in use.", st.ForksInUse)
	g("awflex_max_forks", "Ansible fork budget.", st.MaxForks)
	fmt.Fprint(w, "# HELP awflex_hosts Hosts by the status of their last run.\n# TYPE awflex_hosts gauge\n")
	for _, k := range []string{StQueued, StRunning, StSuccessful, StFailed, StUnreachable, StError, StCanceled, StTimeout} {
		fmt.Fprintf(w, "awflex_hosts{status=%q} %d\n", k, st.Hosts[k])
	}
	fmt.Fprint(w, "# HELP awflex_runs_finished_total Runs finished since start, by status.\n# TYPE awflex_runs_finished_total counter\n")
	for _, k := range []string{StSuccessful, StFailed, StUnreachable, StError, StCanceled, StTimeout} {
		fmt.Fprintf(w, "awflex_runs_finished_total{status=%q} %d\n", k, st.Finished[k])
	}
	fmt.Fprintf(w, "# HELP awflex_hook_calls_total Node hook calls accepted.\n# TYPE awflex_hook_calls_total counter\nawflex_hook_calls_total %d\n", st.HookCalls)
}

// ---- process

// Serve runs the server until SIGINT or SIGTERM.
func Serve() error {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return err
	}
	st, err := OpenStore(cfg.DataDir, cfg.SecretKey)
	if err != nil {
		return err
	}
	for _, e := range st.LoadExternal(cfg.CredentialsDir) {
		log.Printf("warning: %v", e)
	}
	git, err := NewGit(cfg)
	if err != nil {
		return err
	}
	eng, err := NewEngine(cfg, st, git)
	if err != nil {
		return err
	}
	eng.Start()

	if cfg.NoAuth {
		log.Printf("warning: AWFLEX_INSECURE_NO_AUTH=1, the API is open")
	}
	if cfg.SecretKey == "" {
		log.Printf("warning: AWFLEX_SECRET_KEY is not set, API-managed credentials are stored unencrypted")
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: NewServer(cfg, st, eng), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.CredentialsDir != "" {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					st.LoadExternal(cfg.CredentialsDir)
				}
			}
		}()
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("awflex %s listening on %s, repo=%s branch=%s playbook=%s max_forks=%d window=%s",
			Version, cfg.Listen, cfg.Repo, cfg.Ref, cfg.Playbook, cfg.MaxForks, cfg.BatchWindow)
		if cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		eng.Stop()
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down: unfinished hosts stay queued on disk")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	eng.Stop()
	return nil
}
