package fleet

// This opt-in experiment drives the real Broker.Sync, placement, admission,
// resource checks, journals and asynchronous registration cleanup. Only workers
// and GitHub are simulated. It never starts QEMU or connects to a service.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

type simJob struct {
	queue                         int
	arrival, duration, start, end int
	host                          string
}
type simQueue struct {
	demand    Demand
	pending   []*simJob
	running   int
	completed []string
}
type simBoot struct {
	profile Profile
	due     int
}
type simWorker struct {
	model      *simModel
	inventory  Inventory
	priority   int
	speed      float64
	records    map[string]Record
	jobs       map[string]*simJob
	ready      map[string]int
	boots      []simBoot
	placements int
}
type simBackend struct{ queue int }

func (s simBackend) JIT(context.Context, string) (string, error) { return fmt.Sprint(s.queue), nil }
func (simBackend) Remove(context.Context, string) error          { return nil }
func (w *simWorker) PlacementPriority() int {
	switch w.model.policy {
	case "spread":
		used := 0
		for _, r := range w.records {
			if r.State != "terminal" {
				used += r.Request.CPUs
			}
		}
		return -used*10000/w.inventory.Budget.MaxCPUs + w.priority
	case "round-robin":
		return -w.placements*100 + w.priority
	}
	return w.priority
}
func (w *simWorker) used() CapacityUsed {
	var u CapacityUsed
	add := func(p Profile, n int) { u.VMs += n; u.CPUs += p.CPUs * n; u.MemoryMiB += p.MemoryMiB * n }
	for _, p := range w.model.profiles {
		add(p, w.ready[p.ID])
	}
	for _, b := range w.boots {
		add(b.profile, 1)
	}
	for _, r := range w.records {
		if r.State != "terminal" {
			u.VMs++
			u.CPUs += r.Request.CPUs
			u.MemoryMiB += r.Request.MemoryMiB
		}
	}
	return u
}
func (w *simWorker) Inventory(context.Context) (Inventory, error) {
	if w.model.offline(w) {
		return Inventory{}, errors.New("simulated offline")
	}
	s := w.inventory
	s.Used = w.used()
	s.Profiles = nil
	s.Records = nil
	s.Draining = w.model.scenario == "drain" && w.inventory.Identity.WorkerID == "worker-00" && w.model.tick >= 100
	for _, p := range w.model.profiles {
		n := 0
		for _, b := range w.boots {
			if b.profile.ID == p.ID {
				n++
			}
		}
		s.Profiles = append(s.Profiles, ProfileInventory{Profile: p, Ready: w.ready[p.ID], Booting: n})
	}
	for _, r := range w.records {
		s.Records = append(s.Records, r)
	}
	return s, nil
}
func (w *simWorker) fits(p Profile) bool {
	u := w.used()
	b := w.inventory.Budget
	return u.VMs+1 <= b.MaxVMs && u.CPUs+p.CPUs <= b.MaxCPUs && u.MemoryMiB+p.MemoryMiB <= b.MaxMemoryMiB
}
func (w *simWorker) Reserve(_ context.Context, id, profile, digest string) (Record, error) {
	if r, ok := w.records[id]; ok {
		return r, nil
	}
	p := w.model.profile(profile)
	if p.Digest != digest {
		return Record{}, errors.New("wrong digest")
	}
	if w.ready[profile] > 0 {
		w.ready[profile]--
		r := Record{Request: Request{Identity: w.inventory.Identity, AssignmentID: id, VMID: "sim-" + id, ProfileDigest: digest, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB, DiskGiB: p.DiskGiB}, State: "reserved"}
		w.records[id] = r
		return r, nil
	}
	for _, b := range w.boots {
		if b.profile.ID == profile {
			return Record{}, context.DeadlineExceeded
		}
	}
	// Like the worker, reclaim only incompatible credential-free READY guests.
	if !w.fits(p) {
		for name := range w.ready {
			if name != profile {
				w.ready[name] = 0
			}
		}
	}
	if w.fits(p) {
		w.boots = append(w.boots, simBoot{p, w.model.tick + w.model.bootSeconds})
	}
	return Record{}, context.DeadlineExceeded
}
func (w *simWorker) Seal(_ context.Context, id string) (Record, error) {
	r := w.records[id]
	r.State = "sealed"
	w.records[id] = r
	return r, nil
}
func (w *simWorker) Deliver(_ context.Context, id, token string) error {
	var qi int
	if _, err := fmt.Sscan(token, &qi); err != nil {
		return err
	}
	q := w.model.queues[qi]
	if len(q.pending) == 0 {
		return errors.New("no simulated job")
	}
	job := q.pending[0]
	if job.start >= 0 {
		return errors.New("job was already started")
	}
	q.pending = q.pending[1:]
	q.running++
	job.start = w.model.tick
	job.end = job.start + int(math.Ceil(float64(job.duration)/w.speed))
	job.host = w.inventory.Identity.WorkerID
	w.jobs[id] = job
	w.placements++
	r := w.records[id]
	r.State = "delivery_intent"
	w.records[id] = r
	return nil
}
func (w *simWorker) Status(_ context.Context, id string) (Record, error) {
	if w.model.offline(w) {
		return Record{}, errors.New("simulated offline")
	}
	r, ok := w.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}
func (w *simWorker) Drain(context.Context) error { w.inventory.Draining = true; return nil }
func (w *simWorker) advance() {
	for id, job := range w.jobs {
		if job.end > w.model.tick {
			continue
		}
		q := w.model.queues[job.queue]
		q.running--
		q.completed = append(q.completed, "chickadee-"+id)
		r := w.records[id]
		r.State = "terminal"
		r.CompletedAt = time.Now().UTC()
		w.records[id] = r
		delete(w.jobs, id)
	}
	kept := w.boots[:0]
	for _, b := range w.boots {
		if b.due <= w.model.tick {
			w.ready[b.profile.ID]++
		} else {
			kept = append(kept, b)
		}
	}
	w.boots = kept
	if w.model.offline(w) || w.inventory.Draining {
		return
	}
	// Counterfactual: prewarm from arrived, quota-eligible pending demand rather
	// than retaining a fixed target for every profile. No future arrivals or job
	// durations are consulted. Reclamation only touches credential-free READYs.
	profiles := append([]Profile(nil), w.model.profiles...)
	if w.model.policy == "adaptive-warm" {
		needed := map[string]int{}
		for _, q := range w.model.queues {
			needed[q.demand.Profile.ID] += min(len(q.pending), max(0, q.demand.ScopeMax-q.running))
		}
		total := 0
		for _, n := range needed {
			total += n
		}
		if total > 0 {
			for id := range w.ready {
				if needed[id] == 0 {
					w.ready[id] = 0
				}
			}
			sort.SliceStable(profiles, func(i, j int) bool { return needed[profiles[i].ID] > needed[profiles[j].ID] })
			keptProfiles := profiles[:0]
			for _, p := range profiles {
				if needed[p.ID] > 0 {
					keptProfiles = append(keptProfiles, p)
				}
			}
			profiles = keptProfiles
		}
	}
	// Default: one shared small target; mixed-profile runs model both sizes.
	for _, p := range profiles {
		booting := false
		for _, b := range w.boots {
			if b.profile.ID == p.ID {
				booting = true
			}
		}
		if w.ready[p.ID] == 0 && !booting && w.fits(p) {
			w.boots = append(w.boots, simBoot{p, w.model.tick + w.model.bootSeconds})
		}
	}
}

type simModel struct {
	tick, bootSeconds int
	scenario, policy  string
	profiles          []Profile
	queues            []*simQueue
	hosts             []*simWorker
	jobs              []*simJob
}

func (s *simModel) profile(id string) Profile {
	for _, p := range s.profiles {
		if p.ID == id {
			return p
		}
	}
	panic("unknown simulated profile")
}
func (s *simModel) offline(w *simWorker) bool {
	return s.scenario == "outage" && w.inventory.Identity.WorkerID == "worker-00" && s.tick >= 100 && s.tick < 200
}

type simResult struct {
	ScopeBlockedJobSeconds    int            `json:"scope_blocked_job_s"`
	ResourceBlockedJobSeconds int            `json:"resource_blocked_job_s"`
	Seed                      int64          `json:"seed"`
	EarlyScopeAdmissions      map[string]int `json:"first_100_scope_admissions"`
	PerScopeP95               map[string]int `json:"scope_p95_wait_s"`
	PlaceableQueueSeconds     int            `json:"queued_while_placeable_queue_s"`
	PeakRunning               int            `json:"peak_running"`
	Scenario                  string         `json:"scenario"`
	Policy                    string         `json:"policy"`
	Jobs                      int            `json:"jobs"`
	Finish                    int            `json:"finish_s"`
	P50, P95, Max             int
	PerHost                   map[string]int `json:"per_host"`
	PerScope                  map[string]int `json:"per_scope"`
	PeakAllocated             int            `json:"peak_allocated"`
	WallMS                    int64          `json:"wall_ms"`
}

func simulateScheduler(t *testing.T, scenario, policy string, seed int64) simResult {
	t.Helper()
	s := &simModel{scenario: scenario, policy: policy, bootSeconds: 6}
	p := Profile{ID: "small", Digest: "simulated-ubuntu-immutable-image", Machine: "q35", CPUs: 2, MemoryMiB: 4096, DiskGiB: 48}
	s.profiles = []Profile{p}
	if scenario == "mixed" {
		m := p
		m.ID = "medium"
		m.CPUs = 4
		m.MemoryMiB = 8192
		s.profiles = append(s.profiles, m)
	}
	hosts, slots, queues := 2, 3, 8
	if scenario == "scale" {
		hosts = 16
		slots = 2
		queues = 32
	}
	if scenario == "burst" {
		queues = 1
	}
	if scenario == "label-fairness" {
		queues = 9
		slots = 3
	}
	limits := Limits{hosts * slots, hosts * slots * 2, hosts * slots * 4096}
	workers := map[string]Worker{}
	backends := map[string]Backend{}
	for i := 0; i < queues; i++ {
		profile := s.profiles[i%len(s.profiles)]
		scope := fmt.Sprintf("https://github.com/sim-scope-%02d", i)
		if scenario == "label-fairness" {
			scope = "https://github.com/sim-scope-a"
			if i == 8 {
				scope = "https://github.com/sim-scope-z"
			}
		}
		scopeMax := 1
		if scenario == "low-quota4" {
			scopeMax = 4
		}
		if scenario == "burst" || scenario == "label-fairness" {
			scopeMax = 32
		}
		q := &simQueue{demand: Demand{QueueID: fmt.Sprintf("queue-%02d", i), ScopeURL: scope, Label: fmt.Sprintf("sim-label-%02d", i), Profile: profile, Revision: 1, ScopeMax: scopeMax}}
		s.queues = append(s.queues, q)
		backends[q.demand.QueueID] = simBackend{i}
	}
	for i := 0; i < hosts; i++ {
		id := fmt.Sprintf("worker-%02d", i)
		w := &simWorker{model: s, inventory: Inventory{Identity: Identity{id, "sim-broker", 1}, Budget: Budget{slots, slots * 2, slots * 4096}}, speed: 1, ready: map[string]int{}, records: map[string]Record{}, jobs: map[string]*simJob{}}
		if i == 0 {
			w.priority = 10
		}
		if scenario == "heterogeneous" && i == 0 {
			w.speed = 1.4
		}
		for _, p := range s.profiles {
			if w.fits(p) {
				w.ready[p.ID]++
			}
		}
		s.hosts = append(s.hosts, w)
		workers[id] = w
	}
	rng := rand.New(rand.NewSource(seed))
	count := 240
	if scenario == "scale" {
		count = 1000
	}
	for i := 0; i < count; i++ {
		qi := rng.Intn(queues)
		arrival := rng.Intn(600)
		duration := 8 + rng.Intn(25)
		switch scenario {
		case "burst":
			arrival = 0
			qi = 0
			duration = 10
		case "low", "low-quota4":
			arrival = rng.Intn(2400)
		case "near-capacity", "heterogeneous", "outage", "drain", "mixed":
			arrival = rng.Intn(1200)
		case "overload":
			arrival = rng.Intn(120)
		case "scale":
			arrival = rng.Intn(600)
		case "label-fairness":
			qi = 8
			if i%2 == 0 {
				qi = (i / 2) % 8
			}
			arrival = 0
			duration = 10
		}
		s.jobs = append(s.jobs, &simJob{queue: qi, arrival: arrival, duration: duration, start: -1})
	}
	sort.SliceStable(s.jobs, func(i, j int) bool { return s.jobs[i].arrival < s.jobs[j].arrival })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	broker, err := Open(dir, "sim-broker", limits)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	result := simResult{Seed: seed, EarlyScopeAdmissions: map[string]int{}, PerScopeP95: map[string]int{}, Scenario: scenario, Policy: policy, Jobs: count, PerHost: map[string]int{}, PerScope: map[string]int{}}
	started := time.Now()
	next := 0
	seenAdmissions := 0
	for s.tick = 0; s.tick < 20000; s.tick++ {
		for next < len(s.jobs) && s.jobs[next].arrival <= s.tick {
			j := s.jobs[next]
			s.queues[j.queue].pending = append(s.queues[j.queue].pending, j)
			next++
		}
		for _, w := range s.hosts {
			w.advance()
		}
		demands := make([]Demand, 0, queues)
		for _, q := range s.queues {
			q.demand.Assigned = len(q.pending) + q.running
			q.demand.Revision++
			q.demand.CompletedRunners = q.completed
			q.completed = nil
			demands = append(demands, q.demand)
		}
		for {
			before := len(broker.assignments)
			if err := broker.Sync(context.Background(), demands, workers, backends); err != nil {
				t.Fatal(err)
			}
			// Counterfactual: immediately run another bounded admission pass instead
			// of waiting for the next one-second tick. Same broker and all safety
			// checks; this does not simulate real network/fsync costs.
			if policy != "eager-fill" || len(broker.assignments) == before {
				break
			}
		}
		// Deterministically finish zero-latency fake registration RPCs before the next
		// virtual tick. Their result is still consumed only by the next real Sync.
		for _, check := range broker.registrationChecks {
			select {
			case <-check.done:
			case <-time.After(time.Second):
				t.Fatal("simulated cleanup stuck")
			}
		}
		allocated := 0
		usedCPU, usedMem := 0, 0
		scopeCounts := map[string]int{}
		for _, a := range broker.assignments {
			if a.Phase != Complete {
				allocated++
				usedCPU += a.Profile.CPUs
				usedMem += a.Profile.MemoryMiB
				scopeCounts[a.ScopeURL]++
			}
		}
		if allocated > limits.MaxVMs || usedCPU > limits.MaxCPUs || usedMem > limits.MaxMemoryMiB {
			t.Fatal("global resource cap exceeded")
		}
		for _, q := range s.queues {
			if scopeCounts[q.demand.ScopeURL] > q.demand.ScopeMax {
				t.Fatal("scope quota exceeded")
			}
		}
		result.PeakAllocated = max(result.PeakAllocated, allocated)
		for seenAdmissions < len(broker.assignments) {
			if seenAdmissions < 100 {
				result.EarlyScopeAdmissions[broker.assignments[seenAdmissions].ScopeURL]++
			}
			seenAdmissions++
		}
		running := 0
		for _, w := range s.hosts {
			running += len(w.jobs)
		}
		result.PeakRunning = max(result.PeakRunning, running)
		// Diagnostic only: does an unmet queue still fit authenticated inventory and
		// both global/scope quotas at the end of this pass? Does not speculate over
		// unobserved reservations, completion credits or worker-owned booting slots.
		ids := make([]string, 0, len(workers))
		snapshots := map[string]Inventory{}
		for id, w := range workers {
			ids = append(ids, id)
			v, e := w.Inventory(context.Background())
			if e == nil && !broker.hasUnobservedAssignment(v) {
				snapshots[id] = v
			}
		}
		sort.Strings(ids)
		for _, q := range s.queues {
			d := q.demand
			if len(q.pending) > 0 && broker.scopeCount(d.ScopeURL) >= d.ScopeMax {
				result.ScopeBlockedJobSeconds += len(q.pending)
			}
			if len(q.pending) > 0 && broker.scopeCount(d.ScopeURL) < d.ScopeMax {
				_, _, fits := broker.place(d.Profile, snapshots, ids)
				if !broker.globalCapacity(d.Profile) || !fits {
					result.ResourceBlockedJobSeconds += len(q.pending)
				}
			}
			if len(q.pending) > 0 && broker.scopeCount(d.ScopeURL) < d.ScopeMax && broker.queueCount(d.QueueID)+broker.completedCredits(d.QueueID, d.Revision) < d.Assigned && broker.globalCapacity(d.Profile) {
				if _, _, ok := broker.place(d.Profile, snapshots, ids); ok {
					result.PlaceableQueueSeconds++
				}
			}
		}
		for _, w := range s.hosts {
			u := w.used()
			b := w.inventory.Budget
			if u.VMs > b.MaxVMs || u.CPUs > b.MaxCPUs || u.MemoryMiB > b.MaxMemoryMiB {
				t.Fatal("worker resource cap exceeded")
			}
		}
		if next == count && allocated == 0 {
			done := true
			for _, q := range s.queues {
				if len(q.pending) > 0 || q.running > 0 {
					done = false
				}
			}
			if done {
				break
			}
		}
	}
	if s.tick == 20000 {
		t.Fatal("simulation did not converge")
	}
	waits := make([]int, 0, count)
	for _, j := range s.jobs {
		if j.start < j.arrival {
			t.Fatal("job missing or started before arrival")
		}
		waits = append(waits, j.start-j.arrival)
		result.PerHost[j.host]++
		result.PerScope[s.queues[j.queue].demand.ScopeURL]++
	}
	for _, q := range s.queues {
		scope := q.demand.ScopeURL
		if _, ok := result.PerScopeP95[scope]; ok {
			continue
		}
		var sw []int
		for _, j := range s.jobs {
			if s.queues[j.queue].demand.ScopeURL == scope {
				sw = append(sw, j.start-j.arrival)
			}
		}
		sort.Ints(sw)
		if len(sw) > 0 {
			result.PerScopeP95[scope] = sw[int(math.Ceil(.95*float64(len(sw))))-1]
		}
	}
	sort.Ints(waits)
	result.P50 = waits[(len(waits)-1)/2]
	result.P95 = waits[int(math.Ceil(.95*float64(len(waits))))-1]
	result.Max = waits[len(waits)-1]
	result.Finish = s.tick
	result.WallMS = time.Since(started).Milliseconds()
	if len(broker.assignments) != count {
		t.Fatalf("duplicate or missing runner: %d != %d", len(broker.assignments), count)
	}
	return result
}
func TestSchedulerSimulation(t *testing.T) {
	if os.Getenv("CHICKADEE_SCHEDULER_SIM") == "" {
		t.Skip("set CHICKADEE_SCHEDULER_SIM=1 to run offline scheduling experiment")
	}
	seed := int64(42)
	if v := os.Getenv("CHICKADEE_SCHEDULER_SIM_SEED"); v != "" {
		var err error
		seed, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	var results []simResult
	for _, scenario := range []string{"low", "low-quota4", "near-capacity", "overload", "burst", "heterogeneous", "outage", "drain", "mixed", "label-fairness", "scale"} {
		for _, policy := range []string{"current", "spread", "round-robin", "adaptive-warm", "eager-fill"} {
			t.Run(scenario+"/"+policy, func(t *testing.T) {
				r := simulateScheduler(t, scenario, policy, seed)
				results = append(results, r)
				t.Logf("%s/%s jobs=%d p50=%ds p95=%ds max=%ds finish=%ds hosts=%v wall=%dms", r.Scenario, r.Policy, r.Jobs, r.P50, r.P95, r.Max, r.Finish, r.PerHost, r.WallMS)
			})
		}
	}
	if path := os.Getenv("CHICKADEE_SCHEDULER_SIM_OUT"); path != "" {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Clean(path), append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Characterize the outstanding serial-RPC limitation using channels, not a
// timing threshold: even a healthy ready worker waits for unrelated inventory.
type simBlockedInventory struct {
	*fakeWorker
	entered chan struct{}
	release chan struct{}
}

func (w simBlockedInventory) Inventory(ctx context.Context) (Inventory, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	select {
	case <-w.release:
		return Inventory{}, errors.New("simulated unavailable")
	case <-ctx.Done():
		return Inventory{}, ctx.Err()
	}
}
func TestSchedulerSlowInventoryCharacterization(t *testing.T) {
	if os.Getenv("CHICKADEE_SCHEDULER_SIM") == "" {
		t.Skip("opt-in scheduler characterization")
	}
	b, w, backend, d := fixture(t)
	w.inventory.Identity.WorkerID = "worker-z"
	blocked := simBlockedInventory{fakeWorker: w, entered: make(chan struct{}, 1), release: make(chan struct{})}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		done <- b.Sync(ctx, []Demand{d}, map[string]Worker{"worker-a": blocked, "worker-z": w}, map[string]Backend{d.QueueID: backend})
	}()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("inventory call did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("Sync unexpectedly bypassed serial inventory: %v", err)
	default:
	}
	close(blocked.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("healthy worker did not progress after release")
	}
	if len(b.Assignments()) != 1 || b.Assignments()[0].Worker.WorkerID != "worker-z" {
		t.Fatal("healthy idle worker did not receive demand")
	}
}

func TestSchedulerManyLabelsCharacterization(t *testing.T) {
	if os.Getenv("CHICKADEE_SCHEDULER_SIM") == "" {
		t.Skip("opt-in scheduler characterization")
	}
	b, w, _, base := fixture(t)
	backend := simBackend{}
	b.limits = Limits{6, 24, 49152}
	w.inventory.Budget = Budget{6, 24, 49152}
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{}
	var demands []Demand
	for i := 0; i < 9; i++ {
		d := base
		d.QueueID = fmt.Sprintf("label-%d", i)
		d.Label = d.QueueID
		d.Assigned = 6
		d.ScopeMax = 6
		d.ScopeURL = "https://github.com/sim-scope-a"
		if i == 8 {
			d.ScopeURL = "https://github.com/sim-scope-z"
		}
		demands = append(demands, d)
		backends[d.QueueID] = backend
	}
	counts := map[string]int{}
	for cycle := 0; cycle < 10; cycle++ {
		// Deliberately synchronized completions and six READY guests remove cold
		// boot noise. Both scopes remain continuously backlogged with equal caps.
		w.inventory.Used = CapacityUsed{6, 24, 49152}
		w.inventory.Profiles[0].Ready = 6
		before := len(b.assignments)
		if err := b.Sync(context.Background(), demands, workers, backends); err != nil {
			t.Fatal(err)
		}
		for _, a := range b.assignments[before:] {
			counts[a.ScopeURL]++
			r := w.records[a.ID]
			r.State = "terminal"
			r.CompletedAt = time.Now().UTC()
			w.records[a.ID] = r
		}
		for {
			if err := b.Sync(context.Background(), nil, workers, backends); err != nil {
				t.Fatal(err)
			}
			for id := range b.registrationChecks {
				awaitCleanup(t, b, id)
			}
			if err := b.finishRegistrationChecks(); err != nil {
				t.Fatal(err)
			}
			remaining := false
			for _, a := range b.assignments {
				if a.Phase != Complete {
					remaining = true
				}
			}
			if !remaining {
				break
			}
		}
		for i := range demands {
			demands[i].Revision++
			// GitHub completion acknowledgements prevent old revision credits from
			// hiding this experiment's intentionally maintained continuous backlog.
			demands[i].CompletedRunners = nil
			for _, a := range b.assignments[before:] {
				if a.QueueID == demands[i].QueueID {
					demands[i].CompletedRunners = append(demands[i].CompletedRunners, a.RunnerName)
				}
			}
		}
	}
	t.Logf("equal continuously backlogged scopes, 8 labels vs 1, six free slots: %v", counts)
	if counts["https://github.com/sim-scope-a"] != 51 || counts["https://github.com/sim-scope-z"] != 9 {
		t.Fatalf("characterized policy changed; reassess fairness: %v", counts)
	}
}
