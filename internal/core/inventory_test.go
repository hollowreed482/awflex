package core

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInventory stands in for ansible-inventory --list --export: it prints the
// JSON in the "invjson" file beside it, or fails when the mode file says so.
const fakeInventory = `#!/bin/sh
here=$(dirname "$0")
echo call >> "$here/invcalls"
echo "token=$INV_TOKEN" > "$here/invenv"
if [ "$(cat "$here/mode" 2>/dev/null)" = invfail ]; then
  echo "[WARNING]: Unable to parse /repo/dynamic.yml as an inventory source" >&2
  echo "[ERROR]: No inventory was parsed, please check your configuration and options." >&2
  exit 1
fi
cat "$here/invjson"
`

func invJSON(hosts ...string) string {
	var hs []string
	for _, h := range hosts {
		hs = append(hs, `"`+h+`"`)
	}
	return `{"_meta":{"hostvars":{"` + hosts[0] + `":{"ansible_host":"10.0.0.1"}}},
	"all":{"children":["ungrouped","compute"],"vars":{"site":"x"}},
	"compute":{"hosts":[` + strings.Join(hs, ",") + `],"vars":{"role":"compute"}}}`
}

func newInvRig(t *testing.T, tune func(*Config)) *rig {
	t.Helper()
	old := invMissRetry
	invMissRetry = 0
	t.Cleanup(func() { invMissRetry = old })
	r := newRig(t, func(c *Config) {
		c.Inventory, c.InventoryCache = []string{"dynamic.yml"}, time.Hour
		dir := filepath.Dir(c.AnsibleBin)
		os.WriteFile(filepath.Join(dir, "ansible-inventory"), []byte(fakeInventory), 0o755)
		os.WriteFile(filepath.Join(dir, "invjson"), []byte(invJSON("n1")), 0o644)
		if tune != nil {
			tune(c)
		}
	})
	r.eng.ensureInventory(context.Background(), nil) // wait for the warm-up refresh
	return r
}

func (r *rig) invCalls() int { return strings.Count(r.fake("invcalls"), "call") }

func TestExportToInventory(t *testing.T) {
	raw := `{"_meta":{"hostvars":{"n1":{"ansible_host":"10.0.0.1","big":12345678901234567890,
	  "pw":{"__ansible_vault":"$ANSIBLE_VAULT;1.1;AES256\n6162"},"raw":{"__ansible_unsafe":"{{ x }}"}}}},
	  "all":{"children":["ungrouped","compute"],"vars":{"site":"x"}},
	  "compute":{"hosts":["n1","n2"],"children":["gpu"],"vars":{"role":"compute"}},
	  "gpu":{"hosts":["n2"]},"ungrouped":{"hosts":["n3"]}}`
	doc, hosts, err := exportToInventory([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hosts, " ") != "n1 n2 n3" {
		t.Fatalf("hosts: %v", hosts)
	}
	for _, want := range []string{
		`"all":{"children":{"compute":{"children":{"gpu":{"hosts":{"n2":null}}},"hosts":{"n1":null,"n2":null},"vars":{"role":"compute"}}}`,
		`"n2":null,"n3":null`,
		`"big":12345678901234567890`,
		`"pw":!vault "$ANSIBLE_VAULT;1.1;AES256\n6162"`,
		`"raw":!unsafe "{{ x }}"`,
		`"vars":{"site":"x"}`,
	} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("missing %s in\n%s", want, doc)
		}
	}
	if strings.Contains(string(doc), "ungrouped") {
		t.Errorf("ungrouped is implicit and should not be written:\n%s", doc)
	}
}

func TestInventoryCacheServesRunsAndRefreshesForNewHosts(t *testing.T) {
	r := newInvRig(t, nil)
	if c := r.invCalls(); c != 1 {
		t.Fatalf("warm-up should query the sources once, got %d", c)
	}
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	if run := r.waitFinal("n1"); run.Status != StSuccessful {
		t.Fatalf("%+v", run)
	}
	if c := r.invCalls(); c != 1 {
		t.Fatalf("a fresh cache must not query the sources again, got %d calls", c)
	}
	// A node added to the source after the last refresh is found on its first call.
	os.WriteFile(filepath.Join(filepath.Dir(r.cfg.AnsibleBin), "invjson"), []byte(invJSON("n1", "n2")), 0o644)
	r.eng.Submit([]string{"n2"}, Request{Ref: "main", Playbook: "main.yml"})
	if run := r.waitFinal("n2"); run.Status != StSuccessful {
		t.Fatalf("%+v", run)
	}
	if c := r.invCalls(); c != 2 {
		t.Fatalf("a host missing from the cache should refresh it once, got %d calls", c)
	}
	if info := r.eng.InventoryInfo(true); info.Hosts != 2 || strings.Join(info.HostNames, ",") != "n1,n2" || info.LastError != "" {
		t.Fatalf("%+v", info)
	}
	cache, _ := os.ReadFile(r.eng.invPath())
	if !strings.Contains(string(cache), `"ansible_host":"10.0.0.1"`) {
		t.Fatalf("host vars should be in the cache:\n%s", cache)
	}
}

func TestInventoryEnvReachesOnlyTheRefresh(t *testing.T) {
	r := newInvRig(t, nil)
	if err := r.st.PutCredential(&Credential{Name: "inventory", InventoryEnv: map[string]string{"INV_TOKEN": "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.eng.RefreshInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(r.fake("invenv")); got != "token=s3cret" {
		t.Fatalf("the refresh should see the token, got %q", got)
	}
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	r.waitFinal("n1")
	if got := strings.TrimSpace(r.fake("pbenv")); got != "token=" {
		t.Fatalf("a playbook must never see inventory_env, got %q", got)
	}
}

func TestInventoryRefreshFailureKeepsTheLastCache(t *testing.T) {
	r := newInvRig(t, nil)
	r.mode("invfail")
	if err := r.eng.RefreshInventory(context.Background()); err == nil {
		t.Fatal("refresh should fail")
	}
	info := r.eng.InventoryInfo(false)
	if info.Hosts != 1 || !strings.Contains(info.LastError, "No inventory was parsed") || !strings.Contains(info.Hint, "inventory-env-file") {
		t.Fatalf("%+v", info)
	}
	// ansible-playbook still has to work, so give the fake its normal mode back
	// after the failed refresh; the run must use the kept cache.
	r.mode("")
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	if run := r.waitFinal("n1"); run.Status != StSuccessful {
		t.Fatalf("%+v", run)
	}
}

func TestNoInventoryCacheAtAllIsARunError(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.Inventory, c.InventoryCache = []string{"dynamic.yml"}, time.Hour
		dir := filepath.Dir(c.AnsibleBin)
		os.WriteFile(filepath.Join(dir, "ansible-inventory"), []byte(fakeInventory), 0o755)
		os.WriteFile(filepath.Join(dir, "mode"), []byte("invfail"), 0o644)
	})
	r.eng.Submit([]string{"n1"}, Request{Ref: "main", Playbook: "main.yml"})
	run := r.waitFinal("n1")
	if run.Status != StError || !strings.Contains(run.Error, "inventory:") || !strings.Contains(run.Hint, "awflex inventory refresh") {
		t.Fatalf("%+v", run)
	}
}

func TestInventoryCacheSurvivesRestart(t *testing.T) {
	r := newInvRig(t, nil)
	calls := r.invCalls()
	r.eng.Stop()
	r.open()
	r.eng.ensureInventory(context.Background(), nil)
	if r.invCalls() != calls {
		t.Fatal("a fresh cache from the last process should be reused, not refetched")
	}
	if info := r.eng.InventoryInfo(false); info.Hosts != 1 {
		t.Fatalf("%+v", info)
	}
}

func TestInventoryCacheNeedsInventory(t *testing.T) {
	c := Config{DataDir: t.TempDir(), Repo: "x", Ref: "main", Playbook: "main.yml", AdminToken: "a", InventoryCache: time.Minute}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "AWFLEX_INVENTORY") {
		t.Fatalf("want an error naming AWFLEX_INVENTORY, got %v", err)
	}
}

func TestInventoryRoutes(t *testing.T) {
	r := newInvRig(t, nil)
	srv := NewServer(r.cfg, r.st, r.eng)
	for _, c := range []struct {
		method, path, token string
		want                int
		body                string
	}{
		{"GET", "/api/v1/inventory?hosts=1", "read", 200, `"host_names":["n1"]`},
		{"POST", "/api/v1/inventory/refresh", "read", 401, ""},
		{"POST", "/api/v1/inventory/refresh", "admin", 200, `"hosts":1`},
		{"GET", "/api/v1/status", "read", 200, `"inventory":{"enabled":true`},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.body) {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

func TestDoctorReportsInventoryCache(t *testing.T) {
	r := newInvRig(t, nil)
	r.commit("dynamic.yml", "plugin: example.inventory\n")
	rep := r.eng.Doctor(context.Background(), "n1")
	if c := check(t, rep, "inventory_cache"); c.Status != "ok" || !strings.Contains(c.Detail, "1 hosts") {
		t.Fatalf("%+v", c)
	}
	if c := check(t, rep, "connect"); c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
}

func TestMissingHostWaitsForTheNextAllowedRefresh(t *testing.T) {
	r := newInvRig(t, nil)
	r.eng.inv.mu.Lock()
	r.eng.inv.retry = 600 * time.Millisecond // a refresh just happened at warm-up
	r.eng.inv.mu.Unlock()
	os.WriteFile(filepath.Join(filepath.Dir(r.cfg.AnsibleBin), "invjson"), []byte(invJSON("n1", "n3")), 0o644)
	start := time.Now()
	if err := r.eng.ensureInventory(context.Background(), []string{"n3"}); err != nil {
		t.Fatal(err)
	}
	if r.invCalls() != 2 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("should have waited for the retry window, then refreshed: calls=%d after %s", r.invCalls(), time.Since(start))
	}
	if r.eng.InventoryInfo(false).Hosts != 2 {
		t.Fatal("n3 should be in the cache")
	}
	// A host the source does not have: one wait, one refresh, then give up.
	done := make(chan struct{})
	go func() { r.eng.ensureInventory(context.Background(), []string{"nope"}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a host the source does not have must not loop")
	}
	if r.invCalls() != 3 {
		t.Fatalf("want exactly one more refresh for an unknown host, got %d calls", r.invCalls())
	}
}
