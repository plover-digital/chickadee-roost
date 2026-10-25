package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type idleBackend struct {
	fakeBackend
	retireCalls  int
	retireErr    error
	beforeRetire func(context.Context, string)
}

func (b *idleBackend) RetireIdle(ctx context.Context, name string) error {
	b.retireCalls++
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
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	old := b.assignments[0]
	b.assignments[0].ReservedAt = time.Now().Add(-3 * time.Minute)
	d.Assigned = 0
	d.Revision++
	if err := b.Sync(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if backend.retireCalls != 1 {
		t.Fatalf("overdue runner was not deregistered: calls=%d", backend.retireCalls)
	}
	if w.reserve != 1 || !b.scopeBusy(d.ScopeURL) {
		t.Fatal("deregistration released quota before process/disk cleanup")
	}
	if b.assignments[0].Phase != Delivered {
		t.Fatal("deregistration changed durable execution ownership")
	}
	if err := b.Sync(context.Background(), []Demand{d, other}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if backend.retireCalls != 1 {
		t.Fatal("unbounded deregistration retries")
	}
	r := w.records[old.ID]
	r.State = "terminal"
	r.CompletedAt = time.Now().UTC()
	w.records[old.ID] = r
	if err := b.Sync(context.Background(), []Demand{d, other}, workers, backends); err != nil {
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
			b.assignments[0].ReservedAt = time.Now().Add(-3 * time.Minute)
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
			if backend.retireCalls != 1 || w.reserve != 1 || w.deliver != 1 || backend.jit != 1 {
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
			if backend.retireCalls != 1 || w.reserve != 1 || w.deliver != 1 || !recovered.scopeBusy(d.ScopeURL) {
				t.Fatal("restart retried early or released uncertain execution")
			}
			recovered.assignments[0].RegistrationCheckedAt = time.Now().Add(-time.Minute)
			backend.retireErr = nil
			if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
				t.Fatal(err)
			}
			if backend.retireCalls != 2 || w.reserve != 1 {
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
			b.assignments[0].ReservedAt = time.Now().Add(-3 * time.Minute)
			switch state {
			case "fresh":
				b.assignments[0].ReservedAt = time.Now()
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
			if backend.retireCalls != 0 || w.deliver != 1 || backend.jit != 1 {
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
	b.assignments[0].ReservedAt = time.Now().Add(-3 * time.Minute)
	// Move the directory away so fsync/rename cannot establish the attempt.
	original := b.dir
	if err := os.Rename(original, original+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(original+"-unavailable", original)
	if err := b.retireIdle(context.Background(), 0, backend, time.Now()); err == nil {
		t.Fatal("unpersisted retirement accepted")
	}
	if backend.retireCalls != 0 || w.reserve != 1 || w.deliver != 1 {
		t.Fatal("GitHub called or credentials replayed after persistence failure")
	}
}
