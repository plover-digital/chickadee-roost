package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The admission bridge requires config_sha256 plus a fresh UTC timestamp. A
// different hash key silently makes both apply and rollback acknowledgements fail.
func TestAdmissionBridgeAcknowledgementContract(t *testing.T) {
	dir := t.TempDir()
	started := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	if err := acknowledge(dir, digest, "applied"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "reload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Hash    string    `json:"config_sha256"`
		Status  string    `json:"status"`
		Updated time.Time `json:"updated_at"`
	}
	if err = json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Hash != digest || ack.Status != "applied" || ack.Updated.Before(started) {
		t.Fatalf("bridge would reject acknowledgement: %s", raw)
	}
}
