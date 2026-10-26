package fleet

import (
	"context"
	"testing"
	"time"
)

func TestWithdrawnIntentEstablishesCredentialFreeExpiry(t *testing.T) {
	for _, withdrawal := range []string{"paused", "removed", "profile-changed"} {
		t.Run(withdrawal, func(t *testing.T) {
			b, w, backend, d := fixture(t)
			a := Assignment{ID: "pending", RunnerName: "chickadee-pending", QueueID: d.QueueID, ScopeURL: d.ScopeURL, Label: d.Label, Profile: d.Profile, Worker: w.inventory.Identity, WorkerProfileID: d.Profile.ID, Phase: Intent, DemandRevision: 1, LastDemandRevision: 1, ReservedAt: time.Now().UTC()}
			b.assignments = []Assignment{a}
			if err := b.persist(); err != nil {
				t.Fatal(err)
			}
			d.Revision = 2
			switch withdrawal {
			case "paused":
				d.Assigned = 0
			case "profile-changed":
				d.Profile.Digest = "replacement-image"
			}
			ds := []Demand{d}
			if withdrawal == "removed" {
				ds = nil
			}
			workers := map[string]Worker{"worker-a": w}
			backends := map[string]Backend{d.QueueID: backend}
			if err := b.Sync(context.Background(), ds, workers, backends); err != nil {
				t.Fatal(err)
			}
			if w.reserve != 1 || b.assignments[0].Phase != Reserved || w.seal != 0 || backend.jit != 0 || w.deliver != 0 {
				t.Fatalf("withdrawn intent did not acquire safe expiry: reserve=%d phase=%s seal=%d jit=%d deliver=%d", w.reserve, b.assignments[0].Phase, w.seal, backend.jit, w.deliver)
			}
			if b.scopeCount(a.ScopeURL) != 1 {
				t.Fatal("quota released before terminal cleanup")
			}
			// Reopening must preserve the reservation without another reserve or credentials.
			dir := b.dir
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, b.id, b.limits)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.Sync(context.Background(), ds, workers, backends); err != nil {
				t.Fatal(err)
			}
			if w.reserve != 1 || w.seal != 0 || backend.jit != 0 || w.deliver != 0 {
				t.Fatal("restart replayed credentials or reserve")
			}
			r := w.records[a.ID]
			r.State = "terminal"
			r.CompletedAt = time.Now().UTC()
			w.records[a.ID] = r
			if err := reopened.syncAndCleanup(context.Background(), ds, workers, backends); err != nil {
				t.Fatal(err)
			}
			if reopened.assignments[0].Phase != Complete || reopened.scopeCount(a.ScopeURL) != 0 {
				t.Fatal("terminal canceled reservation retained quota")
			}
		})
	}
}

type unavailableAbandonedReserve struct {
	*fakeWorker
}

func (w unavailableAbandonedReserve) Reserve(context.Context, string, string, string) (Record, error) {
	return Record{}, ErrNotFound
}

func TestWithdrawnIntentCannotReleaseUncertainCapacity(t *testing.T) {
	for _, problem := range []string{"offline", "generation-changed", "reserve-failed"} {
		t.Run(problem, func(t *testing.T) {
			b, w, backend, d := fixture(t)
			a := Assignment{ID: "pending", RunnerName: "chickadee-pending", QueueID: d.QueueID, ScopeURL: d.ScopeURL, Label: d.Label, Profile: d.Profile, Worker: w.inventory.Identity, WorkerProfileID: d.Profile.ID, Phase: Intent, DemandRevision: 1, LastDemandRevision: 1, ReservedAt: time.Now().UTC()}
			b.assignments = []Assignment{a}
			if err := b.persist(); err != nil {
				t.Fatal(err)
			}
			var worker Worker = w
			switch problem {
			case "offline":
				w.unavailable = true
			case "generation-changed":
				w.inventory.Identity.Generation++
			case "reserve-failed":
				worker = unavailableAbandonedReserve{w}
			}
			if err := b.Sync(context.Background(), nil, map[string]Worker{"worker-a": worker}, map[string]Backend{d.QueueID: backend}); err != nil {
				t.Fatal(err)
			}
			if b.assignments[0].Phase != Intent || b.scopeCount(d.ScopeURL) != 1 || w.seal != 0 || backend.jit != 0 || w.deliver != 0 {
				t.Fatal("uncertain reservation released quota or acquired credentials")
			}
		})
	}
}

func TestPendingReservationIntentChargesWorkerPlacementCapacity(t *testing.T) {
	b, w, backend, d := fixture(t)
	b.limits.MaxVMs = 2
	w.inventory.Budget = Budget{1, 4, 8192}
	cold := *w
	cold.inventory = w.inventory
	cold.inventory.Identity.WorkerID = "worker-b"
	cold.inventory.Profiles = append([]ProfileInventory(nil), w.inventory.Profiles...)
	cold.inventory.Profiles[0].Ready = 0
	cold.inventory.Used = CapacityUsed{}
	cold.records = map[string]Record{}
	d.QueueID = "a"
	d.ScopeURL = "https://github.com/a"
	other := d
	other.QueueID = "b"
	other.ScopeURL = "https://github.com/b"
	workers := map[string]Worker{"worker-a": unavailableAbandonedReserve{w}, "worker-b": &cold}
	backends := map[string]Backend{d.QueueID: backend, other.QueueID: backend}
	if err := b.Sync(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if len(b.assignments) != 2 {
		t.Fatalf("assignments=%d", len(b.assignments))
	}
	if b.assignments[0].Worker.WorkerID == b.assignments[1].Worker.WorkerID {
		t.Fatal("unacknowledged intent reused the same physical capacity while second worker stayed free")
	}
}

func TestRestartWithUnobservedAssignmentUsesOtherWorker(t *testing.T) {
	for _, phase := range []string{Intent, Reserved, SealIntent, JITIntent, Delivered, Uncertain, Terminal} {
		t.Run(phase, func(t *testing.T) {
			b, w, backend, d := fixture(t)
			b.limits.MaxVMs = 2
			a := Assignment{ID: "pending", RunnerName: "chickadee-pending", QueueID: d.QueueID, ScopeURL: d.ScopeURL, Label: d.Label, Profile: d.Profile, Worker: w.inventory.Identity, WorkerProfileID: d.Profile.ID, Phase: phase, DemandRevision: 1, LastDemandRevision: 1, ReservedAt: time.Now().UTC()}
			if phase == Terminal {
				a.CompletedAt = time.Now().UTC()
			}
			b.assignments = []Assignment{a}
			if err := b.persist(); err != nil {
				t.Fatal(err)
			}
			dir := b.dir
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, b.id, b.limits)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			cold := *w
			cold.inventory = w.inventory
			cold.inventory.Identity.WorkerID = "worker-b"
			cold.inventory.Profiles = append([]ProfileInventory(nil), w.inventory.Profiles...)
			cold.inventory.Profiles[0].Ready = 0
			cold.inventory.Used = CapacityUsed{}
			cold.records = map[string]Record{}
			alias := d
			alias.QueueID = "another-scope|alias"
			alias.Label = "alias"
			alias.ScopeURL = "https://github.com/another-scope"
			if err := reopened.Sync(context.Background(), []Demand{alias}, map[string]Worker{"worker-a": unavailableAbandonedReserve{w}, "worker-b": &cold}, map[string]Backend{d.QueueID: backend, alias.QueueID: backend}); err != nil {
				t.Fatal(err)
			}
			if len(reopened.assignments) != 2 || reopened.assignments[1].Worker.WorkerID != "worker-b" {
				t.Fatalf("unobserved %s did not direct alias to available worker: %#v", phase, reopened.assignments)
			}
			if reopened.scopeCount(d.ScopeURL) != 1 {
				t.Fatal("unobserved assignment released old quota")
			}
		})
	}
}
