package fleet

import (
	"context"
	"time"
)

const (
	idleRetirementGrace   = 2 * time.Minute
	idleRetirementRetry   = 30 * time.Second
	idleRetirementTimeout = 5 * time.Second
)

// idleRetirer must use GitHub's ordinary, server-guarded deregistration. It must
// never force removal or cancel an executing job. Busy/unknown results are errors;
// success only means deregistered/already absent, never that the worker exited.
// Backends without this capability retain the existing job-timeout fallback.
type idleRetirer interface {
	RetireIdle(context.Context, string) error
}

func (b *Broker) retireIdle(ctx context.Context, index int, backend Backend, now time.Time) error {
	a := b.assignments[index]
	retirer, ok := backend.(idleRetirer)
	if !ok || a.CredentialUncertain || now.Sub(a.ReservedAt) < idleRetirementGrace || now.Sub(a.RegistrationCheckedAt) < idleRetirementRetry {
		return nil
	}
	// Persist the attempt before the request: a restart or lost response must not
	// cause an unbounded retry loop. Keep the same phase/VM/credential intent.
	b.assignments[index].RegistrationCheckedAt = now.UTC()
	if err := b.persist(); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, idleRetirementTimeout)
	defer cancel()
	_ = retirer.RetireIdle(requestCtx, a.RunnerName)
	// GitHub refuses a busy runner. On success, the listener observes its removal
	// and exits through the normal worker lifecycle. Every outcome keeps quota
	// until authenticated terminal worker status and registration cleanup.
	return nil
}
