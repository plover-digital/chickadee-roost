package fleet

import (
	"context"
	"time"
)

func (b *Broker) provision(ctx context.Context, index int, worker Worker, backend Backend) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a := b.assignments[index]
	if backend == nil {
		return nil
	}
	if a.Phase == Intent {
		record, err := worker.Reserve(ctx, a.ID, a.WorkerProfileID, a.Profile.Digest)
		if err != nil {
			return nil
		} // Durable intent retains capacity after ambiguous failure.
		if !matchesRecord(a, record) || record.State != "reserved" {
			return nil
		}
		b.assignments[index].VMID = record.Request.VMID
		b.assignments[index].Phase = Reserved
		if err = b.persist(); err != nil {
			return err
		}
		a = b.assignments[index]
	}
	if a.Phase == Reserved || a.Phase == SealIntent {
		b.assignments[index].Phase = SealIntent
		if err := b.persist(); err != nil {
			return err
		}
		sealed, err := worker.Seal(ctx, a.ID)
		if err != nil {
			return nil
		}
		if !matchesRecord(a, sealed) || sealed.State != "sealed" {
			return nil
		}
		b.assignments[index].Phase = JITIntent
		if err = b.persist(); err != nil {
			return err
		}
		jitCtx, cancelJIT := context.WithTimeout(ctx, 5*time.Second)
		jit, err := backend.JIT(jitCtx, a.RunnerName)
		cancelJIT()
		if err != nil || jit == "" {
			b.assignments[index].CredentialUncertain = true
			b.assignments[index].Phase = Uncertain
			return b.persist()
		}
		err = worker.Deliver(ctx, a.ID, jit)
		// No retries even if the worker consumed JIT but its acknowledgement was lost.
		if err != nil {
			b.assignments[index].CredentialUncertain = true
			b.assignments[index].Phase = Uncertain
		} else {
			b.assignments[index].Phase = Delivered
			b.observeIdle(a.ID, time.Now())
		}
		return b.persist()
	}
	return nil
}

// Drain stops a worker's admission while preserving running jobs. It never
// changes durable quota based merely on the drain acknowledgement.
func (b *Broker) Drain(ctx context.Context, worker Worker) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return worker.Drain(ctx)
}

// establishAbandonedReservation reconciles durable reservation intent without
// starting credential intent. A missing record is only a point-in-time sample:
// an earlier timed-out Reserve may still complete. The idempotent original
// reservation gives either request the same worker-owned expiry and cleanup.
func (b *Broker) establishAbandonedReservation(ctx context.Context, index int, worker Worker) error {
	a := b.assignments[index]
	record, err := worker.Reserve(ctx, a.ID, a.WorkerProfileID, a.Profile.Digest)
	if err != nil || !matchesRecord(a, record) || record.State != "reserved" {
		return nil
	}
	b.assignments[index].VMID = record.Request.VMID
	b.assignments[index].Phase = Reserved
	return b.persist()
}
