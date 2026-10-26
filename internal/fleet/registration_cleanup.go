package fleet

import (
	"context"
	"sort"
	"time"
)

const maxRegistrationChecks = 2

type registrationCheck struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Results are consumed only by Sync under the broker lock. Worker-established
// terminal cleanup remains durable before any registration request is queued.
func (b *Broker) finishRegistrationChecks() error {
	changed := false
	for id, check := range b.registrationChecks {
		select {
		case <-check.done:
			check.cancel()
			delete(b.registrationChecks, id)
			if check.err == nil {
				for i, a := range b.assignments {
					if a.ID == id && a.Phase == Terminal {
						b.assignments[i].Phase = Complete
						changed = true
						break
					}
				}
			}
		default:
		}
	}
	if changed {
		return b.persist()
	}
	return nil
}

// Registration cleanup must not hold up inventory reconciliation or another
// queue. Terminal executions take priority over delayed-registration polling.
// Backends must honor cancellation independently of their listener/JIT client.
func (b *Broker) scheduleRegistrationChecks(ctx context.Context, backends map[string]Backend, now time.Time) error {
	candidates := []int{}
	for i, a := range b.assignments {
		if a.Phase == Terminal || a.Phase == Complete && a.CredentialUncertain && now.Sub(a.CompletedAt) <= 10*time.Minute {
			candidates = append(candidates, i)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, z := b.assignments[candidates[i]], b.assignments[candidates[j]]
		if (a.Phase == Terminal) != (z.Phase == Terminal) {
			return a.Phase == Terminal
		}
		if !a.RegistrationCheckedAt.Equal(z.RegistrationCheckedAt) {
			return a.RegistrationCheckedAt.Before(z.RegistrationCheckedAt)
		}
		return a.ID < z.ID
	})
	for _, index := range candidates {
		a := b.assignments[index]
		backend := backends[a.QueueID]
		if backend == nil || now.Sub(a.RegistrationCheckedAt) < idleRetirementRetry {
			continue
		}
		if _, ok := b.registrationChecks[a.ID]; ok {
			continue
		}
		if len(b.registrationChecks) >= maxRegistrationChecks {
			break
		}
		b.assignments[index].RegistrationCheckedAt = now.UTC()
		if err := b.persist(); err != nil {
			return err
		}
		requestCtx, cancel := context.WithTimeout(ctx, idleRetirementTimeout)
		check := &registrationCheck{cancel: cancel, done: make(chan struct{})}
		if b.registrationChecks == nil {
			b.registrationChecks = make(map[string]*registrationCheck)
		}
		b.registrationChecks[a.ID] = check
		go func(name string) {
			defer close(check.done)
			defer cancel()
			check.err = backend.Remove(requestCtx, name)
		}(a.RunnerName)
	}
	return nil
}
