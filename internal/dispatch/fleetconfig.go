package dispatch

import (
	"encoding/json"
	"fmt"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
	"github.com/plover-digital/chickadee/workerapi"
	"io"
	"os"
)

type FleetConfig struct {
	Version   int    `json:"version"`
	BrokerID  string `json:"broker_id"`
	StateDir  string `json:"state_dir"`
	StatusDir string `json:"status_dir"`
	Budget    struct {
		MaxVMs       int `json:"max_vms"`
		MaxCPUs      int `json:"max_cpus"`
		MaxMemoryMiB int `json:"max_memory_mib"`
	} `json:"budget"`
	ImageDigests map[string]string        `json:"image_digests"`
	Workers      []workerapi.ClientConfig `json:"workers"`
}

func LoadFleet(path string) (FleetConfig, error) {
	var c FleetConfig
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 128*1024))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("invalid fleet config")
	}
	if c.Version != 1 || c.BrokerID == "" || c.StateDir == "" || c.StatusDir == "" || len(c.Workers) < 1 || len(c.Workers) > 16 || c.Budget.MaxVMs < 1 || c.Budget.MaxVMs > 32 || c.Budget.MaxCPUs < 1 || c.Budget.MaxMemoryMiB < 512 {
		return c, fmt.Errorf("invalid fleet limits")
	}
	seen := map[string]bool{}
	for _, w := range c.Workers {
		if !w.Identity.Valid() || w.Identity.BrokerID != c.BrokerID || seen[w.Identity.WorkerID] {
			return c, fmt.Errorf("invalid worker identity")
		}
		seen[w.Identity.WorkerID] = true
	}
	for _, digest := range c.ImageDigests {
		if !digestPattern.MatchString(digest) {
			return c, fmt.Errorf("invalid image digest")
		}
	}
	return c, nil
}
func (c FleetConfig) Limits() fleet.Limits {
	return fleet.Limits{MaxVMs: c.Budget.MaxVMs, MaxCPUs: c.Budget.MaxCPUs, MaxMemoryMiB: c.Budget.MaxMemoryMiB}
}
