package main

import (
	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
)

// Identity has already been checked by reload. A scope quota can change while
// the per-label advertised maximum stays the same. Publish the policy to both
// future observations and existing demand without restarting the listener.
func refreshQueuePolicy(polls map[string]poller, demands map[string]fleet.Demand, q dispatch.Queue) {
	p := polls[q.Key]
	p.queue = q
	polls[q.Key] = p
	if d, ok := demands[q.Key]; ok {
		d.ScopeMax = q.ScopeMax
		demands[q.Key] = d
	}
}
