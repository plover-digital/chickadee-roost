package fleet

import (
	"context"
	"testing"
)

type prioritizedWorker struct {
	*fakeWorker
	priority int
}

func (w prioritizedWorker) PlacementPriority() int { return w.priority }

func TestPlacementPriorityPreservesEligibilityAndWarmFirst(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		priority                                    int
		cold, offline, draining, incompatible, full bool
		want                                        string
	}{
		{name: "higher-ready", priority: 10, want: "worker-z"},
		{name: "equal-stable-id", want: "worker-a"},
		{name: "lower-ready", priority: -10, want: "worker-a"},
		{name: "ready-before-priority-cold", priority: 100, cold: true, want: "worker-a"},
		{name: "offline", priority: 100, offline: true, want: "worker-a"},
		{name: "draining", priority: 100, draining: true, want: "worker-a"},
		{name: "incompatible", priority: 100, incompatible: true, want: "worker-a"},
		{name: "cold-full", priority: 100, cold: true, full: true, want: "worker-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, a, backend, d := fixture(t)
			z := *a
			z.records = map[string]Record{}
			z.inventory = a.inventory
			z.inventory.Identity.WorkerID = "worker-z"
			z.inventory.Profiles = append([]ProfileInventory(nil), a.inventory.Profiles...)
			z.unavailable = tc.offline
			z.inventory.Draining = tc.draining
			if tc.cold {
				z.inventory.Profiles[0].Ready = 0
				z.inventory.Used = CapacityUsed{}
			}
			if tc.incompatible {
				z.inventory.Profiles[0].Digest = "different-image"
			}
			if tc.full {
				z.inventory.Used = CapacityUsed{3, 12, 24576}
			}
			workers := map[string]Worker{"worker-a": a, "worker-z": prioritizedWorker{&z, tc.priority}}
			if err := b.Sync(context.Background(), []Demand{d}, workers, map[string]Backend{d.QueueID: backend}); err != nil {
				t.Fatal(err)
			}
			assignments := b.Assignments()
			if len(assignments) != 1 || assignments[0].Worker.WorkerID != tc.want {
				t.Fatalf("assignments: %+v", assignments)
			}
			// Priority cannot bypass the global scope quota across another label.
			d.QueueID = "second-label"
			d.Label = "second-label"
			if err := b.Sync(context.Background(), []Demand{d}, workers, map[string]Backend{d.QueueID: backend}); err != nil {
				t.Fatal(err)
			}
			if len(b.Assignments()) != 1 {
				t.Fatal("priority bypassed scope quota")
			}
		})
	}
}

func TestColdPlacementPriority(t *testing.T) {
	b, a, backend, d := fixture(t)
	a.inventory.Profiles[0].Ready = 0
	a.inventory.Used = CapacityUsed{}
	z := *a
	z.records = map[string]Record{}
	z.inventory = a.inventory
	z.inventory.Identity.WorkerID = "worker-z"
	z.inventory.Profiles = append([]ProfileInventory(nil), a.inventory.Profiles...)
	if err := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": a, "worker-z": prioritizedWorker{&z, 10}}, map[string]Backend{d.QueueID: backend}); err != nil {
		t.Fatal(err)
	}
	if b.Assignments()[0].Worker.WorkerID != "worker-z" {
		t.Fatal("cold host priority ignored")
	}
}
