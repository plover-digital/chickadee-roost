package site

import (
	"testing"
	"time"
)

func TestUserTotalUsageDistinctReposOrgDedupAndAuthorization(t *testing.T) {
	day := time.Now().UTC().Format("2006-01-02")
	makeEntry := func(id string, repo int64, org bool, seconds float64) Enrollment {
		a := Account{ID: 7, Type: "User"}
		if org {
			a.Type = "Organization"
			a.ID = 10
		}
		installation := int64(8)
		if org {
			installation = 9
		}
		return Enrollment{ID: id, User: User{ID: 7}, InstallationID: installation, Repository: Repository{ID: repo}, Account: a, Updated: time.Now(), Usage: []UsageDay{{Date: day, VMSeconds: seconds, VMs: 1}}}
	}
	a := makeEntry("personal1", 1, false, 60)
	b := makeEntry("personal2", 2, false, 120)
	o1 := makeEntry("org1", 3, true, 180)
	o2 := makeEntry("org2", 4, true, 240)
	o1.Updated = o2.Updated.Add(-time.Hour)
	denied := makeEntry("lost-role", 5, false, 6000)
	other := makeEntry("other-user", 6, false, 6000)
	other.User.ID = 99
	p := page{User: &User{ID: 7}, Enrollments: []Enrollment{a, b, o1, o2, denied, other}}
	for _, repo := range []int64{1, 2, 3, 4, 6} {
		p.Choices = append(p.Choices, Choice{Installation: Installation{ID: 8, Account: Account{ID: 7, Type: "User"}}, Repository: Repository{ID: repo}})
	}
	for i := range p.Choices {
		if p.Choices[i].Repository.ID == 3 || p.Choices[i].Repository.ID == 4 {
			p.Choices[i].Installation = Installation{ID: 9, Account: Account{ID: 10, Type: "Organization"}}
		}
	}
	p.AccountUsage = []AccountUsage{{ObservedAt: time.Now().UTC(), InstallationID: 9, AccountID: 10, AccountType: "Organization", RepositoryIDs: []int64{3, 4}, Usage: o2.Usage}}
	chart := p.TotalUsage().UsageChart()
	if chart.TotalMinutes != "7.0" || chart.TotalVMs != 3 {
		t.Fatalf("total %+v", chart)
	}
	p.User = nil
	if len(p.TotalUsage().Usage) != 0 {
		t.Fatal("anonymous usage leaked")
	}
}
