package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type idleBackend struct {
	fakeBackend
	retireCalls  atomic.Int32
	retireErr    error
	beforeRetire func(context.Context, string)
}

func (b *idleBackend) RetireIdle(ctx context.Context, name string) error {
	b.retireCalls.Add(1)
	if b.beforeRetire != nil {
		b.beforeRetire(ctx, name)
	}
	return b.retireErr
}

func TestOverdueIdleRunnerRetainsQuotaUntilWorkerCleanup(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &idleBackend{}
	workers := map[string]Worker{"worker-a": w}
	other := d
	other.QueueID = "scope|other"
	other.Label = "other"
	backends := map[string]Backend{d.QueueID: backend, other.QueueID: backend}
	if err := b.syncAndCleanup(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	old := b.assignments[0]
	b.assignments[0].ReservedAt = time.Now()
	b.idleSince[b.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
	d.Assigned = 0
	d.Revision++
	if err := b.syncAndCleanup(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	waitIdleChecks(t, b)
	if backend.retireCalls.Load() != 1 {
		t.Fatalf("overdue runner was not deregistered: calls=%d", backend.retireCalls.Load())
	}
	if w.reserve != 1 || !b.scopeBusy(d.ScopeURL) {
		t.Fatal("deregistration released quota before process/disk cleanup")
	}
	if b.assignments[0].Phase != Delivered {
		t.Fatal("deregistration changed durable execution ownership")
	}
	if err := b.syncAndCleanup(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	waitIdleChecks(t, b)
	if backend.retireCalls.Load() != 1 {
		t.Fatal("unbounded deregistration retries")
	}
	r := w.records[old.ID]
	r.State = "terminal"
	r.CompletedAt = time.Now().UTC()
	w.records[old.ID] = r
	if err := b.syncAndCleanup(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if w.reserve != 2 {
		t.Fatal("next queue stayed blocked after confirmed cleanup")
	}
}

func TestIdleRetirementRetainsQuotaAndRetryThrottleAcrossRestart(t *testing.T) {
	for _, failure := range []string{"successful deregistration", "job started during deletion", "GitHub unavailable", "deletion acknowledgement lost"} {
		t.Run(failure, func(t *testing.T) {
			b, w, _, d := fixture(t)
			backend := &idleBackend{}
			if failure != "successful deregistration" {
				backend.retireErr = errors.New(failure)
			}
			workers := map[string]Worker{"worker-a": w}
			backends := map[string]Backend{d.QueueID: backend}
			if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			b.assignments[0].ReservedAt = time.Now()
			b.idleSince[b.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
			backend.beforeRetire = func(ctx context.Context, name string) {
				if name != b.assignments[0].RunnerName {
					t.Fatal("wrong runner retired")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("unbounded retirement request")
				}
				raw, err := os.ReadFile(filepath.Join(b.dir, "assignments.json"))
				if err != nil {
					t.Fatal(err)
				}
				var saved journal
				if json.Unmarshal(raw, &saved) != nil || saved.Assignments[0].RegistrationCheckedAt.IsZero() {
					t.Fatal("retirement preceded durable attempt record")
				}
			}
			if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			waitIdleChecks(t, b)
			if backend.retireCalls.Load() != 1 || w.reserve != 1 || w.deliver != 1 || backend.jit != 1 {
				t.Fatal("retirement failure replayed or replaced execution")
			}
			checked := b.assignments[0].RegistrationCheckedAt
			if checked.IsZero() {
				t.Fatal("retirement attempt not recorded")
			}
			dir, limits := b.dir, b.limits
			b.Close()
			recovered, err := Open(dir, "roost", limits)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if !recovered.assignments[0].RegistrationCheckedAt.Equal(checked) {
				t.Fatal("retry throttle lost on restart")
			}
			backend.beforeRetire = nil
			if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			waitIdleChecks(t, recovered)
			if backend.retireCalls.Load() != 1 || w.reserve != 1 || w.deliver != 1 || !recovered.scopeBusy(d.ScopeURL) {
				t.Fatal("restart retried early or released uncertain execution")
			}
			recovered.assignments[0].RegistrationCheckedAt = time.Now().Add(-time.Minute)
			recovered.idleSince[recovered.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
			backend.retireErr = nil
			if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			waitIdleChecks(t, recovered)
			if backend.retireCalls.Load() != 2 || w.reserve != 1 {
				t.Fatal("retirement did not retry safely")
			}
		})
	}
}

func TestIdleRetirementSkipsFreshUndeliveredUncertainAndUnreachableExecutions(t *testing.T) {
	for _, state := range []string{"fresh", "reserved", "seal_intent", "jit_intent", "uncertain", "worker-offline", "worker-generation-changed", "record-mismatch", "credentials-uncertain"} {
		t.Run(state, func(t *testing.T) {
			b, w, _, d := fixture(t)
			backend := &idleBackend{}
			workers := map[string]Worker{"worker-a": w}
			backends := map[string]Backend{d.QueueID: backend}
			if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			b.assignments[0].ReservedAt = time.Now()
			b.idleSince[b.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
			switch state {
			case "fresh":
				b.assignments[0].ReservedAt = time.Now()
				b.idleSince[b.assignments[0].ID] = time.Now()
			case "worker-offline":
				w.unavailable = true
			case "worker-generation-changed":
				w.inventory.Identity.Generation++
			case "credentials-uncertain":
				b.assignments[0].CredentialUncertain = true
			case "record-mismatch":
				r := w.records[b.assignments[0].ID]
				r.Request.VMID = "foreign"
				w.records[b.assignments[0].ID] = r
			default:
				b.assignments[0].Phase = state
			}
			// No demand must not resume a reservation or mint another credential.
			d.Assigned = 0
			d.Revision++
			_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
			waitIdleChecks(t, b)
			if backend.retireCalls.Load() != 0 || w.deliver != 1 || backend.jit != 1 {
				t.Fatal("unsafe execution was retired or credentials replayed")
			}
		})
	}
}

func TestIdleRetirementDoesNotCallGitHubWhenAttemptCannotBePersisted(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &idleBackend{}
	if err := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); err != nil {
		t.Fatal(err)
	}
	b.assignments[0].ReservedAt = time.Now()
	b.idleSince[b.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
	// Move the directory away so fsync/rename cannot establish the attempt.
	original := b.dir
	if err := os.Rename(original, original+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(original+"-unavailable", original)
	if err := b.retireIdle(context.Background(), 0, backend, time.Now()); err == nil {
		t.Fatal("unpersisted retirement accepted")
	}
	waitIdleChecks(t, b)
	if backend.retireCalls.Load() != 0 || w.reserve != 1 || w.deliver != 1 {
		t.Fatal("GitHub called or credentials replayed after persistence failure")
	}
}

func waitIdleChecks(t *testing.T, b *Broker) {
	t.Helper()
	for _, check := range b.idleChecks {
		select {
		case <-check.done:
		case <-time.After(2 * time.Second):
			t.Fatal("idle request did not finish")
		}
	}
}

func TestIdleGraceStartsAfterDeliveryAndAfterRecovery(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &idleBackend{}
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	// Boot/seal/JIT may consume the entire reservation grace before ACK. An old
	// reservation must not make the newly delivered runner eligible immediately.
	b.assignments[0].ReservedAt = time.Now().Add(-10 * time.Minute)
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	waitIdleChecks(t, b)
	if backend.retireCalls.Load() != 0 {
		t.Fatal("slow provisioning consumed listener grace")
	}
	dir, limits := b.dir, b.limits
	if err := b.persist(); err != nil {
		t.Fatal(err)
	}
	b.Close()
	recovered, err := Open(dir, "roost", limits)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	waitIdleChecks(t, recovered)
	if backend.retireCalls.Load() != 0 || recovered.idleSince[b.assignments[0].ID].IsZero() {
		t.Fatal("recovery failed to grant fresh observation grace")
	}
	recovered.idleSince[b.assignments[0].ID] = time.Now().Add(-3 * time.Minute)
	if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	waitIdleChecks(t, recovered)
	if backend.retireCalls.Load() != 1 {
		t.Fatal("eligible recovered runner never checked")
	}
}

func TestBlockedIdleChecksDoNotBlockOtherScopesAndAreBounded(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &idleBackend{}
	started := make(chan struct{}, 8)
	backend.beforeRetire = func(ctx context.Context, _ string) { started <- struct{}{}; <-ctx.Done() }
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	d.ScopeMax = 2
	d.Assigned = 2
	for i := 0; i < 2; i++ {
		if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range b.assignments {
		b.idleSince[a.ID] = time.Now().Add(-3 * time.Minute)
	}
	other := d
	other.QueueID = "another|queue"
	other.ScopeURL = "https://github.com/another"
	other.ScopeMax = 1
	other.Assigned = 1
	backends[other.QueueID] = backend
	done := make(chan error, 1)
	go func() { done <- b.Sync(context.Background(), []Demand{d, other}, workers, backends) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked retirement stalled fleet admission")
	}
	if w.reserve != 3 {
		t.Fatal("unrelated scope could not use free capacity")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("background checks did not start")
		}
	}
	// A third overdue allocation cannot start another request, even if previous
	// attempts have aged past the retry interval while their calls remain blocked.
	for i, a := range b.assignments {
		b.idleSince[a.ID] = time.Now().Add(-3 * time.Minute)
		b.assignments[i].RegistrationCheckedAt = time.Now().Add(-time.Minute)
	}
	if err := b.Sync(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if len(b.idleChecks) != maxIdleChecks || backend.retireCalls.Load() != 2 {
		t.Fatal("background request bound or per-assignment fencing lost")
	}
	// Close must cancel/join background work before the state lock can be reused.
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("broker close did not cancel background checks")
	}
	waitIdleChecks(t, b)
}

func TestIdleChecksDoNotStarveLaterAllocationsWhenRetriesBecomeDue(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &idleBackend{}
	d.ScopeMax = 3
	d.Assigned = 3
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	for i := 0; i < 3; i++ {
		if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, a := range b.assignments {
		b.idleSince[a.ID] = now.Add(-3 * time.Minute)
	}
	started := make(chan string, 8)
	release := make(chan struct{})
	backend.beforeRetire = func(ctx context.Context, name string) {
		started <- name
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	candidates := []int{0, 1, 2}
	if err := b.scheduleIdleChecks(context.Background(), candidates, backends, now); err != nil {
		t.Fatal(err)
	}
	first := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case name := <-started:
			first[name] = true
		case <-time.After(time.Second):
			t.Fatal("checks never started")
		}
	}
	missing := ""
	for _, a := range b.assignments {
		if !first[a.RunnerName] {
			missing = a.RunnerName
		}
	}
	close(release)
	waitIdleChecks(t, b)
	// Model a slow reconciliation: the earliest busy checks are due again by
	// the next scheduling pass. The never-attempted runner must still get a slot.
	release = make(chan struct{})
	if err := b.scheduleIdleChecks(context.Background(), candidates, backends, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	seenMissing := false
	for i := 0; i < 2; i++ {
		select {
		case name := <-started:
			seenMissing = seenMissing || name == missing
		case <-time.After(time.Second):
			t.Fatal("next checks never started")
		}
	}
	close(release)
	waitIdleChecks(t, b)
	if !seenMissing {
		t.Fatal("busy retries starved the later idle allocation")
	}
}
