package site

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCurrentActivityAuthorizationFreshnessAndSectionOrder(t *testing.T) {
	now := time.Now().UTC()
	account := Account{ID: 7, Type: "User"}
	sample := AccountUsage{ObservedAt: now, LiveAt: now, InstallationID: 8, AccountID: 7, AccountType: "User", RepositoryIDs: []int64{99}, Runners: []RunnerActivity{{Label: "chickadee", Allocated: 2, Credentialed: 1}}}
	p := page{Title: "Your repositories", User: &User{ID: 7}, AccountUsage: []AccountUsage{sample}, Choices: []Choice{{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 99}}}}
	view := p.CurrentActivity()
	if !view.Available || len(view.Runners) != 1 || view.Runners[0].Allocated != 2 {
		t.Fatal("live counts absent")
	}
	s := fixture(t)
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, "page.html", p); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	graph := strings.Index(html, "Your dashboard")
	current := strings.Index(html, "<h2>Current runners</h2>")
	setup := strings.Index(html, "Repository and organization setup")
	status := strings.Index(html, "<h2>Service status</h2>")
	if !(graph < current && current < setup && setup < status) {
		t.Fatalf("order %d %d %d %d", graph, current, setup, status)
	}
	p.AccountUsage[0].LiveAt = now.Add(-4 * time.Minute)
	if p.CurrentActivity().Available {
		t.Fatal("stale counted")
	}
	p.AccountUsage[0] = sample
	p.Choices = nil
	if p.CurrentActivity().Available {
		t.Fatal("unauthorized live counts leaked")
	}
	p.Choices = []Choice{{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 99}}}
	p.AccountUsage[0].Runners = nil
	if !p.CurrentActivity().Available || len(p.CurrentActivity().Runners) != 0 {
		t.Fatal("fresh empty lost")
	}
}
