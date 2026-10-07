package site

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountUsagePrimaryScopeAuthorizationAndDedup(t *testing.T) {
	now := time.Now().UTC()
	account := Account{ID: 56267830, Type: "Organization"}
	snapshot := AccountUsage{ObservedAt: now, InstallationID: 8, AccountID: account.ID, AccountType: account.Type, RepositoryIDs: []int64{99, 100}, Usage: []UsageDay{{Date: now.Format("2006-01-02"), VMSeconds: 600, VMs: 2}}}
	p := page{User: &User{ID: 38401861}, AccountUsage: []AccountUsage{snapshot}, Choices: []Choice{{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 99}}}}
	if len(p.TotalUsage().Usage) != 0 {
		t.Fatal("partial repository administration leaked org total")
	}
	p.Choices = append(p.Choices, Choice{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 100}})
	if p.TotalUsage().UsageChart().TotalMinutes != "10.0" {
		t.Fatal("operator scope without enrollment missing")
	}
	p.Enrollments = []Enrollment{{ID: "duplicate", User: *p.User, Account: account, InstallationID: 8, Repository: Repository{ID: 99}, Usage: snapshot.Usage}}
	if p.TotalUsage().UsageChart().TotalMinutes != "10.0" {
		t.Fatal("scope snapshot doubled")
	}
	p.AccountUsage[0].ObservedAt = now.Add(-6 * time.Minute)
	p.Enrollments = nil
	if len(p.TotalUsage().Usage) != 0 {
		t.Fatal("stale ACL published")
	}
	p.AccountUsage[0] = snapshot
	p.AccountUsage[0].AccountType = "User"
	p.AccountUsage[0].AccountID = 7
	p.AccountUsage[0].RepositoryIDs = []int64{99}
	p.Choices[0].Installation.Account = Account{ID: 7, Type: "User"}
	if len(p.TotalUsage().Usage) != 0 {
		t.Fatal("other personal owner leaked")
	}
}
func TestAccountUsagePrivateImportReplaceAndPersistence(t *testing.T) {
	s := fixture(t)
	now := time.Now().UTC()
	sample := AccountUsage{ObservedAt: now, InstallationID: 8, AccountID: 7, AccountType: "User", RepositoryIDs: []int64{99}, Usage: []UsageDay{{Date: now.Format("2006-01-02"), VMSeconds: 60, VMs: 1}}}
	post := func(entries []AccountUsage, public bool) int {
		data, _ := json.Marshal(entries)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/account-usage", bytes.NewReader(data))
		if public {
			s.ServeHTTP(w, r)
		} else {
			s.AdminHandler().ServeHTTP(w, r)
		}
		return w.Code
	}
	if post([]AccountUsage{sample}, true) != 404 {
		t.Fatal("public import exposed")
	}
	if post([]AccountUsage{sample}, false) != 204 {
		t.Fatal("import failed")
	}
	restored, err := New(s.cfg)
	if err != nil || len(restored.accountUsage) != 1 {
		t.Fatal("restart", err)
	}
	sample.RepositoryIDs = []int64{99, 99}
	if post([]AccountUsage{sample}, false) != 400 {
		t.Fatal("duplicate ACL accepted")
	}
	if post([]AccountUsage{}, false) != 204 || len(s.accountUsage) != 0 {
		t.Fatal("omitted scope retained")
	}
}
