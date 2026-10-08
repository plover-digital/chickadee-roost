package site

import (
	"fmt"
	"time"
)

type platformStats struct {
	Users, Accounts, Repositories, Active, Pending, Attention, Disconnected int
	UsageScopes, Completed, PeakAssigned, PartialReports, Reports           int
	Minutes, AverageAssigned                                                string
}

// Summaries are derived from persisted enrollments and non-overlapping, fresh
// authoritative usage snapshots. Enrollment rows are never summed as usage.
func platformStatistics(entries []Enrollment, usage []AccountUsage, samples []FleetSample, now time.Time) platformStats {
	var result platformStats
	users, accounts, repos := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, e := range entries {
		if e.Authenticated && e.User.ID > 0 {
			users[e.User.ID] = true
		}
		if e.Status == "disconnected" || e.DesiredState == "disconnected" {
			result.Disconnected++
			continue
		}
		if e.Account.ID > 0 {
			accounts[e.Account.ID] = true
		}
		if e.Repository.ID > 0 {
			repos[e.Repository.ID] = true
		}
		switch e.Status {
		case "active":
			result.Active++
		case "pending":
			result.Pending++
		default:
			result.Attention++
		}
	}
	result.Users, result.Accounts, result.Repositories = len(users), len(accounts), len(repos)
	seen := map[string]bool{}
	seconds := 0.0
	firstDay := now.UTC().AddDate(0, 0, -6).Format("2006-01-02")
	lastDay := now.UTC().Format("2006-01-02")
	for _, a := range usage {
		if a.ObservedAt.Before(now.Add(-5*time.Minute)) || a.ObservedAt.After(now.Add(time.Minute)) || len(a.RepositoryIDs) == 0 {
			continue
		}
		key := a.key()
		if seen[key] {
			continue
		}
		seen[key] = true
		result.UsageScopes++
		for _, d := range a.Usage {
			if d.Date >= firstDay && d.Date <= lastDay {
				seconds += d.VMSeconds
				result.Completed += d.VMs
			}
		}
	}
	result.Minutes = fmt.Sprintf("%.1f", seconds/60)
	sum := 0
	for _, sample := range samples {
		if sample.At.Before(now.Add(-24*time.Hour)) || sample.At.After(now) {
			continue
		}
		result.Reports++
		sum += sample.Assigned()
		result.PeakAssigned = max(result.PeakAssigned, sample.Assigned())
		if sample.WorkersOnline < sample.WorkersTotal {
			result.PartialReports++
		}
	}
	result.AverageAssigned = "unavailable"
	if result.Reports > 0 {
		result.AverageAssigned = fmt.Sprintf("%.1f", float64(sum)/float64(result.Reports))
	}
	return result
}
