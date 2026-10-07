package fleet

import "time"

// Telemetry combines observed credential-free physical capacity with durable
// allocation intent. Running means JIT intent/delivered, never proof of a live
// process or a GitHub job. Worker coverage must accompany partial snapshots.
type Telemetry struct {
	At            time.Time `json:"at"`
	Ready         int       `json:"ready"`
	Booting       int       `json:"booting"`
	Reserved      int       `json:"reserved"`
	Running       int       `json:"running"`
	Uncertain     int       `json:"uncertain"`
	WorkersOnline int       `json:"workers_online"`
	WorkersTotal  int       `json:"workers_total"`
}

func (b *Broker) Telemetry() Telemetry { b.mu.Lock(); defer b.mu.Unlock(); return b.telemetry }
func telemetrySnapshot(at time.Time, assignments []Assignment, snapshots map[string]Inventory, total int) Telemetry {
	sample := Telemetry{At: at.UTC(), WorkersTotal: total}
	for _, snapshot := range snapshots {
		ready, booting, valid := physicalWarmCounts(snapshot)
		if !valid {
			continue
		}
		sample.WorkersOnline++
		sample.Ready += ready
		sample.Booting += booting
	}
	seen := map[string]bool{}
	for _, assignment := range assignments {
		if seen[assignment.ID] {
			continue
		}
		seen[assignment.ID] = true
		switch assignment.Phase {
		case Delivered, JITIntent:
			sample.Running++
		case Uncertain:
			sample.Uncertain++
		case Intent, Reserved, SealIntent:
			sample.Reserved++
		}
	}
	return sample
}
func physicalWarmCounts(snapshot Inventory) (int, int, bool) {
	pools := map[Profile]ProfileInventory{}
	if snapshot.Budget.MaxVMs < 1 || snapshot.Used.VMs < 0 || snapshot.Used.VMs > snapshot.Budget.MaxVMs {
		return 0, 0, false
	}
	for _, profile := range snapshot.Profiles {
		if !validProfile(profile.Profile) || profile.Ready < 0 || profile.Booting < 0 || profile.Ready > snapshot.Budget.MaxVMs || profile.Booting > snapshot.Budget.MaxVMs {
			return 0, 0, false
		}
		shape := profile.Profile
		shape.ID = ""
		previous := pools[shape]
		previous.Ready = max(previous.Ready, profile.Ready)
		previous.Booting = max(previous.Booting, profile.Booting)
		pools[shape] = previous
	}
	ready, booting := 0, 0
	for _, pool := range pools {
		ready += pool.Ready
		booting += pool.Booting
	}
	if ready+booting > snapshot.Used.VMs {
		return 0, 0, false
	}
	return ready, booting, true
}
