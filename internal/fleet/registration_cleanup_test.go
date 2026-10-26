package fleet

import (
	"context"
	"testing"
	"time"
)

type blockedCleanupBackend struct {
	*fakeBackend
	started chan struct{}
	release chan struct{}
}

func (b *blockedCleanupBackend) Remove(ctx context.Context, _ string) error {
	close(b.started)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestTerminalCleanupDoesNotBlockOtherQueues(t *testing.T) {
	b, w, base, d := fixture(t)
	backend := &blockedCleanupBackend{base, make(chan struct{}), make(chan struct{})}
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	a := b.assignments[0]
	r := w.records[a.ID]
	r.State = "terminal"
	w.records[a.ID] = r
	other := d
	other.QueueID = "other"
	other.ScopeURL = "https://github.com/other"
	backends[other.QueueID] = &fakeBackend{}
	done := make(chan error, 1)
	go func() { done <- b.Sync(context.Background(), []Demand{other}, workers, backends) }()
	<-backend.started
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if w.reserve != 2 || b.assignments[0].Phase != Terminal {
			t.Fatal("unrelated queue stalled or terminal quota released early")
		}
	case <-time.After(2 * time.Second):
		close(backend.release)
		<-done
		t.Fatal("one registration removal blocked unrelated queue scheduling")
	}
}

type controlledCleanupBackend struct {
	*fakeBackend
	calls   chan string
	release chan error
}

func (b *controlledCleanupBackend) Remove(ctx context.Context, name string) error {
	select {
	case b.calls <- name:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-b.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func terminalFixture(t *testing.T, n int) (*Broker, *fakeWorker, *controlledCleanupBackend, Demand) {
	t.Helper()
	b, w, base, d := fixture(t)
	backend := &controlledCleanupBackend{base, make(chan string, 16), make(chan error, 16)}
	d.Assigned = n
	d.ScopeMax = n
	for i := 0; i < n; i++ {
		if err := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); err != nil {
			t.Fatal(err)
		}
	}
	for id, r := range w.records {
		r.State = "terminal"
		w.records[id] = r
	}
	return b, w, backend, d
}
func awaitCleanup(t *testing.T, b *Broker, id string) {
	t.Helper()
	b.mu.Lock()
	check := b.registrationChecks[id]
	b.mu.Unlock()
	if check == nil {
		t.Fatal("cleanup not pending")
	}
	select {
	case <-check.done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
}
func TestRegistrationCleanupBoundedAndFailureRetainsQuota(t *testing.T) {
	b, w, backend, d := terminalFixture(t, 3)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if err := b.Sync(context.Background(), nil, workers, backends); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRegistrationChecks; i++ {
		select {
		case <-backend.calls:
		case <-time.After(time.Second):
			t.Fatal("cleanup did not start")
		}
	}
	if len(b.registrationChecks) != maxRegistrationChecks || b.scopeCount(d.ScopeURL) != 3 {
		t.Fatal("unbounded cleanup or premature quota release")
	}
	ids := []string{}
	for id := range b.registrationChecks {
		ids = append(ids, id)
	}
	backend.release <- context.DeadlineExceeded
	backend.release <- context.DeadlineExceeded
	for _, id := range ids {
		awaitCleanup(t, b, id)
	}
	if err := b.Sync(context.Background(), nil, workers, backends); err != nil {
		t.Fatal(err)
	}
	if b.scopeCount(d.ScopeURL) != 3 {
		t.Fatal("failed cleanup released quota")
	}
	select {
	case <-backend.calls:
	case <-time.After(time.Second):
		t.Fatal("unattempted cleanup starved")
	}
	// Only the third assignment runs now; recent failures keep their retry bound.
	if len(b.registrationChecks) != 1 {
		t.Fatal("recent failed cleanup retried immediately")
	}
}
func TestCloseCancelsCleanupBeforeReleasingJournal(t *testing.T) {
	b, w, backend, d := terminalFixture(t, 1)
	if err := b.Sync(context.Background(), nil, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.calls:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	id := b.assignments[0].ID
	b.mu.Lock()
	check := b.registrationChecks[id]
	b.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel cleanup")
	}
	select {
	case <-check.done:
	default:
		t.Fatal("journal released before cleanup ended")
	}
	recovered, err := Open(b.dir, b.id, b.limits)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.assignments[0].Phase != Terminal || recovered.assignments[0].RegistrationCheckedAt.IsZero() {
		t.Fatal("cancellation lost quota ownership or durable retry stamp")
	}
	if err := recovered.Sync(context.Background(), nil, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); err != nil {
		t.Fatal(err)
	}
	if len(recovered.registrationChecks) != 0 {
		t.Fatal("restart ignored durable retry bound")
	}
}

func TestDelayedRegistrationCleanupDoesNotBlockAdmission(t *testing.T) {
	b, w, backend, d := terminalFixture(t, 1)
	b.assignments[0].Phase = Complete
	b.assignments[0].CredentialUncertain = true
	b.assignments[0].CompletedAt = time.Now().UTC()
	if err := b.persist(); err != nil {
		t.Fatal(err)
	}
	other := d
	other.QueueID = "another"
	other.ScopeURL = "https://github.com/another"
	done := make(chan error, 1)
	go func() {
		done <- b.Sync(context.Background(), []Demand{other}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend, other.QueueID: &fakeBackend{}})
	}()
	select {
	case <-backend.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed cleanup did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		backend.release <- nil
		<-done
		t.Fatal("delayed cleanup blocked another queue")
	}
	if w.reserve != 2 {
		t.Fatal("unrelated queue was not admitted")
	}
}
