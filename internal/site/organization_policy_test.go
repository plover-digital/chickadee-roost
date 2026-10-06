package site

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOrganizationSecondRepoFailsActionablyWithoutTenantDisclosure(t *testing.T) {
	s := fixture(t)
	s.cfg.AutomaticActivation = true
	user := User{7, "tester"}
	org := Account{10, "example", "Organization"}
	s.sessions["session"] = session{User: user, Token: "fixture", CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
	s.enrollments = []Enrollment{{ID: "established", User: user, InstallationID: 8, Account: org, Repository: Repository{ID: 1, Name: "example/hidden-established"}, Status: "active", Queues: []string{"chickadee"}, EnabledQueues: []string{"chickadee"}, Created: time.Now().Add(-time.Hour)}}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/user/installations/") {
			return response(`{"total_count":1,"repositories":[{"id":2,"full_name":"example/requested","permissions":{"admin":true}}]}`), nil
		}
		return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":10,"login":"example","type":"Organization"},"permissions":{"organization_self_hosted_runners":"write"}}]}`), nil
	})
	form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"2"}}
	r := httptest.NewRequest("POST", "/enroll", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://chickadee.run")
	r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 409 || len(s.enrollments) != 1 || !strings.Contains(w.Body.String(), "one selected repository") || strings.Contains(w.Body.String(), "hidden-established") {
		t.Fatal("second repository not rejected safely")
	}
	r = httptest.NewRequest("GET", "/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), `action="/enroll"`) || !strings.Contains(w.Body.String(), "operator reconciliation") {
		t.Fatal("blocked org still offers enrollment form")
	}
}
func TestEstablishedOrganizationScopeBeatsErroneousRequest(t *testing.T) {
	org := Account{ID: 10, Type: "Organization"}
	existing := Enrollment{ID: "existing", User: User{ID: 7}, InstallationID: 8, Account: org, Repository: Repository{ID: 1}, Status: "active", Updated: time.Now().Add(-time.Hour)}
	bad := existing
	bad.ID = "bad"
	bad.Repository.ID = 2
	bad.Status = "error"
	bad.Updated = time.Now()
	choice := Choice{Installation: Installation{ID: 8, Account: org}, Repository: existing.Repository}
	if organizationConstraint([]Enrollment{existing, bad}, User{ID: 7}, choice) != "" {
		t.Fatal("failed second request blocks established editor")
	}
	for _, change := range []string{"repo", "requester", "installation"} {
		t.Run(change, func(t *testing.T) {
			c := choice
			user := User{ID: 7}
			switch change {
			case "repo":
				c.Repository.ID = 2
			case "requester":
				user.ID = 9
			case "installation":
				c.Installation.ID = 9
			}
			if organizationConstraint([]Enrollment{existing}, user, c) == "" {
				t.Fatal(fmt.Sprint("conflicting ", change, " allowed"))
			}
		})
	}
	p := page{Enrollments: []Enrollment{existing, bad}, Choices: []Choice{choice, {Installation: choice.Installation, Repository: bad.Repository}}}
	services := p.Services()
	if len(services) != 1 || services[0].ID != "existing" || len(services[0].Repositories) != 1 {
		t.Fatal("unapplied second request misrepresents current pool")
	}
}
