package fleet

import (
	"context"
	"testing"
	"time"
)

type failedDeadlineJIT struct {
	calls    int
	deadline time.Time
	before   func()
}

func (b *failedDeadlineJIT) JIT(ctx context.Context, _ string) (string, error) {
	b.calls++
	if b.before != nil {
		b.before()
	}
	b.deadline, _ = ctx.Deadline()
	return "", context.DeadlineExceeded
}
func (*failedDeadlineJIT) Remove(context.Context, string) error { return nil }
func TestJITDeadlineRetainsUncertainIntentWithoutReplay(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &failedDeadlineJIT{before: func() {
		if b.assignments[0].Phase != JITIntent || w.seal != 1 {
			t.Fatal("JIT preceded durable seal/credential intent")
		}
	}}
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if backend.deadline.IsZero() || time.Until(backend.deadline) > 5*time.Second || b.assignments[0].Phase != Uncertain || !b.assignments[0].CredentialUncertain || w.deliver != 0 {
		t.Fatal("unbounded JIT or ambiguous credential attempt reused")
	}
	if err := b.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(b.dir, b.id, b.limits)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Sync(context.Background(), []Demand{d}, workers, backends); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 || w.seal != 1 || w.deliver != 0 {
		t.Fatal("ambiguous JIT replayed after reconciliation/restart")
	}
}
