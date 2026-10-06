package fleet

import "testing"

func TestColdSmallPlacementCanReclaimIncompatibleWarmMedium(t *testing.T) {
	small := Profile{ID: "small", Digest: "image", Machine: "q35", CPUs: 2, MemoryMiB: 4096, DiskGiB: 48}
	medium := small
	medium.ID = "medium"
	medium.CPUs = 4
	medium.MemoryMiB = 8192
	snapshot := Inventory{Identity: Identity{"host", "roost", 1}, Budget: Budget{3, 6, 12288}, Used: CapacityUsed{2, 6, 12288}, Profiles: []ProfileInventory{{Profile: medium, Ready: 1}, {Profile: small}}, Records: []Record{{Request: Request{CPUs: 2, MemoryMiB: 4096, DiskGiB: 48}, State: "sealed"}}}
	b := &Broker{}
	host, p, ok := b.place(small, map[string]Inventory{"host": snapshot}, []string{"host"})
	if !ok || host != "host" || p.ID != "small" {
		t.Fatal("incompatible warm medium prevented cold small placement")
	}
	// Cold medium allocation cannot evict the reserved small or double-count
	// the existing compatible warm medium as newly available physical space.
	if workerCapacity(snapshot, medium) {
		t.Fatal("another medium bypassed the physical host budget")
	}
	occupiedMedium := snapshot
	occupiedMedium.Used = CapacityUsed{1, 4, 8192}
	occupiedMedium.Profiles = []ProfileInventory{{Profile: medium}}
	if workerCapacity(occupiedMedium, medium) {
		t.Fatal("two medium VMs fit a six-CPU/twelve-GiB host")
	}
	snapshot.Profiles[0].Ready = 0
	snapshot.Profiles[0].Booting = 1
	if workerCapacity(snapshot, small) {
		t.Fatal("booting guest counted as reclaimable")
	}
	snapshot.Profiles[0].Booting = 0
	snapshot.Records = append(snapshot.Records, Record{Request: Request{CPUs: 4, MemoryMiB: 8192, DiskGiB: 48}, State: "delivery_intent"})
	if workerCapacity(snapshot, small) {
		t.Fatal("credentialed/uncertain reservation counted as reclaimable")
	}
}
func TestReadyAliasesDoNotDoubleCountReclaimableCapacity(t *testing.T) {
	medium := Profile{ID: "default", Digest: "same-image", Machine: "q35", CPUs: 4, MemoryMiB: 8192, DiskGiB: 48}
	alias := medium
	alias.ID = "versioned"
	large := medium
	large.ID = "large"
	large.CPUs = 8
	large.MemoryMiB = 16384
	snapshot := Inventory{Budget: Budget{3, 10, 20480}, Used: CapacityUsed{2, 8, 16384}, Profiles: []ProfileInventory{{Profile: medium, Ready: 1}, {Profile: alias, Ready: 1}, {Profile: large}}}
	if workerCapacity(snapshot, large) {
		t.Fatal("aliases reclaimed the same warm guest twice")
	}
}
