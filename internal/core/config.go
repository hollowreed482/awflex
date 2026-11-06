package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of awflex's configuration, all from environment
// variables. There is one playbook repo and one set of run settings; they are
// fixed at deploy time rather than managed through the API.
type Config struct {
	Listen         string
	DataDir        string
	TmpDir         string // per-run secrets and scratch; put on tmpfs in Kubernetes
	CredentialsDir string // optional read-only dir of <name>.json (mounted Secret)
	AdminToken     string
	ReadToken      string
	HookToken      string // optional; empty leaves the node hook open
	SecretKey      string // seals API-managed credentials at rest
	NoAuth         bool

	Repo      string         // git URL of the playbook repo
	Ref       string         // branch used when a caller names none
	Playbook  string         // playbook used when a caller names none
	Inventory []string       // inventories, repo relative or absolute (mounted); empty = generated from the calling hosts
	Groups    []string       // extra groups for hosts in a generated inventory
	ExtraVars map[string]any // applied to every run

	HookExtraVars bool // let a node pass variables; off because a node is the least trusted caller
	SSHAgent      bool // hold the SSH key in a per-run agent, for jump hosts

	MaxForks    int // global ansible fork budget, the memory knob
	Forks       int // forks per ansible run
	BatchWindow time.Duration
	MaxBatch    int
	MaxAttempts int // starts per request before a never-finishing run is given up
	Timeout     time.Duration
	FetchTTL    time.Duration
	AnsibleBin  string
	GitBin      string
	TrustProxy  bool
	TLSCert     string
	TLSKey      string
}

var (
	refRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
)

func validRef(r string) error {
	if !refRe.MatchString(r) || strings.Contains(r, "..") {
		return fmt.Errorf("invalid branch/ref %q", r)
	}
	return nil
}

// cleanRel accepts a repo relative path and rejects anything that could leave
// the checkout or be read as a command line flag.
func cleanRel(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "-") || filepath.IsAbs(p) || strings.ContainsAny(p, "\n\x00") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q leaves the repo", p)
	}
	return c, nil
}

func validWords(kind string, ws []string) error {
	for _, w := range ws {
		if w == "" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, ", \n") {
			return fmt.Errorf("invalid %s %q", kind, w)
		}
	}
	return nil
}

// env reads NAME, or the file named by NAME_FILE (for mounted Secrets).
func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if f := os.Getenv(name + "_FILE"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: want a positive integer, got %q", name, v)
	}
	return n, nil
}

func envDur(name string, def time.Duration) (time.Duration, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s: want seconds or a duration like 10s, got %q", name, v)
	}
	return d, nil
}

func envList(name string) []string {
	var out []string
	for _, p := range strings.Split(env(name, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Listen:         env("AWFLEX_LISTEN", ":8080"),
		DataDir:        env("AWFLEX_DATA_DIR", "/data"),
		TmpDir:         env("AWFLEX_TMP_DIR", ""),
		CredentialsDir: env("AWFLEX_CREDENTIALS_DIR", ""),
		AdminToken:     env("AWFLEX_ADMIN_TOKEN", ""),
		ReadToken:      env("AWFLEX_READ_TOKEN", ""),
		HookToken:      env("AWFLEX_HOOK_TOKEN", ""),
		SecretKey:      env("AWFLEX_SECRET_KEY", ""),
		NoAuth:         env("AWFLEX_INSECURE_NO_AUTH", "") == "1",
		Repo:           env("AWFLEX_REPO", ""),
		Ref:            env("AWFLEX_BRANCH", "main"),
		Playbook:       env("AWFLEX_PLAYBOOK", "main.yml"),
		Inventory:      envList("AWFLEX_INVENTORY"),
		HookExtraVars:  env("AWFLEX_HOOK_EXTRA_VARS", "") == "1",
		SSHAgent:       env("AWFLEX_SSH_AGENT", "1") != "0",
		Groups:         envList("AWFLEX_GROUPS"),
		AnsibleBin:     env("AWFLEX_ANSIBLE_PLAYBOOK", "ansible-playbook"),
		GitBin:         env("AWFLEX_GIT", "git"),
		TrustProxy:     env("AWFLEX_TRUST_PROXY", "") == "1",
		TLSCert:        env("AWFLEX_TLS_CERT", ""),
		TLSKey:         env("AWFLEX_TLS_KEY", ""),
	}
	if v := env("AWFLEX_EXTRA_VARS", ""); v != "" {
		if err := json.Unmarshal([]byte(v), &c.ExtraVars); err != nil {
			return c, fmt.Errorf("AWFLEX_EXTRA_VARS: want a JSON object: %w", err)
		}
	}
	var err error
	if c.MaxForks, err = envInt("AWFLEX_MAX_FORKS", 50); err != nil {
		return c, err
	}
	if c.Forks, err = envInt("AWFLEX_FORKS", 25); err != nil {
		return c, err
	}
	if c.MaxBatch, err = envInt("AWFLEX_MAX_BATCH", 100); err != nil {
		return c, err
	}
	if c.MaxAttempts, err = envInt("AWFLEX_MAX_ATTEMPTS", 3); err != nil {
		return c, err
	}
	if c.BatchWindow, err = envDur("AWFLEX_BATCH_WINDOW", 10*time.Second); err != nil {
		return c, err
	}
	if c.Timeout, err = envDur("AWFLEX_TIMEOUT", 2*time.Hour); err != nil {
		return c, err
	}
	if c.FetchTTL, err = envDur("AWFLEX_FETCH_TTL", 10*time.Second); err != nil {
		return c, err
	}
	return c, c.Normalize()
}

// Normalize validates the config and fills derived defaults.
func (c *Config) Normalize() error {
	if c.Repo == "" || strings.HasPrefix(c.Repo, "-") || strings.ContainsAny(c.Repo, " \n") {
		return fmt.Errorf("AWFLEX_REPO: want the git URL of the playbook repo, got %q", c.Repo)
	}
	if err := validRef(c.Ref); err != nil {
		return fmt.Errorf("AWFLEX_BRANCH: %w", err)
	}
	if _, err := cleanRel(c.Playbook); err != nil {
		return fmt.Errorf("AWFLEX_PLAYBOOK: %w", err)
	}
	for i, p := range c.Inventory {
		if filepath.IsAbs(p) && !strings.ContainsAny(p, "\n\x00") {
			c.Inventory[i] = filepath.Clean(p) // a mounted file or directory
			continue
		}
		rel, err := cleanRel(p)
		if err != nil {
			return fmt.Errorf("AWFLEX_INVENTORY: %w", err)
		}
		c.Inventory[i] = rel
	}
	for _, g := range c.Groups {
		if !hostRe.MatchString(g) {
			return fmt.Errorf("AWFLEX_GROUPS: invalid group %q", g)
		}
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.DataDir = abs
	if c.TmpDir == "" {
		c.TmpDir = filepath.Join(c.DataDir, "tmp")
	}
	if c.AdminToken == "" && !c.NoAuth {
		return fmt.Errorf("AWFLEX_ADMIN_TOKEN (or AWFLEX_ADMIN_TOKEN_FILE) is required; set AWFLEX_INSECURE_NO_AUTH=1 only for local development")
	}
	return nil
}
