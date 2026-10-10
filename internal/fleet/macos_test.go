package fleet

import "testing"

func TestNativeMacCannotUseMatchingLinuxResourceShape(t *testing.T) {
	linux := Profile{Digest: "same", Machine: "q35", CPUs: 2, MemoryMiB: 4096, DiskGiB: 64}
	mac := linux
	mac.Machine = "apple-vz"
	if sameProfile(linux, mac) {
		t.Fatal("OS/backend identity ignored")
	}
	b := &Broker{}
	inventories := map[string]Inventory{"linux": {Identity: Identity{WorkerID: "linux"}, Profiles: []ProfileInventory{{Profile: linux, Ready: 1}}}, "mac": {Identity: Identity{WorkerID: "mac"}, Profiles: []ProfileInventory{{Profile: mac, Ready: 1}}}}
	id, _, ok := b.place(mac, inventories, []string{"linux", "mac"})
	if !ok || id != "mac" {
		t.Fatal("Mac queue misrouted")
	}
	delete(inventories, "mac")
	if _, _, ok = b.place(mac, inventories, []string{"linux"}); ok {
		t.Fatal("Linux fallback for Mac queue")
	}
}
