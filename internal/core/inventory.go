package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// With AWFLEX_INVENTORY_CACHE set, awflex reads the configured inventory
// sources (often a dynamic inventory plugin that queries another system) with
// ansible-inventory, and runs use the saved result. The sources are queried at
// most once per TTL, again when a caller's host is not in the cache (so a new
// node is not invisible until the TTL passes), and on demand. A failed refresh
// keeps the last good cache.

var invMissRetry = 30 * time.Second // the soonest a missing host can trigger another refresh

type invState struct {
	refresh     sync.Mutex    // one refresh at a time; callers wait for it
	retry       time.Duration // copy of invMissRetry, fixed when the engine starts
	mu          sync.Mutex    // the fields below; held only briefly
	hosts       map[string]bool
	sources     string
	refreshedAt time.Time
	lastTry     time.Time
	lastErr     string
	warnings    []string
}

// InventoryInfo is what GET /api/v1/inventory returns.
type InventoryInfo struct {
	Enabled     bool       `json:"enabled"`
	Sources     []string   `json:"sources,omitempty"`
	TTL         string     `json:"ttl,omitempty"`
	Hosts       int        `json:"hosts"`
	RefreshedAt *time.Time `json:"refreshed_at,omitempty"`
	AgeSeconds  int        `json:"age_seconds,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Warnings    []string   `json:"warnings,omitempty"`
	Hint        string     `json:"hint,omitempty"`
	HostNames   []string   `json:"host_names,omitempty"`
}

type invMeta struct {
	Sources     string    `json:"sources"`
	RefreshedAt time.Time `json:"refreshed_at"`
	Hosts       []string  `json:"hosts"`
	Warnings    []string  `json:"warnings,omitempty"`
}

func (e *Engine) invDir() string     { return filepath.Join(e.cfg.DataDir, "inventory") }
func (e *Engine) invPath() string    { return filepath.Join(e.invDir(), "cache.yml") }
func (e *Engine) invSources() string { return strings.Join(e.cfg.Inventory, ",") }

// loadInventoryCache picks up the cache a previous process saved, if it was
// made from the same sources.
func (e *Engine) loadInventoryCache() {
	b, err := os.ReadFile(filepath.Join(e.invDir(), "meta.json"))
	if err != nil {
		return
	}
	var m invMeta
	if json.Unmarshal(b, &m) != nil || m.Sources != e.invSources() {
		return
	}
	if _, err := os.Stat(e.invPath()); err != nil {
		return
	}
	s := e.inv
	s.hosts = map[string]bool{}
	for _, h := range m.Hosts {
		s.hosts[h] = true
	}
	s.sources, s.refreshedAt, s.warnings = m.Sources, m.RefreshedAt, m.Warnings
}

// ensureInventory makes sure the cache is usable for hosts. It returns an
// error only when there is no cache at all to run with. A host missing from
// the cache waits for the next allowed refresh rather than failing, so a node
// added to the source just after a refresh is still found.
func (e *Engine) ensureInventory(ctx context.Context, hosts []string) error {
	s := e.inv
	waited := false
	for {
		s.refresh.Lock()
		s.mu.Lock()
		have := s.hosts != nil
		stale := !have || time.Since(s.refreshedAt) > e.cfg.InventoryCache
		missing := false
		for _, h := range hosts {
			if have && !s.hosts[h] {
				missing = true
				break
			}
		}
		wait := time.Until(s.lastTry.Add(s.retry))
		s.mu.Unlock()
		retryOK := wait <= 0
		if (stale && (!have || retryOK)) || (missing && retryOK) {
			if err := e.refreshInventoryLocked(ctx); err != nil {
				log.Printf("inventory refresh failed: %v", err)
			}
		} else if missing && !waited {
			s.refresh.Unlock()
			waited = true
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		s.refresh.Unlock()
		break
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosts == nil {
		if s.lastErr != "" {
			return fmt.Errorf("no inventory cache yet, and refreshing it failed: %s", s.lastErr)
		}
		return errors.New("no inventory cache yet")
	}
	return nil
}

// RefreshInventory queries the sources now, whatever the cache's age.
func (e *Engine) RefreshInventory(ctx context.Context) error {
	e.inv.refresh.Lock()
	defer e.inv.refresh.Unlock()
	return e.refreshInventoryLocked(ctx)
}

// refreshInventoryLocked needs inv.refresh held, not inv.mu.
func (e *Engine) refreshInventoryLocked(ctx context.Context) (err error) {
	s := e.inv
	s.mu.Lock()
	s.lastTry = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if err != nil {
			s.lastErr = err.Error()
		} else {
			s.lastErr = ""
		}
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	tmp, err := os.MkdirTemp(e.cfg.TmpDir, "inventory-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	write := func(name, data string) string {
		p := filepath.Join(tmp, name)
		os.WriteFile(p, []byte(data), 0o600)
		return p
	}
	cred := mergeCreds(e.st.ListCredentials())
	sha, dir, release, err := e.git.Checkout(ctx, e.cfg.Ref, e.gitAuth(cred, write))
	if err != nil {
		return err
	}
	defer release()

	args := []string{"--list", "--export"}
	for _, inv := range e.cfg.Inventory {
		args = append(args, "-i", inv)
	}
	if cred.VaultPassword != "" {
		args = append(args, "--vault-password-file", write("vault_pass", cred.VaultPassword))
	}
	env, _ := ansibleEnv(cred)
	for k, v := range cred.InventoryEnv { // only this process sees these
		env = append(env, k+"="+v)
	}
	cmd := exec.CommandContext(ctx, e.inventoryBin(), args...)
	cmd.Dir, cmd.Env = dir, env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible-inventory at %s (%s): %v: %s", e.cfg.Ref, sha[:12], err, strings.TrimSpace(tail(errb.Bytes(), 1500)))
	}
	doc, hosts, err := exportToInventory(out.Bytes())
	if err != nil {
		return fmt.Errorf("ansible-inventory output: %v", err)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("the inventory sources returned no hosts; the last cache is kept: %s", strings.TrimSpace(tail(errb.Bytes(), 1500)))
	}
	var warnings []string
	for _, l := range strings.Split(errb.String(), "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, "WARNING") || strings.Contains(l, "ERROR") {
			warnings = append(warnings, l)
			if len(warnings) == 20 {
				break
			}
		}
	}
	if err := os.MkdirAll(e.invDir(), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(e.invPath(), doc, 0o600); err != nil {
		return err
	}
	meta, _ := json.Marshal(invMeta{Sources: e.invSources(), RefreshedAt: time.Now().UTC(), Hosts: hosts, Warnings: warnings})
	if err := writeAtomic(filepath.Join(e.invDir(), "meta.json"), meta, 0o600); err != nil {
		return err
	}
	set := map[string]bool{}
	for _, h := range hosts {
		set[h] = true
	}
	s.mu.Lock()
	s.hosts, s.sources, s.refreshedAt, s.warnings = set, e.invSources(), time.Now(), warnings
	s.mu.Unlock()
	log.Printf("inventory refreshed: %d hosts from %s at %s", len(hosts), e.invSources(), sha[:12])
	return nil
}

// inventoryBin is ansible-inventory beside the configured ansible-playbook.
func (e *Engine) inventoryBin() string {
	if p, err := exec.LookPath(e.cfg.AnsibleBin); err == nil {
		if c := filepath.Join(filepath.Dir(p), "ansible-inventory"); fileExists(c) {
			return c
		}
	}
	return "ansible-inventory"
}

func (e *Engine) InventoryInfo(withHosts bool) InventoryInfo {
	if e.inv == nil {
		return InventoryInfo{Hint: "the inventory is read on every run. Set AWFLEX_INVENTORY_CACHE (for example 10m) to cache a dynamic inventory between runs."}
	}
	s := e.inv
	s.mu.Lock()
	defer s.mu.Unlock()
	info := InventoryInfo{Enabled: true, Sources: e.cfg.Inventory, TTL: e.cfg.InventoryCache.String(),
		Hosts: len(s.hosts), LastError: s.lastErr, Warnings: s.warnings}
	if !s.refreshedAt.IsZero() {
		t := s.refreshedAt.UTC()
		info.RefreshedAt, info.AgeSeconds = &t, int(time.Since(s.refreshedAt).Seconds())
	}
	if s.lastErr != "" {
		info.Hint = hintFor(s.lastErr)
	}
	if withHosts {
		for h := range s.hosts {
			info.HostNames = append(info.HostNames, h)
		}
		sort.Strings(info.HostNames)
	}
	return info
}

// exportToInventory turns `ansible-inventory --list --export` output (the JSON
// script format) into a static YAML inventory and lists its hosts. Group vars
// stay on their groups and host vars on their hosts, as in the source.
func exportToInventory(raw []byte) ([]byte, []string, error) {
	var in map[string]json.RawMessage
	if err := decodeExact(raw, &in); err != nil {
		return nil, nil, err
	}
	type group struct {
		Hosts    []string       `json:"hosts"`
		Children []string       `json:"children"`
		Vars     map[string]any `json:"vars"`
	}
	var meta struct {
		Hostvars map[string]map[string]any `json:"hostvars"`
	}
	if m, ok := in["_meta"]; ok {
		if err := decodeExact(m, &meta); err != nil {
			return nil, nil, fmt.Errorf("_meta: %v", err)
		}
	}
	groups := map[string]group{}
	for name, v := range in {
		if name == "_meta" {
			continue
		}
		var g group
		if err := decodeExact(v, &g); err != nil {
			return nil, nil, fmt.Errorf("group %s: %v", name, err)
		}
		groups[name] = g
	}
	hostSet := map[string]bool{}
	for h := range meta.Hostvars {
		hostSet[h] = true
	}
	for _, g := range groups {
		for _, h := range g.Hosts {
			hostSet[h] = true
		}
	}
	var hosts []string
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	allHosts := map[string]any{}
	for _, h := range hosts {
		var hv any
		if v := meta.Hostvars[h]; len(v) > 0 {
			hv = v
		}
		allHosts[h] = hv
	}
	all := map[string]any{"hosts": allHosts}
	if g, ok := groups["all"]; ok && len(g.Vars) > 0 {
		all["vars"] = g.Vars
	}
	// Nest groups as the source does: each group is written once, inside its
	// first parent, and named (with no body) under any other parent.
	defined := map[string]bool{"all": true, "ungrouped": true}
	var def func(name string) map[string]any
	def = func(name string) map[string]any {
		g := groups[name]
		d := map[string]any{}
		if len(g.Hosts) > 0 {
			hs := map[string]any{}
			for _, h := range g.Hosts {
				hs[h] = nil
			}
			d["hosts"] = hs
		}
		if len(g.Vars) > 0 {
			d["vars"] = g.Vars
		}
		if len(g.Children) > 0 {
			cs := map[string]any{}
			for _, c := range g.Children {
				if defined[c] {
					cs[c] = nil
					continue
				}
				defined[c] = true
				cs[c] = def(c)
			}
			d["children"] = cs
		}
		return d
	}
	children := map[string]any{}
	top := groups["all"].Children
	var rest []string
	for name := range groups {
		rest = append(rest, name) // groups no parent named, written under all
	}
	sort.Strings(rest)
	for _, name := range append(append([]string{}, top...), rest...) {
		if defined[name] {
			continue
		}
		defined[name] = true
		children[name] = def(name)
	}
	if len(children) > 0 {
		all["children"] = children
	}
	var b bytes.Buffer
	b.WriteString("# written by awflex from ansible-inventory --list --export; do not edit\n")
	writeYAMLFlow(&b, map[string]any{"all": all})
	b.WriteByte('\n')
	return b.Bytes(), hosts, nil
}

// decodeExact keeps numbers exactly as ansible wrote them.
func decodeExact(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// writeYAMLFlow writes v as JSON, which YAML reads, except that the markers
// ansible's JSON output uses for vaulted and unsafe values become the YAML
// tags ansible reads them from.
func writeYAMLFlow(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			for _, tag := range []string{"__ansible_vault", "__ansible_unsafe"} {
				if s, ok := t[tag].(string); ok {
					b.WriteString("!" + strings.TrimPrefix(tag, "__ansible_") + " ")
					js, _ := json.Marshal(s)
					b.Write(js)
					return
				}
			}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			js, _ := json.Marshal(k)
			b.Write(js)
			b.WriteByte(':')
			writeYAMLFlow(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeYAMLFlow(b, x)
		}
		b.WriteByte(']')
	default:
		js, _ := json.Marshal(t)
		b.Write(js)
	}
}
