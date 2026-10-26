package fleet

import "sort"

// Rotate scopes and then labels so one continuous backlog cannot monopolize a
// shared slot. These cursors affect admission order only: all durable quota,
// resource, identity and placement checks still run before creating an intent.
// Restart resets fairness order without changing execution ownership.
func (b *Broker) admissionOrder(demands []Demand) []Demand {
	scopes := map[string][]Demand{}
	for _, d := range demands {
		key := scopeKey(d.ScopeURL)
		scopes[key] = append(scopes[key], d)
	}
	keys := make([]string, 0, len(scopes))
	for key := range scopes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	split := sort.Search(len(keys), func(i int) bool { return keys[i] > b.lastScope })
	keys = append(keys[split:], keys[:split]...)
	ordered := make([]Demand, 0, len(demands))
	for _, key := range keys {
		queues := scopes[key]
		sort.Slice(queues, func(i, j int) bool { return queues[i].QueueID < queues[j].QueueID })
		split = sort.Search(len(queues), func(i int) bool { return queues[i].QueueID > b.lastQueues[key] })
		ordered = append(ordered, queues[split:]...)
		ordered = append(ordered, queues[:split]...)
	}
	return ordered
}
