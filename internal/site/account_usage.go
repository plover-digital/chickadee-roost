package site

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// AccountUsage is imported only through the private operator socket. Repository
// IDs must describe the complete verified selected runner-group access boundary.
type RunnerActivity struct {
	Label        string `json:"label"`
	Allocated    int    `json:"allocated"`
	Credentialed int    `json:"credentialed"`
}
type AccountUsage struct {
	Resources      []ResourceRun    `json:"resources,omitempty"`
	LiveAt         time.Time        `json:"live_at,omitempty"`
	Runners        []RunnerActivity `json:"runners,omitempty"`
	ObservedAt     time.Time        `json:"observed_at"`
	InstallationID int64            `json:"installation_id"`
	AccountID      int64            `json:"account_id"`
	AccountType    string           `json:"account_type"`
	RepositoryIDs  []int64          `json:"repository_ids"`
	Usage          []UsageDay       `json:"usage"`
}

func (a AccountUsage) key() string {
	if a.AccountType == "Organization" {
		return fmt.Sprintf("org:%d:%d", a.InstallationID, a.AccountID)
	}
	return fmt.Sprintf("repo:%d:%d", a.InstallationID, a.RepositoryIDs[0])
}
func validAccountUsage(entries []AccountUsage, now time.Time) bool {
	if len(entries) > 64 {
		return false
	}
	seen := map[string]bool{}
	for _, a := range entries {
		if !validResourceRuns(a.Resources, now) {
			return false
		}
		if len(a.Runners) > 32 || a.LiveAt.After(now.Add(time.Minute)) || len(a.Runners) > 0 && a.LiveAt.IsZero() {
			return false
		}
		labels := map[string]bool{}
		for _, runner := range a.Runners {
			allowed := knownQueue(runner.Label)
			for _, label := range extraQueues {
				if runner.Label == label {
					allowed = true
				}
			}
			if !allowed || labels[runner.Label] || runner.Allocated < 0 || runner.Allocated > 512 || runner.Credentialed < 0 || runner.Credentialed > runner.Allocated {
				return false
			}
			labels[runner.Label] = true
		}
		if a.ObservedAt.IsZero() || a.ObservedAt.After(now.Add(time.Minute)) || a.ObservedAt.Before(now.Add(-5*time.Minute)) {
			return false
		}
		if a.InstallationID <= 0 || a.AccountID <= 0 || (a.AccountType != "User" && a.AccountType != "Organization") || len(a.RepositoryIDs) == 0 || len(a.RepositoryIDs) > 100 || a.AccountType == "User" && len(a.RepositoryIDs) != 1 || !validUsage(a.Usage, now) {
			return false
		}
		repos := map[int64]bool{}
		for _, id := range a.RepositoryIDs {
			if id <= 0 || repos[id] {
				return false
			}
			repos[id] = true
		}
		key := a.key()
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}
func (a AccountUsage) authorized(p page) bool {
	if a.ObservedAt.Before(time.Now().Add(-5*time.Minute)) || p.User == nil || a.AccountType == "User" && a.AccountID != p.User.ID {
		return false
	}
	for _, id := range a.RepositoryIDs {
		found := false
		for _, c := range p.Choices {
			if c.Installation.ID == a.InstallationID && c.Installation.Account.ID == a.AccountID && c.Installation.Account.Type == a.AccountType && c.Repository.ID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (s *Server) loadAccountUsage() error {
	path := filepath.Join(s.cfg.StateDir, "account-usage.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedUsageFile(info) || info.Size() > 2*1024*1024 {
		return fmt.Errorf("invalid account usage store")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Expired daily data is dropped; identity/ACL validation still fails closed.
	var entries []AccountUsage
	if json.Unmarshal(data, &entries) != nil {
		return fmt.Errorf("invalid account usage store")
	}
	fresh := []AccountUsage{}
	for i := range entries {
		resourceRows := entries[i].Resources
		entries[i].Resources = nil
		for _, r := range resourceRows {
			if r.CompletedAt.Before(time.Now().AddDate(0, 0, -7)) {
				continue
			}
			entries[i].Resources = append(entries[i].Resources, r)
		}
		days := entries[i].Usage
		entries[i].Usage = nil
		for _, d := range days {
			date, err := time.Parse("2006-01-02", d.Date)
			if err != nil || !validUsage([]UsageDay{d}, date) {
				return fmt.Errorf("invalid account usage store")
			}
			if validUsage([]UsageDay{d}, time.Now()) {
				entries[i].Usage = append(entries[i].Usage, d)
			}
		}
	}
	for _, entry := range entries {
		if !entry.ObservedAt.Before(time.Now().Add(-5 * time.Minute)) {
			fresh = append(fresh, entry)
		}
	}
	entries = fresh
	if !validAccountUsage(entries, time.Now()) {
		return fmt.Errorf("invalid account usage store")
	}
	s.accountUsage = entries
	return nil
}
func ownedUsageFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}
func (s *Server) importAccountUsage(w http.ResponseWriter, r *http.Request) {
	var entries []AccountUsage
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	d.DisallowUnknownFields()
	if d.Decode(&entries) != nil || d.Decode(new(any)) != io.EOF || !validAccountUsage(entries, time.Now()) {
		http.Error(w, "Invalid account usage", 400)
		return
	}
	data, _ := json.Marshal(entries)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.CreateTemp(s.cfg.StateDir, ".account-usage-")
	if err != nil {
		http.Error(w, "Usage unavailable", 500)
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.cfg.StateDir, "account-usage.json"))
	}
	if err == nil {
		dir, e := os.Open(s.cfg.StateDir)
		if e != nil {
			err = e
		} else {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		http.Error(w, "Usage unavailable", 500)
		return
	}
	s.accountUsage = entries
	w.WriteHeader(204)
}

type ActivityView struct {
	Runners   []RunnerActivity
	Available bool
	Partial   bool
}

func (p page) CurrentActivity() ActivityView {
	out := ActivityView{}
	byLabel := map[string]RunnerActivity{}
	seen := map[string]bool{}
	for _, snapshot := range p.AccountUsage {
		if !snapshot.authorized(p) || seen[snapshot.key()] {
			continue
		}
		seen[snapshot.key()] = true
		if snapshot.LiveAt.IsZero() || time.Since(snapshot.LiveAt) > 3*time.Minute {
			out.Partial = true
			continue
		}
		out.Available = true
		for _, runner := range snapshot.Runners {
			if runner.Allocated == 0 {
				continue
			}
			total := byLabel[runner.Label]
			total.Label = runner.Label
			total.Allocated += runner.Allocated
			total.Credentialed += runner.Credentialed
			byLabel[runner.Label] = total
		}
	}
	for _, runner := range byLabel {
		out.Runners = append(out.Runners, runner)
	}
	sort.Slice(out.Runners, func(i, j int) bool { return out.Runners[i].Label < out.Runners[j].Label })
	return out
}
