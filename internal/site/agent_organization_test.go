package site

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func agentOrganizationFixture(t *testing.T) (*Server, string, Enrollment) {
	t.Helper()
	s, token := agentFixture(t)
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/user/installations":
			return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":77,"login":"example-org","type":"Organization"},"permissions":{"organization_self_hosted_runners":"write","members":"read"}}]}`), nil
		case "/user/memberships/orgs/example-org":
			return response(`{"state":"active","role":"admin","organization":{"id":77,"login":"example-org"},"user":{"id":7,"login":"tester"}}`), nil
		default:
			return response(`{"total_count":2,"repositories":[{"id":99,"full_name":"example-org/repo","permissions":{"admin":true}},{"id":100,"full_name":"example-org/other","permissions":{"admin":true}}]}`), nil
		}
	})
	e := Enrollment{ID: "org-service", User: User{7, "tester"}, InstallationID: 8, Account: Account{77, "example-org", "Organization"}, Repository: Repository{ID: 99, Name: "example-org/repo"}, Scope: "organization", Status: "active", DesiredState: "active", Queues: []string{"chickadee"}, EnabledQueues: []string{"chickadee"}, EnabledWorkflowAccess: "repository"}
	return s, token, e
}
func TestAgentOrganizationPoliciesRemainBrowserOnly(t *testing.T) {
	for _, mode := range []string{"all-desired", "all-applied", "selected", "selected-single", "owner-takeover"} {
		t.Run(mode, func(t *testing.T) {
			s, token, e := agentOrganizationFixture(t)
			policy := &OrganizationAccess{RepositorySelection: "all"}
			if strings.HasPrefix(mode, "selected") {
				policy = &OrganizationAccess{RepositorySelection: "selected", RepositoryIDs: []int64{99}}
				if mode == "selected" {
					policy.RepositoryIDs = append(policy.RepositoryIDs, 100)
				}
			}
			if mode == "all-applied" {
				e.AppliedOrganizationAccess = policy
			} else {
				e.OrganizationAccess = policy
			}
			if mode == "owner-takeover" {
				e.User = User{9, "other-owner"}
			}
			s.enrollments = []Enrollment{e}
			got := agentCall(s, token, "GET", "/api/v1/enrollments", "")
			if got.Code != 200 || strings.Contains(got.Body.String(), e.ID) {
				t.Fatal("repository token disclosed organization service")
			}
			got = agentCall(s, token, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"disconnected"}`)
			if got.Code != 403 || !strings.Contains(got.Body.String(), `"error"`) || s.enrollments[0].DesiredState != "active" {
				t.Fatal("organization owner bypassed API repository authority")
			}
			got = agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99,"queues":["chickadee-small-ubuntu-2604"]}`)
			if got.Code != 403 || !strings.Contains(got.Body.String(), `"error"`) || len(s.enrollments[0].Queues) != 1 || s.enrollments[0].Queues[0] != "chickadee" {
				t.Fatal("API changed organization policy or queues")
			}
			got = agentCall(s, token, "GET", "/api/v1/repositories", "")
			if got.Code != 200 || strings.Contains(got.Body.String(), "example-org/other") || !strings.Contains(got.Body.String(), "browser owner setup") {
				t.Fatal("organization repository discovery broadened or hid setup requirement")
			}
			// The browser's verified organization-owner authority is intentionally broader.
			browser := agentForm(s, "/manage", url.Values{"id": {e.ID}, "action": {"paused"}}, "csrf", "https://chickadee.run", "session")
			if browser.Code != 303 || s.enrollments[0].DesiredState != "paused" {
				t.Fatal("API guard blocked browser organization management")
			}
		})
	}
}
func TestAgentLegacyOrganizationIdentityRemainsExact(t *testing.T) {
	for _, mode := range []string{"own", "other-owner", "other-repository", "other-installation"} {
		t.Run(mode, func(t *testing.T) {
			s, token, e := agentOrganizationFixture(t)
			switch mode {
			case "other-owner":
				e.User = User{9, "other-owner"}
			case "other-repository":
				e.Repository.ID = 100
			case "other-installation":
				e.InstallationID = 88
			}
			s.enrollments = []Enrollment{e}
			got := agentCall(s, token, "GET", "/api/v1/enrollments", "")
			if got.Code != 200 {
				t.Fatal("legacy lookup failed")
			}
			if strings.Contains(got.Body.String(), e.ID) != (mode == "own") {
				t.Fatal("legacy service projection escaped exact identity")
			}
			got = agentCall(s, token, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"paused"}`)
			expected := 403
			if mode == "own" {
				expected = 202
			}
			if got.Code != expected {
				t.Fatalf("legacy manage returned %d, want %d", got.Code, expected)
			}
			if mode == "own" {
				got = agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99,"queues":["chickadee"]}`)
				if got.Code != 202 || s.enrollments[0].OrganizationAccess != nil {
					t.Fatal("legacy API maintenance changed policy or failed")
				}
			}
		})
	}
}
func TestAgentNewOrganizationSetupRequiresBrowser(t *testing.T) {
	s, token, _ := agentOrganizationFixture(t)
	got := agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99,"workflow_path":".github/workflows/build.yml"}`)
	if got.Code != 403 || len(s.enrollments) != 0 || !strings.Contains(got.Body.String(), `"error"`) {
		t.Fatal("versionless API created new organization enrollment")
	}
	got = agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99,"organization_access_version":1,"repository_access":"all"}`)
	if got.Code != 400 || len(s.enrollments) != 0 {
		t.Fatal("JSON API accepted browser organization policy fields")
	}
}

func TestAgentCannotUseLegacyRowAfterOrganizationConversion(t *testing.T) {
	s, token, e := agentOrganizationFixture(t)
	s.enrollments = []Enrollment{e}
	browser := agentForm(s, "/enroll", url.Values{"installation_id": {"8"}, "repository_id": {"99"}, "organization_access_version": {"1"}, "repository_access": {"all"}}, "csrf", "https://chickadee.run", "session")
	if browser.Code != 303 || len(s.enrollments) != 1 || s.enrollments[0].OrganizationAccess == nil {
		t.Fatalf("browser conversion failed: %d %s", browser.Code, browser.Body.String())
	}
	if got := agentCall(s, token, "GET", "/api/v1/enrollments", ""); got.Code != 200 || strings.Contains(got.Body.String(), e.ID) {
		t.Fatal("converted service remained visible through repository token")
	}
	if agentCall(s, token, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"paused"}`).Code != 403 {
		t.Fatal("old agent token controlled converted organization service")
	}
	// A retained legacy row must not bypass the canonical organization's policy.
	e.ID = "legacy-leftover"
	e.Status = "paused"
	s.enrollments = append(s.enrollments, e)
	if got := agentCall(s, token, "GET", "/api/v1/enrollments", ""); got.Code != 200 || strings.Contains(got.Body.String(), "legacy-leftover") {
		t.Fatal("legacy leftover disclosed broader organization scope")
	}
	if agentCall(s, token, "POST", "/api/v1/enrollments/legacy-leftover/state", `{"state":"active"}`).Code != 403 {
		t.Fatal("legacy leftover bypassed canonical organization scope")
	}
}
