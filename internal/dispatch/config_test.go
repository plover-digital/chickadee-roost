package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	raw := `{"app_client_id":"Iv1.example","app_key_file":"/etc/example/app.pem","state_dir":"/ignored/host/path","images":{"u26":{"disk_gib":48,"machine":"q35","path":"/ignored/image"}},"resource_classes":{"medium":{"cpus":4,"memory_mib":8192}},"scopes":{"a":{"github_url":"https://github.com/example/repo","app_installation_id":1,"runner_group_id":2,"max_vms":1,"profiles":{"chickadee":{"image":"u26","resources":"medium","max_vms":2}}}}}`
	var c map[string]any
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(c)
	}
	b, _ := json.Marshal(c)
	p := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestCatalogPreservesScopeQuotaAndDigest(t *testing.T) {
	q, hash, e := LoadCatalog(fixture(t, nil), map[string]string{"u26": strings.Repeat("a", 64)})
	if e != nil {
		t.Fatal(e)
	}
	if len(q) != 1 || q[0].Max != 1 || q[0].ScopeMax != 1 || q[0].CPUs != 4 || len(hash) != 64 {
		t.Fatalf("invalid resolution %#v", q)
	}
}
func TestCatalogRejectsDuplicateTenantQueueAndMissingDigest(t *testing.T) {
	p := fixture(t, func(c map[string]any) { s := c["scopes"].(map[string]any); s["b"] = s["a"] })
	if _, _, e := LoadCatalog(p, map[string]string{"u26": strings.Repeat("a", 64)}); e == nil {
		t.Fatal("duplicate queue accepted")
	}
	if _, _, e := LoadCatalog(fixture(t, nil), nil); e == nil {
		t.Fatal("unverified image admitted")
	}
}
func TestDisconnectedScopeCreatesNoListener(t *testing.T) {
	p := fixture(t, func(c map[string]any) { c["scopes"].(map[string]any)["a"].(map[string]any)["disabled"] = true })
	q, _, e := LoadCatalog(p, nil)
	if e != nil || len(q) != 0 {
		t.Fatalf("disconnected scope admitted: %v %v", q, e)
	}
}
