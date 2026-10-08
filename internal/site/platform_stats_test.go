package site

import (
	"testing"
	"time"
)

func TestPlatformStatisticsDeduplicateAndRespectFreshness(t *testing.T) {
	now := time.Now().UTC()
	entries := []Enrollment{
		{Authenticated: true, User: User{ID: 7}, Account: Account{ID: 8}, Repository: Repository{ID: 9}, Status: "active"},
		{Authenticated: true, User: User{ID: 7}, Account: Account{ID: 8}, Repository: Repository{ID: 10}, Status: "pending"},
		{Authenticated: true, User: User{ID: 11}, Account: Account{ID: 12}, Repository: Repository{ID: 13}, Status: "active", DesiredState: "disconnected"},
		{User: User{ID: 99}, Status: "permission-required"},
	}
	a := AccountUsage{ObservedAt: now, InstallationID: 1, AccountID: 8, AccountType: "Organization", RepositoryIDs: []int64{9, 10}, Usage: []UsageDay{{Date: now.Format("2006-01-02"), VMSeconds: 120, VMs: 2}, {Date: now.AddDate(0, 0, -7).Format("2006-01-02"), VMSeconds: 9999, VMs: 99}}}
	stale := a
	stale.InstallationID = 2
	stale.ObservedAt = now.Add(-6 * time.Minute)
	samples := []FleetSample{{At: now.Add(-25 * time.Hour), Running: 100}, {At: now.Add(-time.Hour), Running: 2, WorkersOnline: 1, WorkersTotal: 2}, {At: now, Running: 4, WorkersOnline: 2, WorkersTotal: 2}}
	got := platformStatistics(entries, []AccountUsage{a, a, stale}, samples, now)
	if got.Users != 2 || got.Accounts != 1 || got.Repositories != 2 || got.Active != 1 || got.Pending != 1 || got.Attention != 1 || got.Disconnected != 1 {
		t.Fatalf("enrollment summary: %+v", got)
	}
	if got.Completed != 2 || got.Minutes != "2.0" || got.UsageScopes != 1 {
		t.Fatalf("usage duplicated or expired: %+v", got)
	}
	if got.Reports != 2 || got.PeakAssigned != 4 || got.AverageAssigned != "3.0" || got.PartialReports != 1 {
		t.Fatalf("report summary: %+v", got)
	}
	empty := platformStatistics(nil, nil, nil, now)
	if empty.UsageScopes != 0 || empty.AverageAssigned != "unavailable" {
		t.Fatalf("empty inferred: %+v", empty)
	}
}
