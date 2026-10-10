// Package dispatch adapts the existing operator queue catalog to Roost's broker.
// It owns no VM image paths, processes or guest networking.
package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

type Queue struct {
	Key, Scope, Label, GitHubURL, ClientID, KeyFile, ScaleSet, ImageDigest, Machine string
	InstallationID                                                                  int64
	RunnerGroupID, Max, ScopeMax, CPUs, MemoryMiB, DiskGiB                          int
}
type Catalog struct {
	Limits struct {
		Max int `json:"max_vms"`
	} `json:"limits"`
	ClientID string `json:"app_client_id"`
	KeyFile  string `json:"app_key_file"`
	Images   map[string]struct {
		DiskGiB int    `json:"disk_gib"`
		Machine string `json:"machine"`
	} `json:"images"`
	Resources map[string]struct {
		CPUs      int `json:"cpus"`
		MemoryMiB int `json:"memory_mib"`
	} `json:"resource_classes"`
	Scopes map[string]struct {
		Disabled       bool   `json:"disabled"`
		Max            int    `json:"max_vms"`
		GitHubURL      string `json:"github_url"`
		InstallationID int64  `json:"app_installation_id"`
		RunnerGroupID  int    `json:"runner_group_id"`
		Profiles       map[string]struct {
			Image     string `json:"image"`
			Resources string `json:"resources"`
			Max       int    `json:"max_vms"`
		} `json:"profiles"`
	} `json:"scopes"`
}

var labelPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadCatalog deliberately reads the established single-host bridge schema.
// Legacy host fields stay in that operator file but are not sent to workers.
// Immutable digest mappings come from the separate, private fleet catalog.
func LoadCatalog(path string, digests map[string]string) ([]Queue, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > 2<<20 {
		return nil, "", fmt.Errorf("queue catalog too large")
	}
	var c Catalog
	d := json.NewDecoder(strings.NewReader(string(raw)))
	if err = d.Decode(&c); err != nil {
		return nil, "", fmt.Errorf("invalid queue catalog")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, "", fmt.Errorf("trailing catalog data")
	}
	if c.ClientID == "" || c.KeyFile == "" || len(c.Scopes) == 0 || len(c.Scopes) > 500 {
		return nil, "", fmt.Errorf("missing or oversized scope catalog")
	}
	names := make([]string, 0, len(c.Scopes))
	for n := range c.Scopes {
		names = append(names, n)
	}
	sort.Strings(names)
	var queues []Queue
	seen := map[string]bool{}
	for _, name := range names {
		s := c.Scopes[name]
		if s.Max == 0 {
			s.Max = c.Limits.Max
		}
		if s.Disabled {
			continue
		}
		u, e := url.Parse(s.GitHubURL)
		if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, "", fmt.Errorf("invalid GitHub scope")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 1 || len(parts) > 2 || parts[0] == "" || parts[0] == "enterprises" || s.InstallationID <= 0 || s.RunnerGroupID <= 0 || s.Max < 1 || s.Max > 32 {
			return nil, "", fmt.Errorf("invalid scope identity or quota")
		}
		labels := make([]string, 0, len(s.Profiles))
		for n := range s.Profiles {
			labels = append(labels, n)
		}
		sort.Strings(labels)
		if len(labels) > 32 {
			return nil, "", fmt.Errorf("too many scope queues")
		}
		for _, label := range labels {
			p := s.Profiles[label]
			im, ok := c.Images[p.Image]
			r, rok := c.Resources[p.Resources]
			digest := digests[p.Image]
			if !labelPattern.MatchString(label) || !ok || !rok || !digestPattern.MatchString(digest) || (im.Machine != "q35" && im.Machine != "microvm" && im.Machine != "apple-vz") || p.Max < 1 || p.Max > 32 || r.CPUs < 1 || r.CPUs > 32 || r.MemoryMiB < 512 || r.MemoryMiB > 131072 || im.DiskGiB < 8 || im.DiskGiB > 128 {
				return nil, "", fmt.Errorf("invalid profile or missing immutable image digest")
			}
			scope := strings.ToLower(strings.TrimRight(s.GitHubURL, "/"))
			key := scope + "|" + label
			if seen[key] {
				return nil, "", fmt.Errorf("duplicate GitHub queue")
			}
			seen[key] = true
			queues = append(queues, Queue{Key: key, Scope: scope, Label: label, GitHubURL: s.GitHubURL, ClientID: c.ClientID, KeyFile: c.KeyFile, ScaleSet: label, ImageDigest: digest, InstallationID: s.InstallationID, RunnerGroupID: s.RunnerGroupID, Max: min(p.Max, s.Max), ScopeMax: s.Max, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, DiskGiB: im.DiskGiB, Machine: im.Machine})
		}
	}
	sum := sha256.Sum256(raw)
	return queues, hex.EncodeToString(sum[:]), nil
}
