package dispatch

import (
	"context"
	"errors"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
	"github.com/plover-digital/chickadee/workerapi"
	"time"
)

// RemoteWorker maps only the public versioned protocol into service-owned DTOs.
// Roost never imports the host's internal VM/pool/worker implementation.
type RemoteWorker struct {
	Client   *workerapi.Client
	Priority int
}

// PlacementPriority is set by private broker configuration, not the remote host.
func (w RemoteWorker) PlacementPriority() int { return w.Priority }

func record(r workerapi.Record) fleet.Record {
	q := r.Request
	return fleet.Record{Request: fleet.Request{Identity: fleet.Identity{WorkerID: q.Identity.WorkerID, BrokerID: q.Identity.BrokerID, Generation: q.Identity.Generation}, AssignmentID: q.AssignmentID, VMID: q.VMID, ProfileDigest: q.ProfileDigest, CPUs: q.CPUs, MemoryMiB: q.MemoryMiB, DiskGiB: q.DiskGiB}, State: r.State, CompletedAt: r.CompletedAt}
}
func (w RemoteWorker) Inventory(ctx context.Context) (fleet.Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s, e := w.Client.Inventory(ctx)
	if e != nil {
		return fleet.Inventory{}, e
	}
	v := fleet.Inventory{Identity: fleet.Identity{WorkerID: s.Identity.WorkerID, BrokerID: s.Identity.BrokerID, Generation: s.Identity.Generation}, Draining: s.Draining, Used: fleet.CapacityUsed{VMs: s.Used.VMs, CPUs: s.Used.CPUs, MemoryMiB: s.Used.MemoryMiB}, Budget: fleet.Budget{MaxVMs: s.Budget.MaxVMs, MaxCPUs: s.Budget.MaxCPUs, MaxMemoryMiB: s.Budget.MaxMemoryMiB}}
	for _, p := range s.Profiles {
		v.Profiles = append(v.Profiles, fleet.ProfileInventory{Profile: fleet.Profile{ID: p.ID, Digest: p.Digest, Machine: p.Machine, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB, DiskGiB: p.DiskGiB}, Ready: p.Ready, Booting: p.Booting})
	}
	for _, r := range s.Records {
		v.Records = append(v.Records, record(r))
	}
	return v, nil
}
func (w RemoteWorker) Reserve(ctx context.Context, id, profile, digest string) (fleet.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, e := w.Client.Reserve(ctx, id, profile, digest)
	return record(r), e
}
func (w RemoteWorker) Seal(ctx context.Context, id string) (fleet.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, e := w.Client.Seal(ctx, id)
	return record(r), e
}
func (w RemoteWorker) Deliver(ctx context.Context, id, jit string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return w.Client.Deliver(ctx, id, jit)
}
func (w RemoteWorker) Status(ctx context.Context, id string) (fleet.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r, e := w.Client.Status(ctx, id)
	if errors.Is(e, workerapi.ErrNotFound) {
		return fleet.Record{}, fleet.ErrNotFound
	}
	return record(r), e
}
func (w RemoteWorker) Drain(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return w.Client.Drain(ctx)
}
