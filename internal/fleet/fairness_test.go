package fleet

import (
	"context"
	"testing"
	"time"
)

func TestContinuousBacklogDoesNotStarveOtherQueuesOrScopes(t *testing.T) {
	for _, separateScopes := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-scope", true: "separate-scopes"}[separateScopes], func(t *testing.T) {
			b, w, backend, a := fixture(t)
			b.limits.MaxVMs = 1
			a.QueueID = "a"
			a.Label = "a"
			z := a
			z.QueueID = "z"
			z.Label = "z"
			if separateScopes {
				z.ScopeURL = "https://github.com/z-example"
			}
			workers := map[string]Worker{"worker-a": w}
			backends := map[string]Backend{"a": backend, "z": backend}
			counts := map[string]int{}
			for cycle := 0; cycle < 10; cycle++ {
				a.Revision = uint64(cycle + 1)
				z.Revision = a.Revision
				if err := b.syncAndCleanup(context.Background(), []Demand{z, a}, workers, backends); err != nil {
					t.Fatal(err)
				}
				if len(b.assignments) != cycle+1 {
					t.Fatalf("admission stalled at cycle %d", cycle)
				}
				last := b.assignments[len(b.assignments)-1]
				counts[last.QueueID]++
				r := w.records[last.ID]
				r.State = "terminal"
				r.CompletedAt = time.Now().UTC()
				w.records[last.ID] = r
				w.inventory.Used = CapacityUsed{}
				a.CompletedRunners = nil
				z.CompletedRunners = nil
				if last.QueueID == a.QueueID {
					a.CompletedRunners = []string{last.RunnerName}
				} else {
					z.CompletedRunners = []string{last.RunnerName}
				}
			}
			if counts["a"] != 5 || counts["z"] != 5 {
				t.Fatalf("continuous backlog starved a waiting queue: %v", counts)
			}
		})
	}
}

func TestSkippedQueuesDoNotAdvanceFairnessCursors(t *testing.T) {
	b, w, backend, d := fixture(t)
	incompatible := d
	incompatible.QueueID = "a|incompatible"
	incompatible.Profile.Machine = "microvm"
	paused := d
	paused.QueueID = "z|paused"
	paused.Assigned = 0
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend, incompatible.QueueID: backend, paused.QueueID: backend}
	if err := b.Sync(context.Background(), []Demand{incompatible, paused}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if w.reserve != 0 || b.lastScope != "" || len(b.lastQueues) != 0 {
		t.Fatal("skipped queue advanced fairness or created intent")
	}
	if err := b.Sync(context.Background(), []Demand{incompatible, paused, d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if w.reserve != 1 || b.lastScope != scopeKey(d.ScopeURL) || b.lastQueues[scopeKey(d.ScopeURL)] != d.QueueID {
		t.Fatal("durable admitted queue did not advance fairness")
	}
}

func TestLoweredScopeQuotaRetainsExistingJobsAndBlocksNewAdmission(t *testing.T) {
	b, w, backend, d := fixture(t)
	d.ScopeMax = 2
	d.Assigned = 2
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	for i := 0; i < 2; i++ {
		if err := b.syncAndCleanup(context.Background(), []Demand{d}, workers, backends); err != nil {
			t.Fatal(err)
		}
	}
	d.ScopeMax = 1
	d.Revision++
	if err := b.syncAndCleanup(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if w.reserve != 2 || w.deliver != 2 || backend.remove != 0 || b.scopeCount(d.ScopeURL) != 2 {
		t.Fatal("lowered quota interrupted jobs or admitted more")
	}
	a := b.assignments[0]
	r := w.records[a.ID]
	r.State = "terminal"
	r.CompletedAt = time.Now().UTC()
	w.records[a.ID] = r
	if err := b.syncAndCleanup(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if w.reserve != 2 || b.scopeCount(d.ScopeURL) != 1 {
		t.Fatal("new admission exceeded lowered quota")
	}
}
