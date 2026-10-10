package site

import "testing"

func TestMacPreviewIsExplicitOptIn(t *testing.T) {
	if _, err := requestedQueuesFor([]string{"chickadee-small-macos-26"}, false); err == nil {
		t.Fatal("disabled preview accepted")
	}
	queues, err := requestedQueuesFor([]string{"chickadee-small-macos-26"}, true)
	if err != nil || len(queues) != 2 || queues[0] != "chickadee" || queues[1] != "chickadee-small-macos-26" {
		t.Fatalf("preview selection invalid %v %v", queues, err)
	}
	queues, err = requestedQueuesFor(nil, true)
	if err != nil || len(queues) != 1 || queues[0] != "chickadee" {
		t.Fatal("Mac enabled by default")
	}
	if _, err = requestedQueuesFor([]string{"chickadee-medium-macos-26"}, true); err == nil {
		t.Fatal("unsupported Mac size accepted")
	}
}
