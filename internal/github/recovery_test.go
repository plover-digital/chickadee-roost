package github

import (
	"context"
	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryRetainsArchivedIdentityWithoutScopeLookup(t *testing.T) {
	key := filepath.Join(t.TempDir(), "app.pem")
	// Constructor does not authenticate until an API operation is made. A fake key
	// and canceled context prove recovery cannot depend on GitHub availability.
	if err := os.WriteFile(key, []byte("not an actual credential"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := dispatch.Queue{GitHubURL: "https://github.com/example/revoked", KeyFile: key, ClientID: "Iv1.example", InstallationID: 1, RunnerGroupID: 2, ScaleSet: "chickadee"}
	c, err := Recover(ctx, q, 73)
	if err != nil || c.SetID != 73 || c.GroupID != 2 || c.SetName != "chickadee" || c.API == nil || c.newRetirementAPI == nil || c.newJITAPI == nil || c.statisticsAPI == nil || c.statisticsAPI == c.API {
		t.Fatalf("recovery lost safe identity: %v", err)
	}
	first, err := c.newRetirementAPI()
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.newRetirementAPI()
	if err != nil {
		t.Fatal(err)
	}
	if first == c.API || second == c.API || first == second {
		t.Fatal("retirement shares a context-unaware SDK mutex")
	}
	firstJIT, err := c.newJITAPI()
	if err != nil {
		t.Fatal(err)
	}
	secondJIT, err := c.newJITAPI()
	if err != nil {
		t.Fatal(err)
	}
	if firstJIT == c.API || secondJIT == c.API || firstJIT == secondJIT {
		t.Fatal("JIT shares a context-unaware SDK mutex")
	}
	if _, err = Recover(ctx, q, 0); err == nil {
		t.Fatal("missing archived identity accepted")
	}
	if err = os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(ctx, q, 73); err == nil {
		t.Fatal("unsafe local key permissions ignored")
	}
}
