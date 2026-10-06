// Package fleet coordinates disposable runners across authenticated workers.
// It stores assignment intent, never GitHub JIT credentials.
package fleet

import (
	"context"
	"time"
)

type Identity struct {
	WorkerID, BrokerID string
	Generation         uint64
}
type Profile struct {
	ID, Digest, Machine      string
	CPUs, MemoryMiB, DiskGiB int
}
type ProfileInventory struct {
	Profile
	Ready, Booting int
}
type CapacityUsed struct{ VMs, CPUs, MemoryMiB int }
type Budget struct{ MaxVMs, MaxCPUs, MaxMemoryMiB int }
type Request struct {
	Identity                          Identity
	AssignmentID, VMID, ProfileDigest string
	CPUs, MemoryMiB, DiskGiB          int
}
type Record struct {
	CompletedAt time.Time
	Request     Request
	State       string
}
type Inventory struct {
	Used     CapacityUsed
	Identity Identity
	Draining bool
	Budget   Budget
	Profiles []ProfileInventory
	Records  []Record
}

// Worker operations have no tenant credentials except the one-time Deliver body.
// terminal must mean process exit and disk cleanup have both been confirmed.
type Worker interface {
	Inventory(context.Context) (Inventory, error)
	Reserve(context.Context, string, string, string) (Record, error)
	Seal(context.Context, string) (Record, error)
	Deliver(context.Context, string, string) error
	Status(context.Context, string) (Record, error)
	Drain(context.Context) error
}

// Backend owns one GitHub scope/queue. Remove is idempotent and reconciles by the
// durable runner name even when a JIT response was lost.
type Backend interface {
	JIT(context.Context, string) (string, error)
	Remove(context.Context, string) error
}
type Demand struct {
	CompletedRunners         []string
	ScopeMax                 int
	QueueID, ScopeURL, Label string
	Profile                  Profile
	Assigned                 int
	Revision                 uint64
}
type Limits struct{ MaxVMs, MaxCPUs, MaxMemoryMiB int }
type Assignment struct {
	GitHubCompletedRevision                  uint64
	CredentialUncertain                      bool
	RegistrationCheckedAt                    time.Time
	ReservedAt, CompletedAt                  time.Time
	DemandRevision                           uint64
	LastDemandRevision                       uint64
	ID, RunnerName, QueueID, ScopeURL, Label string
	Profile                                  Profile
	Worker                                   Identity
	WorkerProfileID, VMID, Phase             string
}

const (
	Intent     = "reserve_intent"
	Reserved   = "reserved"
	SealIntent = "seal_intent"
	JITIntent  = "jit_intent"
	Delivered  = "delivered"
	Uncertain  = "uncertain"
	Terminal   = "terminal"
	Complete   = "complete"
)
