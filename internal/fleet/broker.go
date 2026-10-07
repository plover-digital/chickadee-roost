package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("assignment not found")
var ErrUnjournaled = errors.New("worker has an unjournaled active assignment")

type Broker struct {
	mu          sync.Mutex
	dir, id     string
	limits      Limits
	lock        *os.File
	assignments []Assignment
	failed      bool
	telemetry   Telemetry
}

func (b *Broker) Assignments() []Assignment {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Assignment(nil), b.assignments...)
}
func (b *Broker) persist() error {
	if err := b.save(); err != nil {
		b.failed = true
		return errors.New("broker journal persistence failed")
	}
	return nil
}
func scopeKey(scope string) string { return strings.ToLower(strings.TrimRight(scope, "/")) }
func sameProfile(a, b Profile) bool {
	return a.Digest == b.Digest && a.Machine == b.Machine && a.CPUs == b.CPUs && a.MemoryMiB == b.MemoryMiB && a.DiskGiB == b.DiskGiB
}
func sameIdentity(a, b Identity) bool { return a == b }
func validProfile(p Profile) bool {
	return p.Digest != "" && p.Machine != "" && p.CPUs > 0 && p.MemoryMiB >= 512 && p.DiskGiB >= 4
}
func matchesRecord(a Assignment, r Record) bool {
	return sameIdentity(a.Worker, r.Request.Identity) && r.Request.AssignmentID == a.ID && r.Request.VMID != "" && (a.VMID == "" || r.Request.VMID == a.VMID) && r.Request.ProfileDigest == a.Profile.Digest && r.Request.CPUs == a.Profile.CPUs && r.Request.MemoryMiB == a.Profile.MemoryMiB && r.Request.DiskGiB == a.Profile.DiskGiB
}

// Sync reconciles every durable assignment before admitting new demand. Remote
// jobs continue independently if this broker stops. Calls are serialized; worker
// reservation/run deadlines must not depend on this context or be reset by calls.
func (b *Broker) Sync(ctx context.Context, demands []Demand, workers map[string]Worker, backends map[string]Backend) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.failed || b.lock == nil {
		return errors.New("broker is not available")
	}
	scopeLimits := map[string]int{}
	eligibleDemand := map[string]Demand{}
	seen := map[string]bool{}
	for _, d := range demands {
		if d.QueueID == "" || d.ScopeURL == "" || d.Label == "" || d.Assigned < 0 || !validProfile(d.Profile) || seen[d.QueueID] || d.Revision == 0 || d.ScopeMax < 0 || d.ScopeMax > 32 || len(d.CompletedRunners) > 4096 {
			return errors.New("invalid fleet demand")
		}
		for _, name := range d.CompletedRunners {
			if len(name) > 128 {
				return errors.New("invalid runner completion metadata")
			}
		}
		seen[d.QueueID] = true
		if d.Assigned > 0 {
			eligibleDemand[d.QueueID] = d
		}
		max := d.ScopeMax
		if max == 0 {
			max = 1
		}
		scope := scopeKey(d.ScopeURL)
		if old, ok := scopeLimits[scope]; !ok || max < old {
			scopeLimits[scope] = max
		}
	}
	changed := false
	for _, d := range demands {
		for i, a := range b.assignments {
			if a.QueueID != d.QueueID {
				continue
			}
			for _, name := range d.CompletedRunners {
				if name == a.RunnerName && (a.GitHubCompletedRevision == 0 || d.Revision < a.GitHubCompletedRevision) {
					b.assignments[i].GitHubCompletedRevision = d.Revision
					changed = true
					break
				}
			}
		}
	}

	for i, a := range b.assignments {
		if a.Phase == Complete {
			if a.CredentialUncertain && time.Since(a.CompletedAt) <= 10*time.Minute && time.Since(a.RegistrationCheckedAt) >= 30*time.Second {
				if backend := backends[a.QueueID]; backend != nil {
					_ = backend.Remove(ctx, a.RunnerName)
					b.assignments[i].RegistrationCheckedAt = time.Now().UTC()
					if err := b.persist(); err != nil {
						return err
					}
				}
			}
			continue
		}
		for _, d := range demands {
			if d.QueueID == a.QueueID && d.Revision > a.LastDemandRevision {
				b.assignments[i].LastDemandRevision = d.Revision
				changed = true
			}
		}
	}
	if changed {
		if err := b.persist(); err != nil {
			return err
		}
	}
	inventory := map[string]Inventory{}
	observed := map[string]Inventory{}
	defer func() { b.telemetry = telemetrySnapshot(time.Now(), b.assignments, observed, len(workers)) }()
	ids := []string{}
	for id := range workers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, z := placementPriority(workers[ids[i]]), placementPriority(workers[ids[j]])
		if a != z {
			return a > z
		}
		return ids[i] < ids[j]
	})
	known := map[string]Assignment{}
	for _, a := range b.assignments {
		known[a.ID] = a
	}
	for _, id := range ids {
		snapshot, err := workers[id].Inventory(ctx)
		if err != nil {
			continue
		}
		if snapshot.Identity.WorkerID != id || snapshot.Identity.BrokerID != b.id || snapshot.Identity.Generation == 0 || snapshot.Budget.MaxVMs < 1 || snapshot.Budget.MaxCPUs < 1 || snapshot.Budget.MaxMemoryMiB < 512 || snapshot.Used.VMs < 0 || snapshot.Used.CPUs < 0 || snapshot.Used.MemoryMiB < 0 || snapshot.Used.VMs > snapshot.Budget.MaxVMs || snapshot.Used.CPUs > snapshot.Budget.MaxCPUs || snapshot.Used.MemoryMiB > snapshot.Budget.MaxMemoryMiB {
			continue
		}
		observed[id] = snapshot
		for _, record := range snapshot.Records {
			if record.Request.Identity.BrokerID == b.id && record.State != "terminal" {
				assignment, exists := known[record.Request.AssignmentID]
				if !exists || assignment.Phase == Complete || assignment.Worker.WorkerID != id || !matchesRecord(assignment, record) {
					return ErrUnjournaled
				}
			}
		}
		inventory[id] = snapshot
	}
	for i := range b.assignments {
		a := b.assignments[i]
		if a.Phase == Complete {
			continue
		}
		worker := workers[a.Worker.WorkerID]
		snapshot, ok := inventory[a.Worker.WorkerID]
		if worker == nil || !ok || !sameIdentity(snapshot.Identity, a.Worker) {
			continue
		}
		record, err := worker.Status(ctx, a.ID)
		if err != nil {
			// Reserve is idempotent and has no credentials; replay only an authenticated
			// definitive absence, never an unknown status or a credential intent.
			if current, eligible := eligibleDemand[a.QueueID]; errors.Is(err, ErrNotFound) && a.Phase == Intent && eligible && sameProfile(a.Profile, current.Profile) && scopeKey(a.ScopeURL) == scopeKey(current.ScopeURL) && a.Label == current.Label {
				if err = b.provision(ctx, i, worker, backends[a.QueueID]); err != nil {
					return err
				}
			}
			continue
		}
		if !matchesRecord(a, record) {
			continue
		}
		b.assignments[i].VMID = record.Request.VMID
		if record.State == "terminal" {
			if b.assignments[i].CompletedAt.IsZero() {
				completed := record.CompletedAt
				if completed.IsZero() {
					completed = time.Now().UTC()
				}
				if completed.Before(a.ReservedAt) || completed.Sub(a.ReservedAt) > 25*time.Hour {
					continue
				}
				b.assignments[i].CompletedAt = completed.UTC()
			}
			if a.Phase == JITIntent {
				b.assignments[i].CredentialUncertain = true
			}
			b.assignments[i].Phase = Terminal
			if err = b.persist(); err != nil {
				return err
			}
			if backend := backends[a.QueueID]; backend != nil && backend.Remove(ctx, a.RunnerName) == nil {
				b.assignments[i].RegistrationCheckedAt = time.Now().UTC()
				b.assignments[i].Phase = Complete
				if err = b.persist(); err != nil {
					return err
				}
			}
			continue
		}
		switch a.Phase {
		case Intent:
			current, eligible := eligibleDemand[a.QueueID]
			if !eligible || !sameProfile(a.Profile, current.Profile) || scopeKey(a.ScopeURL) != scopeKey(current.ScopeURL) || a.Label != current.Label {
				continue
			}
			if record.State == "reserved" {
				b.assignments[i].Phase = Reserved
				if err = b.persist(); err != nil {
					return err
				}
				if err = b.provision(ctx, i, worker, backends[a.QueueID]); err != nil {
					return err
				}
			}
		case Reserved, SealIntent:
			current, eligible := eligibleDemand[a.QueueID]
			if !eligible || !sameProfile(a.Profile, current.Profile) || scopeKey(a.ScopeURL) != scopeKey(current.ScopeURL) || a.Label != current.Label {
				continue
			}
			if err = b.provision(ctx, i, worker, backends[a.QueueID]); err != nil {
				return err
			}
		case JITIntent:
			// A crash may have lost either the JIT API result or delivery acknowledgement.
			// Never request or deliver another credential. Worker timeout retires the VM.
			b.assignments[i].CredentialUncertain = true
			b.assignments[i].Phase = Uncertain
			if err = b.persist(); err != nil {
				return err
			}
		}
	}
	ordered := append([]Demand(nil), demands...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].QueueID < ordered[j].QueueID })
	for _, d := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Assigned == 0 || backends[d.QueueID] == nil || b.scopeCount(d.ScopeURL) >= scopeLimits[scopeKey(d.ScopeURL)] || b.queueCount(d.QueueID)+b.completedCredits(d.QueueID, d.Revision) >= d.Assigned || !b.globalCapacity(d.Profile) {
			continue
		}
		choice, profile, ok := b.place(d.Profile, inventory, ids)
		if !ok {
			continue
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := hex.EncodeToString(random[:])
		a := Assignment{ID: id, RunnerName: "chickadee-" + id, QueueID: d.QueueID, ScopeURL: scopeKey(d.ScopeURL), Label: d.Label, Profile: d.Profile, Worker: inventory[choice].Identity, WorkerProfileID: profile.ID, Phase: Intent, ReservedAt: time.Now().UTC(), DemandRevision: d.Revision, LastDemandRevision: d.Revision}
		if len(b.assignments) >= 10000 {
			return errors.New("broker assignment capacity reached")
		}
		b.assignments = append(b.assignments, a)
		if err := b.persist(); err != nil {
			return err
		}
		if err := b.provision(ctx, len(b.assignments)-1, workers[choice], backends[d.QueueID]); err != nil {
			return err
		}
		// Refresh after mutation; never place from an earlier free-capacity snapshot.
		fresh, err := workers[choice].Inventory(ctx)
		if err != nil || !sameIdentity(fresh.Identity, a.Worker) {
			delete(inventory, choice)
			delete(observed, choice)
		} else {
			inventory[choice] = fresh
			observed[choice] = fresh
		}
	}
	return nil
}
func (b *Broker) scopeBusy(scope string) bool {
	for _, a := range b.assignments {
		if a.Phase != Complete && scopeKey(a.ScopeURL) == scopeKey(scope) {
			return true
		}
	}
	return false
}
func (b *Broker) globalCapacity(p Profile) bool {
	v, c, m := 0, 0, 0
	for _, a := range b.assignments {
		if a.Phase != Complete {
			v++
			c += a.Profile.CPUs
			m += a.Profile.MemoryMiB
		}
	}
	return v+1 <= b.limits.MaxVMs && c+p.CPUs <= b.limits.MaxCPUs && m+p.MemoryMiB <= b.limits.MaxMemoryMiB
}
func (b *Broker) place(p Profile, snapshots map[string]Inventory, ids []string) (string, Profile, bool) {
	// Prefer a compatible prebooted guest across all eligible hosts before cold boot.
	for _, warm := range []bool{true, false} {
		for _, id := range ids {
			s, ok := snapshots[id]
			if !ok || s.Draining {
				continue
			}
			for _, profile := range s.Profiles {
				if !sameProfile(profile.Profile, p) {
					continue
				}
				if warm && profile.Ready > 0 {
					return id, profile.Profile, true
				}
				if !warm && workerCapacity(s, p) {
					return id, profile.Profile, true
				}
			}
		}
	}
	return "", Profile{}, false
}
func workerCapacity(s Inventory, p Profile) bool {
	u := s.Used
	if u.VMs < 0 || u.CPUs < 0 || u.MemoryMiB < 0 {
		return false
	}
	fits := func(v CapacityUsed) bool {
		return v.VMs+1 <= s.Budget.MaxVMs && v.CPUs+p.CPUs <= s.Budget.MaxCPUs && v.MemoryMiB+p.MemoryMiB <= s.Budget.MaxMemoryMiB
	}
	if fits(u) {
		return true
	}
	// This is placement eligibility, not resource release. The worker must retire
	// these credential-free READY guests and confirm exit/disk cleanup before
	// allocating the cold VM. Booting and journaled reservations are never reclaimed.
	reclaim := map[Profile]int{}
	for _, available := range s.Profiles {
		if available.Ready < 0 || available.Booting < 0 {
			return false
		}
		if available.Ready == 0 || sameProfile(available.Profile, p) || !validProfile(available.Profile) {
			continue
		}
		key := available.Profile
		key.ID = "" // Aliases can describe the same physical pool.
		if available.Ready > reclaim[key] {
			reclaim[key] = available.Ready
		}
	}
	for profile, count := range reclaim {
		u.VMs -= count
		u.CPUs -= count * profile.CPUs
		u.MemoryMiB -= count * profile.MemoryMiB
	}
	if u.VMs < 0 || u.CPUs < 0 || u.MemoryMiB < 0 {
		return false
	}
	return fits(u)
}
func (b *Broker) scopeCount(scope string) int {
	n := 0
	for _, a := range b.assignments {
		if a.Phase != Complete && scopeKey(a.ScopeURL) == scopeKey(scope) {
			n++
		}
	}
	return n
}
func (b *Broker) queueCount(queue string) int {
	n := 0
	for _, a := range b.assignments {
		if a.Phase != Complete && a.QueueID == queue {
			n++
		}
	}
	return n
}

func (b *Broker) completedCredits(queue string, revision uint64) int {
	n := 0
	for _, a := range b.assignments {
		if a.Phase == Complete && a.QueueID == queue && a.DemandRevision <= revision && a.LastDemandRevision >= revision && (a.GitHubCompletedRevision == 0 || a.GitHubCompletedRevision > revision) {
			n++
		}
	}
	return n
}

// Optional metadata is supplied by the broker adapter, never host inventory.
func placementPriority(w Worker) int {
	if p, ok := w.(interface{ PlacementPriority() int }); ok {
		return p.PlacementPriority()
	}
	return 0
}
