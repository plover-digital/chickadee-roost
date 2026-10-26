package main

import (
	"sync"
	"time"

	"github.com/plover-digital/chickadee-roost/internal/fleet"
)

// Keep one observation per queue instead of replaying a FIFO of historical
// counts after a slow reconciliation. Completion identities are events, so they
// must survive when a newer count replaces an older observation.
type demandUpdates struct {
	mu      sync.Mutex
	pending map[string]update
	ready   chan struct{}
}

func newDemandUpdates() *demandUpdates {
	return &demandUpdates{pending: make(map[string]update), ready: make(chan struct{}, 1)}
}

func (u *demandUpdates) publish(v update) {
	u.mu.Lock()
	old, exists := u.pending[v.key]
	switch {
	case !exists || v.generation > old.generation:
		v.completed = append([]string(nil), v.completed...)
		u.pending[v.key] = v
	case v.generation == old.generation:
		completed := append(old.completed, v.completed...)
		if len(completed) > 256 {
			completed = completed[len(completed)-256:]
		}
		if v.observedAt.Before(old.observedAt) {
			v = old
		}
		v.completed = completed
		u.pending[v.key] = v
	}
	u.mu.Unlock()
	select {
	case u.ready <- struct{}{}:
	default:
	}
}

func (u *demandUpdates) take() map[string]update {
	u.mu.Lock()
	defer u.mu.Unlock()
	pending := u.pending
	u.pending = make(map[string]update)
	return pending
}

func applyDemandUpdates(updates map[string]update, polls map[string]poller, generations map[string]uint64, demands map[string]fleet.Demand, revisions map[string]uint64, observed map[string]time.Time) {
	for _, v := range updates {
		p, exists := polls[v.key]
		if !exists || generations[v.key] != v.generation {
			continue
		}
		// A read begun before a newer listener observation may finish afterward.
		// Retain its completion events without restoring its stale demand count.
		stale := v.observedAt.Before(observed[v.key])
		if stale && len(v.completed) == 0 {
			continue
		}
		q := p.queue
		d := demands[v.key]
		// Completion-only metadata must retain the accepted count revision.
		// Advancing it would discard other runners' completion credits and
		// make an unchanged count look like additional work.
		if !stale {
			revisions[v.key]++
		}
		d.Revision = revisions[v.key]
		d.QueueID = q.Key
		d.ScopeURL = q.Scope
		d.Label = q.Label
		d.ScopeMax = q.ScopeMax
		if !stale {
			d.Assigned = max(0, min(v.n, q.Max))
			observed[v.key] = v.observedAt
		}
		d.CompletedRunners = append(d.CompletedRunners, v.completed...)
		if len(d.CompletedRunners) > 256 {
			d.CompletedRunners = d.CompletedRunners[len(d.CompletedRunners)-256:]
		}
		d.Profile = fleet.Profile{Digest: q.ImageDigest, Machine: q.Machine, CPUs: q.CPUs, MemoryMiB: q.MemoryMiB, DiskGiB: q.DiskGiB}
		demands[v.key] = d
	}
}
