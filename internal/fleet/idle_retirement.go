package fleet

import (
	"context"
	"time"
)

const (
	idleRetirementGrace   = 2 * time.Minute
	idleRetirementRetry   = 30 * time.Second
	idleRetirementTimeout = 5 * time.Second
	maxIdleChecks         = 2
)

// idleRetirer must use GitHub's ordinary, server-guarded deregistration. It must
// never force removal or cancel an executing job. Its independent API client
// must honor cancellation without waiting on another operation's mutex.
// Success means deregistered/already absent, never that the worker exited.
type idleRetirer interface {
	RetireIdle(context.Context, string) error
}

type idleCheck struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (b *Broker) observeIdle(id string, now time.Time) {
	if b.idleSince == nil {
		b.idleSince = make(map[string]time.Time)
	}
	if _, ok := b.idleSince[id]; !ok {
		b.idleSince[id] = now
	}
}

func (b *Broker) retireIdle(ctx context.Context, index int, backend Backend, now time.Time) error {
	a := b.assignments[index]
	retirer, ok := backend.(idleRetirer)
	if !ok || a.CredentialUncertain {
		return nil
	}
	// Provisioning/boot time must not consume the listener's grace. Delivery ACK
	// starts this observation; recovery conservatively grants a new grace period
	// after the first authenticated matching worker status. No journal migration.
	b.observeIdle(a.ID, now)
	if now.Sub(b.idleSince[a.ID]) < idleRetirementGrace || now.Sub(a.RegistrationCheckedAt) < idleRetirementRetry {
		return nil
	}
	for id, check := range b.idleChecks {
		select {
		case <-check.done:
			check.cancel()
			delete(b.idleChecks, id)
		default:
		}
	}
	if _, pending := b.idleChecks[a.ID]; pending || len(b.idleChecks) >= maxIdleChecks {
		return nil
	}
	// Persist before enqueueing, retaining ownership/quota and bounding retries
	// across restart or an ambiguous response. The background result never changes
	// assignment phase: only normal authenticated worker cleanup releases quota.
	b.assignments[index].RegistrationCheckedAt = now.UTC()
	if err := b.persist(); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, idleRetirementTimeout)
	check := idleCheck{cancel: cancel, done: make(chan struct{})}
	if b.idleChecks == nil {
		b.idleChecks = make(map[string]idleCheck)
	}
	b.idleChecks[a.ID] = check
	go func() {
		defer close(check.done)
		defer cancel()
		_ = retirer.RetireIdle(requestCtx, a.RunnerName)
	}()
	return nil
}
