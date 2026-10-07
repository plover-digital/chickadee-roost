package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type lostSealWorker struct {
	*fakeWorker
	loseOnce bool
}

func (w *lostSealWorker) Seal(ctx context.Context, id string) (Record, error) {
	record, e := w.fakeWorker.Seal(ctx, id)
	if e == nil && w.loseOnce {
		w.loseOnce = false
		return Record{}, errors.New("seal acknowledgement lost")
	}
	return record, e
}

func TestDisconnectedDeliveredWorkerRetainsScopeQuotaWhileHealthyPeerWorks(t *testing.T) {
	b, first, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": first}
	backends := map[string]Backend{d.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	old := b.assignments[0]
	_, peer, peerBackend, _ := fixture(t)
	peer.inventory.Identity.WorkerID = "worker-b"
	workers["worker-b"] = peer
	other := d
	other.QueueID = "another-scope"
	other.ScopeURL = "https://github.com/another"
	alias := d
	alias.QueueID = "same-scope-alias"
	alias.Label = "medium-versioned"
	alias.ScopeURL = "https://github.com/EXAMPLE/"
	backends[other.QueueID] = peerBackend
	backends[alias.QueueID] = backend
	first.unavailable = true
	if e := b.Sync(context.Background(), []Demand{d, alias, other}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if first.deliver != 1 || first.reserve != 1 || peer.reserve != 1 || backend.jit != 1 || peerBackend.jit != 1 || b.scopeCount(d.ScopeURL) != 1 || b.assignments[0].Phase != Delivered {
		t.Fatal("offline scope moved or blocked unrelated healthy scope")
	}
	first.unavailable = false
	if e := b.Sync(context.Background(), []Demand{d, alias, other}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if first.records[old.ID].State != "delivery_intent" || first.deliver != 1 || backend.jit != 1 || peer.reserve != 1 {
		t.Fatal("reconnection replayed credentials or duplicated the peer job")
	}
}
func TestLostSealAcknowledgementRestartMintsExactlyOnce(t *testing.T) {
	b, base, backend, d := fixture(t)
	worker := &lostSealWorker{fakeWorker: base, loseOnce: true}
	workers := map[string]Worker{"worker-a": worker}
	backends := map[string]Backend{d.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if b.assignments[0].Phase != SealIntent || backend.jit != 0 || base.deliver != 0 {
		t.Fatal("lost seal acknowledgement advanced credential generation")
	}
	dir := b.dir
	b.Close()
	restart, e := Open(dir, "roost", b.limits)
	if e != nil {
		t.Fatal(e)
	}
	defer restart.Close()
	if e = restart.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if base.reserve != 1 || backend.jit != 1 || base.deliver != 1 || restart.assignments[0].Phase != Delivered {
		t.Fatal("seal recovery allocated again or generated credentials more than once")
	}
	if e = restart.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if backend.jit != 1 || base.deliver != 1 {
		t.Fatal("repeated recovery replayed JIT")
	}
}

func TestKnownAssignmentCannotHideWrongWorkerOrVMOrphan(t *testing.T) {
	for _, mismatch := range []string{"worker", "vm", "profile", "completed"} {
		t.Run(mismatch, func(t *testing.T) {
			b, original, backend, d := fixture(t)
			workers := map[string]Worker{"worker-a": original}
			backends := map[string]Backend{d.QueueID: backend}
			_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
			a := b.assignments[0]
			_, peer, _, _ := fixture(t)
			peer.inventory.Identity.WorkerID = "worker-b"
			record := original.records[a.ID]
			switch mismatch {
			case "worker":
				record.Request.Identity = peer.inventory.Identity
				peer.records[a.ID] = record
			case "vm":
				record.Request.VMID = "unexpected-vm"
				original.records[a.ID] = record
			case "profile":
				record.Request.ProfileDigest = "unexpected-image"
				original.records[a.ID] = record
			case "completed":
				b.assignments[0].Phase = Complete
			}
			workers["worker-b"] = peer
			other := d
			other.QueueID = "other"
			other.ScopeURL = "https://github.com/other"
			backends[other.QueueID] = backend
			if e := b.Sync(context.Background(), []Demand{other}, workers, backends); !errors.Is(e, ErrUnjournaled) {
				t.Fatal("known ID bypassed inventory ownership reconciliation")
			}
			if peer.reserve != 0 || original.reserve != 1 {
				t.Fatal("admitted work before orphan reconciliation")
			}
		})
	}
}

func TestCloseSerializesWithSyncAndAssignmentReaders(t *testing.T) {
	b, w, backend, d := fixture(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				_ = b.Assignments()
				_ = b.Sync(context.Background(), nil, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend})
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; _ = b.Close() }()
	close(start)
	wg.Wait()
	if e := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); e == nil || w.reserve != 0 {
		t.Fatal("closed broker admitted work")
	}
}
