package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
)

type demandFixture struct {
	inbox           *demandUpdates
	polls           map[string]poller
	gens, revisions map[string]uint64
	demands         map[string]fleet.Demand
	observed        map[string]time.Time
}

func newDemandFixture(n int) *demandFixture {
	f := &demandFixture{newDemandUpdates(), map[string]poller{}, map[string]uint64{}, map[string]uint64{}, map[string]fleet.Demand{}, map[string]time.Time{}}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("queue-%d", i)
		f.polls[key] = poller{queue: dispatch.Queue{Key: key, Scope: "https://github.com/example", Label: key, Max: 2, ScopeMax: 3}}
		f.gens[key] = 1
	}
	return f
}
func (f *demandFixture) apply() {
	applyDemandUpdates(f.inbox.take(), f.polls, f.gens, f.demands, f.revisions, f.observed)
}

func TestDemandConvergesBeforeNextReconciliationAfterSlowSync(t *testing.T) {
	f := newDemandFixture(21)
	// Simulate a worker sync blocked across many refresh rounds. Publishers must
	// remain independent of reconciliation and retain each queue's newest count.
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { close(entered); <-release; f.apply(); close(done) }()
	<-entered
	at := time.Now()
	for round := 0; round < 40; round++ {
		for key := range f.polls {
			n := 1
			if round == 39 {
				n = 0
			}
			f.inbox.publish(update{key: key, n: n, generation: 1, observedAt: at.Add(time.Duration(round) * time.Second)})
		}
	}
	if got := len(f.inbox.pending); got != 21 {
		t.Fatalf("historical counts accumulated: %d entries", got)
	}
	close(release)
	<-done
	for key, d := range f.demands {
		if d.Assigned != 0 || d.Revision != 1 {
			t.Fatalf("queue %s replayed historical demand: %+v", key, d)
		}
	}
	if len(f.demands) != 21 {
		t.Fatal("next reconciliation missed a queue")
	}
}

func TestDemandCoalescingPreservesCompletionsAndRejectsOlderCounts(t *testing.T) {
	f := newDemandFixture(1)
	at := time.Now()
	key := "queue-0"
	f.inbox.publish(update{key: key, n: 0, generation: 1, observedAt: at.Add(time.Second), completed: []string{"newer"}})
	// A statistics request started before the listener's zero and finished later.
	f.inbox.publish(update{key: key, n: 2, generation: 1, observedAt: at, completed: []string{"older"}})
	f.apply()
	if d := f.demands[key]; d.Assigned != 0 || !reflect.DeepEqual(d.CompletedRunners, []string{"newer", "older"}) {
		t.Fatalf("stale refresh replaced count or lost completion: %+v", d)
	}
	// The same ordering fence must hold across separate reconciliation batches.
	f.inbox.publish(update{key: key, n: 2, generation: 1, observedAt: at})
	f.apply()
	if f.demands[key].Assigned != 0 || f.revisions[key] != 1 {
		t.Fatal("old count escaped the observation fence")
	}
	f.inbox.publish(update{key: key, n: 2, generation: 1, observedAt: at, completed: []string{"late-completion"}})
	f.apply()
	if d := f.demands[key]; d.Assigned != 0 || len(d.CompletedRunners) != 3 || d.Revision != 1 {
		t.Fatalf("late completion was discarded or restored old count: %+v", d)
	}
	f.inbox.publish(update{key: key, n: 1, generation: 1, observedAt: at.Add(2 * time.Second)})
	f.apply()
	if f.demands[key].Assigned != 1 {
		t.Fatal("fresh work failed to reopen demand")
	}
}

func TestDemandGenerationFenceAndConcurrentPublication(t *testing.T) {
	f := newDemandFixture(1)
	key := "queue-0"
	at := time.Now()
	f.gens[key] = 2
	f.inbox.publish(update{key: key, n: 1, generation: 2, observedAt: at})
	f.inbox.publish(update{key: key, n: 2, generation: 1, observedAt: at.Add(time.Hour), completed: []string{"old-session"}})
	f.apply()
	if d := f.demands[key]; d.Assigned != 1 || len(d.CompletedRunners) != 0 {
		t.Fatalf("old generation escaped fence: %+v", d)
	}
	var publishers sync.WaitGroup
	for i := 0; i < 50; i++ {
		publishers.Add(1)
		go func(i int) {
			defer publishers.Done()
			f.inbox.publish(update{key: key, n: i % 3, generation: 2, observedAt: at.Add(time.Duration(i+1) * time.Second), completed: []string{fmt.Sprint(i)}})
		}(i)
	}
	publishers.Wait()
	f.apply()
	if d := f.demands[key]; d.Assigned != 49%3 || len(d.CompletedRunners) != 50 {
		t.Fatalf("concurrent updates lost the newest count or completions: %+v", d)
	}
	// A notification left over from a tick-driven drain is harmless; subsequent
	// publication must still be visible without requiring another notification.
	f.inbox.publish(update{key: key, n: 2, generation: 2, observedAt: at.Add(time.Hour)})
	f.apply()
	<-f.inbox.ready
	f.inbox.publish(update{key: key, n: 0, generation: 2, observedAt: at.Add(2 * time.Hour)})
	select {
	case <-f.inbox.ready:
	case <-time.After(time.Second):
		t.Fatal("lost notification after drain")
	}
	f.apply()
	if f.demands[key].Assigned != 0 {
		t.Fatal("latest observation was lost")
	}
}

type creditWorker struct {
	snapshot fleet.Inventory
	reserves int
}

func (w *creditWorker) Inventory(context.Context) (fleet.Inventory, error) { return w.snapshot, nil }
func (w *creditWorker) Reserve(context.Context, string, string, string) (fleet.Record, error) {
	w.reserves++
	return fleet.Record{}, fleet.ErrNotFound
}
func (*creditWorker) Seal(context.Context, string) (fleet.Record, error) {
	return fleet.Record{}, fleet.ErrNotFound
}
func (*creditWorker) Deliver(context.Context, string, string) error { return fleet.ErrNotFound }
func (*creditWorker) Status(context.Context, string) (fleet.Record, error) {
	return fleet.Record{}, fleet.ErrNotFound
}
func (*creditWorker) Drain(context.Context) error { return nil }

type creditBackend struct{}

func (creditBackend) JIT(context.Context, string) (string, error) {
	return "", fmt.Errorf("unexpected JIT")
}
func (creditBackend) Remove(context.Context, string) error { return nil }

func TestStaleCompletionPreservesOtherRunnerDemandCredit(t *testing.T) {
	for _, target := range []string{"unrelated", "credited"} {
		t.Run(target, func(t *testing.T) {
			f := newDemandFixture(1)
			key := "queue-0"
			at := time.Now()
			p := f.polls[key]
			p.queue.ImageDigest = "synthetic-image"
			p.queue.Machine = "q35"
			p.queue.CPUs = 2
			p.queue.MemoryMiB = 1024
			p.queue.DiskGiB = 4
			f.polls[key] = p
			identity := fleet.Identity{WorkerID: "worker", BrokerID: "test", Generation: 1}
			profile := fleet.Profile{ID: "small", Digest: p.queue.ImageDigest, Machine: p.queue.Machine, CPUs: p.queue.CPUs, MemoryMiB: p.queue.MemoryMiB, DiskGiB: p.queue.DiskGiB}
			a := fleet.Assignment{ID: "credited", RunnerName: "credited", QueueID: key, ScopeURL: p.queue.Scope, Label: p.queue.Label, Profile: profile, Worker: identity, WorkerProfileID: profile.ID, Phase: fleet.Complete, ReservedAt: at.Add(-time.Minute), CompletedAt: at.Add(-time.Second), DemandRevision: 1, LastDemandRevision: 1}
			b := a
			b.ID = "unrelated"
			b.RunnerName = b.ID
			b.GitHubCompletedRevision = 1
			raw, err := json.Marshal(struct {
				Version     int
				BrokerID    string
				Assignments []fleet.Assignment
			}{1, "test", []fleet.Assignment{a, b}})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "assignments.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			broker, err := fleet.Open(dir, "test", fleet.Limits{MaxVMs: 3, MaxCPUs: 6, MaxMemoryMiB: 3072})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			worker := &creditWorker{snapshot: fleet.Inventory{Identity: identity, Budget: fleet.Budget{MaxVMs: 3, MaxCPUs: 6, MaxMemoryMiB: 3072}, Profiles: []fleet.ProfileInventory{{Profile: profile, Ready: 1}}}}
			reconcile := func() {
				t.Helper()
				if err := broker.Sync(context.Background(), []fleet.Demand{f.demands[key]}, map[string]fleet.Worker{"worker": worker}, map[string]fleet.Backend{key: creditBackend{}}); err != nil {
					t.Fatal(err)
				}
			}
			f.inbox.publish(update{key: key, n: 1, generation: 1, observedAt: at})
			f.apply()
			reconcile()
			if worker.reserves != 0 {
				t.Fatal("fixture did not preserve existing completion credit")
			}
			// This late event carries an obsolete count. Its runner identity remains
			// useful, but must not pretend that an additional count observation arrived.
			f.inbox.publish(update{key: key, n: 2, generation: 1, observedAt: at.Add(-time.Second), completed: []string{target}})
			f.apply()
			reconcile()
			want := 0
			if target == "credited" {
				want = 1
			}
			if worker.reserves != want {
				t.Fatalf("stale completion for %s caused %d reservations, want %d", target, worker.reserves, want)
			}
			if f.demands[key].Revision != 1 || f.demands[key].Assigned != 1 {
				t.Fatalf("stale completion changed accepted count/revision: %+v", f.demands[key])
			}
			if len(broker.Assignments()) != 2+want {
				t.Fatal("stale completion produced an unexpected durable intent")
			}
		})
	}
}
