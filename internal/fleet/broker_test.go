package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeWorker struct {
	inventory                     Inventory
	records                       map[string]Record
	reserve, seal, deliver, drain int
	unavailable, lostDelivery     bool
	beforeSeal                    func()
	gotJIT                        string
}

func (w *fakeWorker) Inventory(context.Context) (Inventory, error) {
	if w.unavailable {
		return Inventory{}, errors.New("offline")
	}
	s := w.inventory
	s.Records = nil
	for _, r := range w.records {
		s.Records = append(s.Records, r)
	}
	return s, nil
}
func (w *fakeWorker) Reserve(_ context.Context, id, profile, digest string) (Record, error) {
	if r, ok := w.records[id]; ok {
		return r, nil
	}
	w.reserve++
	p := w.inventory.Profiles[0]
	r := Record{Request: Request{Identity: w.inventory.Identity, AssignmentID: id, VMID: "worker-owned-" + id, ProfileDigest: digest, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB, DiskGiB: p.DiskGiB}, State: "reserved"}
	w.records[id] = r
	if w.inventory.Profiles[0].Ready > 0 {
		w.inventory.Profiles[0].Ready--
	} else {
		w.inventory.Used.VMs++
		w.inventory.Used.CPUs += p.CPUs
		w.inventory.Used.MemoryMiB += p.MemoryMiB
	}
	return r, nil
}
func (w *fakeWorker) Seal(_ context.Context, id string) (Record, error) {
	if w.beforeSeal != nil {
		w.beforeSeal()
	}
	w.seal++
	r := w.records[id]
	r.State = "sealed"
	w.records[id] = r
	return r, nil
}
func (w *fakeWorker) Deliver(_ context.Context, id, jit string) error {
	w.deliver++
	w.gotJIT = jit
	r := w.records[id]
	r.State = "delivery_intent"
	w.records[id] = r
	if w.lostDelivery {
		return errors.New("ack lost")
	}
	return nil
}
func (w *fakeWorker) Status(_ context.Context, id string) (Record, error) {
	if w.unavailable {
		return Record{}, errors.New("offline")
	}
	r, ok := w.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}
func (w *fakeWorker) Drain(context.Context) error { w.drain++; w.inventory.Draining = true; return nil }

type fakeBackend struct {
	jit, remove int
	removeErr   bool
	beforeJIT   func()
}

func (b *fakeBackend) JIT(context.Context, string) (string, error) {
	if b.beforeJIT != nil {
		b.beforeJIT()
	}
	b.jit++
	return "private-jit-fixture", nil
}
func (b *fakeBackend) Remove(context.Context, string) error {
	b.remove++
	if b.removeErr {
		return errors.New("github offline")
	}
	return nil
}
func fixture(t *testing.T) (*Broker, *fakeWorker, *fakeBackend, Demand) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	b, e := Open(dir, "roost", Limits{MaxVMs: 3, MaxCPUs: 12, MaxMemoryMiB: 24576})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	p := Profile{ID: "medium", Digest: "verified-image-digest", Machine: "q35", CPUs: 4, MemoryMiB: 8192, DiskGiB: 48}
	w := &fakeWorker{inventory: Inventory{Identity: Identity{"worker-a", "roost", 1}, Budget: Budget{3, 12, 24576}, Used: CapacityUsed{1, 4, 8192}, Profiles: []ProfileInventory{{Profile: p, Ready: 1}}}, records: map[string]Record{}}
	return b, w, &fakeBackend{}, Demand{QueueID: "scope|chickadee", ScopeURL: "https://github.com/example", Label: "chickadee", Profile: p, Assigned: 1, Revision: 1}
}
func TestWarmPlacementScopeQuotaAndDurableCredentialIntent(t *testing.T) {
	b, w, backend, d := fixture(t)
	cold := *w
	cold.inventory = w.inventory
	cold.inventory.Identity.WorkerID = "worker-0"
	cold.inventory.Profiles = append([]ProfileInventory(nil), w.inventory.Profiles...)
	cold.inventory.Profiles[0].Ready = 0
	cold.inventory.Used = CapacityUsed{}
	cold.records = map[string]Record{}
	w.beforeSeal = func() {
		data, e := os.ReadFile(filepath.Join(b.dir, "assignments.json"))
		if e != nil || !strings.Contains(string(data), SealIntent) {
			t.Fatal("seal preceded durable intent")
		}
	}
	backend.beforeJIT = func() {
		data, _ := os.ReadFile(filepath.Join(b.dir, "assignments.json"))
		if !strings.Contains(string(data), JITIntent) {
			t.Fatal("JIT preceded durable intent")
		}
	}
	alias := d
	alias.QueueID = "scope|medium-versioned"
	alias.Label = "medium-versioned"
	workers := map[string]Worker{"worker-0": &cold, "worker-a": w}
	backends := map[string]Backend{d.QueueID: backend, alias.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d, alias}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 1 || cold.reserve != 0 || backend.jit != 1 {
		t.Fatal("warm guest not preferred or alias quota exceeded")
	}
	if e := b.Sync(context.Background(), []Demand{d, alias}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 1 || backend.jit != 1 {
		t.Fatal("duplicate assigned demand allocated twice")
	}
	data, _ := os.ReadFile(filepath.Join(b.dir, "assignments.json"))
	if strings.Contains(string(data), "private-jit-fixture") {
		t.Fatal("JIT persisted")
	}
}
func TestLostDeliveryRestartNeverReplaysAndUnknownRetainsCapacity(t *testing.T) {
	b, w, backend, d := fixture(t)
	w.lostDelivery = true
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	dir := b.dir
	b.Close()
	restarted, e := Open(dir, "roost", b.limits)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	w.unavailable = true
	if e = restarted.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	w.unavailable = false
	if e = restarted.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.deliver != 1 || backend.jit != 1 || restarted.assignments[0].Phase != Uncertain {
		t.Fatal("credential or allocation replayed after restart")
	}
}
func TestTerminalCleanupRegistrationRemovalBeforeQuotaRelease(t *testing.T) {
	b, w, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	a := b.assignments[0]
	r := w.records[a.ID]
	r.State = "terminal"
	w.records[a.ID] = r
	backend.removeErr = true
	if e := b.Sync(context.Background(), nil, workers, backends); e != nil {
		t.Fatal(e)
	}
	if !b.scopeBusy(d.ScopeURL) || b.assignments[0].Phase != Terminal {
		t.Fatal("quota released before GitHub cleanup")
	}
	backend.removeErr = false
	if e := b.Sync(context.Background(), nil, workers, backends); e != nil {
		t.Fatal(e)
	}
	if b.scopeBusy(d.ScopeURL) || b.assignments[0].Phase != Complete {
		t.Fatal("cleaned assignment retained")
	}
}
func TestUnjournaledWorkerAndGenerationMismatchFailClosed(t *testing.T) {
	b, w, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	w.records["lost"] = Record{Request: Request{Identity: w.inventory.Identity, AssignmentID: "lost"}, State: "sealed"}
	if !errors.Is(b.Sync(context.Background(), []Demand{d}, workers, backends), ErrUnjournaled) || w.reserve != 0 {
		t.Fatal("lost journal admitted new work")
	}
	delete(w.records, "lost")
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	w.inventory.Identity.Generation = 2
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 1 || backend.jit != 1 {
		t.Fatal("fenced assignment was replaced")
	}
}
func TestStateLockAndPermissions(t *testing.T) {
	b, _, _, _ := fixture(t)
	if _, e := Open(b.dir, "roost", b.limits); e == nil {
		t.Fatal("second broker owns state")
	}
	if e := b.persist(); e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(filepath.Join(b.dir, "assignments.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("journal not private")
	}
}

func TestAssignedRevisionCannotReplaceCompletedRunner(t *testing.T) {
	b, w, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	a := b.assignments[0]
	r := w.records[a.ID]
	r.State = "terminal"
	w.records[a.ID] = r
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 1 {
		t.Fatal("stale assigned snapshot spawned a replacement")
	}
	d.Revision = 2
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 2 {
		t.Fatal("fresh authoritative revision blocked")
	}
}

func TestScopeQuotaAcrossAliasesUsesStrictMinimum(t *testing.T) {
	b, w, backend, d := fixture(t)
	d.ScopeMax = 2
	alias := d
	alias.QueueID = "alias"
	alias.Label = "alias"
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend, alias.QueueID: backend}
	if e := b.Sync(context.Background(), []Demand{d, alias}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 2 {
		t.Fatal("operator scope quota two not supported")
	}
	c, w2, be, first := fixture(t)
	first.ScopeMax = 2
	other := first
	other.ScopeMax = 1
	other.QueueID = "other"
	other.Label = "other"
	if e := c.Sync(context.Background(), []Demand{first, other}, map[string]Worker{"worker-a": w2}, map[string]Backend{first.QueueID: be, other.QueueID: be}); e != nil {
		t.Fatal(e)
	}
	if w2.reserve != 1 {
		t.Fatal("alias bypassed strictest scope quota")
	}
}
func TestJITIntentRecoveryCannotMintOrDeliverAgain(t *testing.T) {
	b, w, backend, d := fixture(t)
	_ = b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend})
	a := b.assignments[0]
	record := w.records[a.ID]
	record.State = "sealed"
	w.records[a.ID] = record
	b.assignments[0].Phase = JITIntent
	if e := b.persist(); e != nil {
		t.Fatal(e)
	}
	dir := b.dir
	b.Close()
	recovered, e := Open(dir, "roost", b.limits)
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	if e = recovered.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); e != nil {
		t.Fatal(e)
	}
	if backend.jit != 1 || w.deliver != 1 || recovered.assignments[0].Phase != Uncertain {
		t.Fatal("ambiguous credential intent retried")
	}
}
func TestPhysicalUsedPreventsAliasedInventoryOvercommit(t *testing.T) {
	b, w, backend, d := fixture(t)
	w.inventory.Budget = Budget{1, 4, 8192}
	if e := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); e != nil {
		t.Fatal(e)
	}
	second := d
	second.ScopeURL = "https://github.com/another"
	second.QueueID = "another"
	if e := b.Sync(context.Background(), []Demand{second}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend, second.QueueID: backend}); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 1 {
		t.Fatal("physical used capacity ignored")
	}
}

func TestWorkerTerminalTimeSurvivesLateBrokerObservation(t *testing.T) {
	b, w, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	a := b.assignments[0]
	b.assignments[0].ReservedAt = time.Now().UTC().Add(-72 * time.Hour)
	record := w.records[a.ID]
	record.State = "terminal"
	record.CompletedAt = b.assignments[0].ReservedAt.Add(time.Hour)
	w.records[a.ID] = record
	if e := b.Sync(context.Background(), nil, workers, backends); e != nil {
		t.Fatal(e)
	}
	if b.assignments[0].Phase != Complete || b.assignments[0].CompletedAt.Sub(b.assignments[0].ReservedAt) != time.Hour {
		t.Fatal("broker outage fabricated VM duration")
	}
}

func TestTwoAssignedJobsDrainSeriallyWithoutDuplicateSnapshotExtras(t *testing.T) {
	b, w, backend, d := fixture(t)
	d.Assigned = 2
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	first := b.assignments[0]
	record := w.records[first.ID]
	record.State = "terminal"
	w.records[first.ID] = record
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 2 {
		t.Fatal("second assigned job stalled behind stale revision fence")
	}
	second := b.assignments[1]
	record = w.records[second.ID]
	record.State = "terminal"
	w.records[second.ID] = record
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if w.reserve != 2 {
		t.Fatal("completed batch allocated an extra VM")
	}
}

type delayedBackend struct {
	fakeBackend
	exists      bool
	removedLate int
}

func (b *delayedBackend) Remove(context.Context, string) error {
	b.remove++
	if b.exists {
		b.exists = false
		b.removedLate++
	}
	return nil
}
func TestUncertainCompleteRetriesLateRegistrationAfterRestart(t *testing.T) {
	b, w, _, d := fixture(t)
	backend := &delayedBackend{}
	w.lostDelivery = true
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	a := b.assignments[0]
	record := w.records[a.ID]
	record.State = "terminal"
	record.CompletedAt = time.Now().UTC()
	w.records[a.ID] = record
	if e := b.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if b.assignments[0].Phase != Complete || !b.assignments[0].CredentialUncertain || backend.remove != 1 {
		t.Fatal("uncertain credential tombstone not retained")
	}
	// The first cleanup found nothing; a delayed GitHub registration arrives later.
	backend.exists = true
	b.assignments[0].RegistrationCheckedAt = time.Now().UTC().Add(-31 * time.Second)
	if e := b.persist(); e != nil {
		t.Fatal(e)
	}
	dir := b.dir
	b.Close()
	recovered, e := Open(dir, "roost", b.limits)
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	if e = recovered.Sync(context.Background(), []Demand{d}, workers, backends); e != nil {
		t.Fatal(e)
	}
	if backend.removedLate != 1 || backend.remove != 2 || w.reserve != 1 || w.deliver != 1 {
		t.Fatal("delayed registration missed or VM/JIT replayed")
	}
	_ = recovered.Sync(context.Background(), nil, workers, backends)
	if backend.remove != 2 {
		t.Fatal("grace cleanup ignored 30-second bound")
	}
	recovered.assignments[0].CompletedAt = time.Now().UTC().Add(-11 * time.Minute)
	recovered.assignments[0].RegistrationCheckedAt = time.Now().UTC().Add(-31 * time.Second)
	_ = recovered.Sync(context.Background(), nil, workers, backends)
	if backend.remove != 2 {
		t.Fatal("cleanup continued beyond grace window")
	}
}
func TestNormalCompleteDoesNotPollRegistrationAgain(t *testing.T) {
	b, w, backend, d := fixture(t)
	workers := map[string]Worker{"worker-a": w}
	backends := map[string]Backend{d.QueueID: backend}
	_ = b.Sync(context.Background(), []Demand{d}, workers, backends)
	a := b.assignments[0]
	record := w.records[a.ID]
	record.State = "terminal"
	w.records[a.ID] = record
	_ = b.Sync(context.Background(), nil, workers, backends)
	b.assignments[0].RegistrationCheckedAt = time.Now().UTC().Add(-time.Minute)
	_ = b.Sync(context.Background(), nil, workers, backends)
	if backend.remove != 1 {
		t.Fatal("successful credentials caused extra registration polling")
	}
}

func TestPausedScopeCannotResumePrecredentialIntent(t *testing.T) {
	for _, phase := range []string{Intent, Reserved, SealIntent} {
		for _, assigned := range []int{0, -1} {
			t.Run(phase+fmt.Sprint(assigned), func(t *testing.T) {
				b, w, backend, d := fixture(t)
				a := Assignment{ID: "planned", RunnerName: "chickadee-planned", QueueID: d.QueueID, ScopeURL: d.ScopeURL, Label: d.Label, Profile: d.Profile, Worker: w.inventory.Identity, WorkerProfileID: d.Profile.ID, Phase: phase, DemandRevision: 1, LastDemandRevision: 1, ReservedAt: time.Now().UTC()}
				b.assignments = append(b.assignments, a)
				record, _ := w.Reserve(context.Background(), a.ID, a.WorkerProfileID, a.Profile.Digest)
				if phase == SealIntent {
					record.State = "sealed"
					w.records[a.ID] = record
				}
				b.assignments[0].VMID = record.Request.VMID
				if e := b.persist(); e != nil {
					t.Fatal(e)
				}
				var demands []Demand
				if assigned == 0 {
					d.Assigned = 0
					demands = []Demand{d}
				}
				workers := map[string]Worker{"worker-a": w}
				backends := map[string]Backend{d.QueueID: backend}
				if e := b.Sync(context.Background(), demands, workers, backends); e != nil {
					t.Fatal(e)
				}
				if backend.jit != 0 || w.deliver != 0 || w.seal != 0 {
					t.Fatal("removed or paused scope generated credentials")
				}
				record.State = "terminal"
				record.CompletedAt = time.Now().UTC()
				w.records[a.ID] = record
				if e := b.Sync(context.Background(), demands, workers, backends); e != nil {
					t.Fatal(e)
				}
				if b.assignments[0].Phase != Complete || backend.remove != 1 {
					t.Fatal("paused reservation timeout did not reconcile cleanup")
				}
			})
		}
	}
}
func TestActiveDemandResumesCompatibleReservedIntent(t *testing.T) {
	b, w, backend, d := fixture(t)
	a := Assignment{ID: "planned", RunnerName: "chickadee-planned", QueueID: d.QueueID, ScopeURL: d.ScopeURL, Label: d.Label, Profile: d.Profile, Worker: w.inventory.Identity, WorkerProfileID: d.Profile.ID, Phase: Reserved, DemandRevision: 1, LastDemandRevision: 1, ReservedAt: time.Now().UTC()}
	record, _ := w.Reserve(context.Background(), a.ID, a.WorkerProfileID, a.Profile.Digest)
	a.VMID = record.Request.VMID
	b.assignments = append(b.assignments, a)
	if e := b.Sync(context.Background(), []Demand{d}, map[string]Worker{"worker-a": w}, map[string]Backend{d.QueueID: backend}); e != nil {
		t.Fatal(e)
	}
	if backend.jit != 1 || w.deliver != 1 {
		t.Fatal("valid reserved intent did not resume")
	}
}
