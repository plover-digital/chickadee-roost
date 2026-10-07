package fleet

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTelemetryDeduplicatesPhysicalAliasesAndDurableAssignments(t *testing.T) {
	p := Profile{ID: "default", Digest: "image", Machine: "q35", CPUs: 4, MemoryMiB: 8192, DiskGiB: 48}
	alias := p
	alias.ID = "medium"
	small := p
	small.ID = "small"
	small.CPUs = 2
	small.MemoryMiB = 4096
	snapshots := map[string]Inventory{"a": {Budget: Budget{MaxVMs: 3}, Used: CapacityUsed{VMs: 2}, Profiles: []ProfileInventory{{Profile: p, Ready: 1, Booting: 1}, {Profile: alias, Ready: 1, Booting: 1}}}, "b": {Budget: Budget{MaxVMs: 3}, Used: CapacityUsed{VMs: 3}, Profiles: []ProfileInventory{{Profile: small, Ready: 2, Booting: 1}}}}
	phases := []string{Intent, Reserved, SealIntent, JITIntent, Delivered, Uncertain, Terminal, Complete}
	assignments := []Assignment{}
	for i, phase := range phases {
		assignments = append(assignments, Assignment{ID: string(rune('a' + i)), Phase: phase})
	}
	assignments = append(assignments, assignments[0])
	at := time.Now().UTC()
	sample := telemetrySnapshot(at, assignments, snapshots, 3)
	if sample.Ready != 3 || sample.Booting != 2 || sample.Reserved != 3 || sample.Running != 2 || sample.Uncertain != 1 || sample.WorkersOnline != 2 || sample.WorkersTotal != 3 || !sample.At.Equal(at) {
		t.Fatalf("incorrect telemetry: %+v", sample)
	}
}

type telemetryWorker struct {
	*fakeWorker
	polls       int
	failRefresh bool
}

func (w *telemetryWorker) Inventory(ctx context.Context) (Inventory, error) {
	w.polls++
	if w.failRefresh && w.polls > 1 {
		return Inventory{}, errors.New("unavailable")
	}
	return w.fakeWorker.Inventory(ctx)
}
func TestTelemetryUsesExistingSyncPollsAndClearsOfflineWarmCounts(t *testing.T) {
	b, w, _, _ := fixture(t)
	worker := &telemetryWorker{fakeWorker: w}
	offline := &fakeWorker{unavailable: true}
	workers := map[string]Worker{"worker-a": worker, "offline": offline}
	if err := b.Sync(context.Background(), nil, workers, nil); err != nil {
		t.Fatal(err)
	}
	first := b.Telemetry()
	if worker.polls != 1 || first.Ready != 1 || first.WorkersOnline != 1 || first.WorkersTotal != 2 {
		t.Fatalf("unexpected initial coverage: %+v polls%d", first, worker.polls)
	}
	w.unavailable = true
	if err := b.Sync(context.Background(), nil, workers, nil); err != nil {
		t.Fatal(err)
	}
	next := b.Telemetry()
	if worker.polls != 2 || next.Ready != 0 || next.Booting != 0 || next.WorkersOnline != 0 || next.WorkersTotal != 2 || next.At.Before(first.At) {
		t.Fatalf("stale/offline inventory retained: %+v", next)
	}
}
func TestTelemetryRefreshFailureKeepsCredentialIntentButDropsStaleCapacity(t *testing.T) {
	b, w, backend, demand := fixture(t)
	worker := &telemetryWorker{fakeWorker: w, failRefresh: true}
	if err := b.Sync(context.Background(), []Demand{demand}, map[string]Worker{"worker-a": worker}, map[string]Backend{demand.QueueID: backend}); err != nil {
		t.Fatal(err)
	}
	sample := b.Telemetry()
	if worker.polls != 2 || sample.Ready != 0 || sample.WorkersOnline != 0 || sample.WorkersTotal != 1 || sample.Running != 1 {
		t.Fatalf("refresh failure misrepresented intent/coverage: %+v polls%d", sample, worker.polls)
	}
}
func TestTelemetryRejectsImpossiblePhysicalWarmCount(t *testing.T) {
	p := Profile{Digest: "image", Machine: "q35", CPUs: 2, MemoryMiB: 4096, DiskGiB: 48}
	sample := telemetrySnapshot(time.Now(), nil, map[string]Inventory{"host": {Budget: Budget{MaxVMs: 1}, Used: CapacityUsed{VMs: 1}, Profiles: []ProfileInventory{{Profile: p, Ready: 1, Booting: 1}}}}, 1)
	if sample.Ready != 0 || sample.Booting != 0 || sample.WorkersOnline != 0 {
		t.Fatal("impossible warm inventory exported as current")
	}
}
