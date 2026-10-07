package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plover-digital/chickadee-roost/internal/fleet"
)

func TestStatusExportsBoundedGlobalFleetSchema(t *testing.T) {
	dir := t.TempDir()
	sample := fleet.Telemetry{At: time.Now().UTC(), Ready: 2, Booting: 1, Reserved: 3, Running: 4, Uncertain: 1, WorkersOnline: 1, WorkersTotal: 2}
	if err := writeStatus(dir, nil, nil, nil, false, sample); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Fleet fleet.Telemetry `json:"fleet"`
	}
	if err = json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Fleet != sample {
		t.Fatalf("fleet snapshot altered: %+v", parsed.Fleet)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(data, &fields)
	var metrics map[string]any
	json.Unmarshal(fields["fleet"], &metrics)
	if len(metrics) != 8 {
		t.Fatalf("unexpected fleet metadata fields: %v", metrics)
	}
	if _, err = time.Parse(time.RFC3339Nano, metrics["at"].(string)); err != nil {
		t.Fatal("fleet timestamp is not RFC3339")
	}
}
func TestStatusDoesNotInventFleetSnapshotBeforeObservation(t *testing.T) {
	dir := t.TempDir()
	if err := writeStatus(dir, nil, nil, nil, false, fleet.Telemetry{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "status.json"))
	var fields map[string]json.RawMessage
	json.Unmarshal(data, &fields)
	if _, ok := fields["fleet"]; ok {
		t.Fatal("unobserved fleet exported as current")
	}
}
