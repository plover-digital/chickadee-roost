package site

import (
	"bytes"
	"github.com/plover-digital/chickadee/workerapi"
	"strings"
	"testing"
	"time"
)

func TestResourceDashboardACLAndBounds(t *testing.T) {
	now := time.Now().UTC()
	account := Account{ID: 8, Type: "Organization"}
	run := ResourceRun{ID: "run", Label: "chickadee", CompletedAt: now, CPUs: 2, MemoryMiB: 4096, Summary: workerapi.ResourceSummary{Version: 1, Samples: 3, DurationMillis: 2000, CPUUsec: 2000000, MemoryLimitBytes: 512 << 20, PeakMemoryBytes: 128 << 20}}
	a := AccountUsage{ObservedAt: now, InstallationID: 9, AccountID: 8, AccountType: "Organization", RepositoryIDs: []int64{10, 11}, Resources: []ResourceRun{run}}
	p := page{Title: "Your repositories", User: &User{ID: 7}, AccountUsage: []AccountUsage{a}, Choices: []Choice{{Installation: Installation{ID: 9, Account: account}, Repository: Repository{ID: 10}}}}
	if len(p.ResourceRuns()) != 0 {
		t.Fatal("partial org role leaked metrics")
	}
	p.Choices = append(p.Choices, Choice{Installation: Installation{ID: 9, Account: account}, Repository: Repository{ID: 11}})
	if len(p.ResourceRuns()) != 1 || run.CPUAverage() != "50.0%" {
		t.Fatal("authorized metrics absent")
	}
	s := fixture(t)
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, "page.html", p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "50.0%") || !strings.Contains(b.String(), "128 MiB") || !strings.Contains(b.String(), "Unavailable: a single accounting device") {
		t.Fatal("measurement labels missing")
	}
	p.AccountUsage[0].ObservedAt = now.Add(-6 * time.Minute)
	if len(p.ResourceRuns()) != 0 {
		t.Fatal("stale ACL leaked")
	}
	if !validResourceRuns([]ResourceRun{run}, now) || validResourceRuns([]ResourceRun{run, run}, now) {
		t.Fatal("deduplication bounds failed")
	}
	run.Summary.Version = 2
	if validResourceRuns([]ResourceRun{run}, now) {
		t.Fatal("unknown resource version accepted")
	}
}
