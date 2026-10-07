package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorPlacementPriorityConfig(t *testing.T) {
	data, err := os.ReadFile("../../examples/fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value string
		valid bool
		want  int
	}{
		{"0", true, 0}, {"100", true, 100}, {"-100", true, -100}, {"101", false, 0}, {"-101", false, 0}, {"1.5", false, 0}, {`"10"`, false, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet.json")
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"placement_priority": 0`, `"placement_priority": `+tc.value, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadFleet(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if tc.valid && (c.Workers[0].PlacementPriority != tc.want || c.Workers[0].Endpoint != "https://127.0.0.1:18443") {
				t.Fatalf("config: %+v", c.Workers[0])
			}
		})
	}
	path := filepath.Join(t.TempDir(), "legacy.json")
	os.WriteFile(path, []byte(strings.Replace(string(data), `"placement_priority": 0,`, "", 1)), 0600)
	c, err := LoadFleet(path)
	if err != nil || c.Workers[0].PlacementPriority != 0 {
		t.Fatalf("legacy default: %v", err)
	}
	if (RemoteWorker{Priority: 17}).PlacementPriority() != 17 {
		t.Fatal("broker metadata lost")
	}
}
