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

func orgAccessFixture(t *testing.T) *Server {
	s := fixture(t)
	s.cfg.AutomaticActivation = true
	s.sessions["session"] = session{User: User{7, "owner"}, Token: "fixture-token", CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/user/installations":
			return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":10,"login":"example","type":"Organization"},"permissions":{"organization_self_hosted_runners":"write","members":"read"}}]}`), nil
		case "/user/memberships/orgs/example":
			return response(`{"state":"active","role":"admin","organization":{"id":10},"user":{"id":7}}`), nil
		default:
			return response(`{"total_count":2,"repositories":[{"id":99,"full_name":"example/one","permissions":{"admin":true}},{"id":100,"full_name":"example/two","permissions":{"admin":true}}]}`), nil
		}
	})
	return s
}
func orgPost(s *Server, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/enroll", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.cfg.PublicURL)
	req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}
func TestOrganizationDefaultAndSelectedEnrollmentStayOneScope(t *testing.T) {
	s := orgAccessFixture(t)
	form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"}, "organization_access_version": {"1"}, "repository_access": {"all"}}
	w := orgPost(s, form)
	if w.Code != 303 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	if len(s.enrollments) != 1 || s.enrollments[0].OrganizationAccess.RepositorySelection != "all" {
		t.Fatal("missing all-repository policy")
	}
	form.Set("repository_id", "100")
	form.Set("repository_access", "selected")
	form["selected_repository"] = []string{"99", "100"}
	form.Set("allowed_workflows", "example/two/.github/workflows/ci.yml@refs/heads/main")
	w = orgPost(s, form)
	if w.Code != 303 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	e := s.enrollments[0]
	if len(s.enrollments) != 1 || len(e.OrganizationAccess.RepositoryIDs) != 2 || e.Repository.ID != 100 || !e.WorkflowChangesPending() {
		t.Fatal("update duplicated scope or claimed applied access")
	}
	form.Del("organization_access_version")
	if orgPost(s, form).Code != 403 {
		t.Fatal("legacy form erased organization authority boundary")
	}
}
func TestOrganizationPolicyRequiresCurrentOwnerAndSelectedRepositoryMembership(t *testing.T) {
	s := orgAccessFixture(t)
	choices, err := s.choices(t.Context(), s.sessions["session"])
	if err != nil || !choices[0].OrganizationAdmin {
		t.Fatal("owner lookup failed")
	}
	form := url.Values{"organization_access_version": {"1"}, "repository_access": {"selected"}, "selected_repository": {"99", "100"}}
	c := choices[0]
	c.OrganizationAdmin = false
	if _, err = requestedOrganizationAccess(form, c); err == nil {
		t.Fatal("repository admin broadened organization")
	}
	form["selected_repository"] = []string{"101"}
	if _, err = requestedOrganizationAccess(form, choices[0]); err == nil {
		t.Fatal("foreign repository allowed")
	}
	form["selected_repository"] = []string{"99"}
	form.Set("allowed_workflows", "example/two/.github/workflows/ci.yml@refs/heads/main")
	if _, err = requestedOrganizationAccess(form, choices[0]); err == nil {
		t.Fatal("workflow outside selection allowed")
	}
}
func TestOrganizationOwnerDefaultAndLegacyEditorPreserveRestrictedSelection(t *testing.T) {
	c := Choice{Repository: Repository{ID: 99}}
	if c.AccessSelection() != "all" {
		t.Fatal("new organization default is not all")
	}
	c.SelectedQueues = []string{"chickadee"}
	if c.AccessSelection() != "selected" || !c.RepositorySelected(99) {
		t.Fatal("legacy restriction broadened")
	}
}
func TestAllRepositoryUsageRequiresOrganizationOwner(t *testing.T) {
	now := time.Now()
	a := AccountUsage{RepositorySelection: "all", ObservedAt: now, InstallationID: 8, AccountID: 10, AccountType: "Organization", RepositoryIDs: []int64{99}}
	p := page{User: &User{ID: 7}, Choices: []Choice{{Installation: Installation{ID: 8, Account: Account{ID: 10, Type: "Organization"}}, Repository: Repository{ID: 99}}}}
	if a.authorized(p) {
		t.Fatal("repository admin saw organization usage")
	}
	p.Choices[0].OrganizationAdmin = true
	if !a.authorized(p) {
		t.Fatal("verified owner denied")
	}
}

func TestAnotherVerifiedOwnerUpdatesExistingOrganizationRecord(t *testing.T) {
	s := orgAccessFixture(t)
	s.enrollments = []Enrollment{{ID: "org", User: User{8, "former-owner"}, Account: Account{10, "example", "Organization"}, InstallationID: 8, Repository: Repository{ID: 99, Name: "example/one"}, Status: "active", Queues: []string{"chickadee"}}}
	form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"100"}, "organization_access_version": {"1"}, "repository_access": {"selected"}, "selected_repository": {"99", "100"}, "confirm_all_workflows": {"1"}}
	if w := orgPost(s, form); w.Code != 303 {
		t.Fatalf("verified owner edit: %d %s", w.Code, w.Body.String())
	}
	if len(s.enrollments) != 1 || s.enrollments[0].ID != "org" || s.enrollments[0].User.ID != 7 {
		t.Fatal("owner edit created another scope or kept revoked requester")
	}
}

func TestLegacyPolicyUsesEnrolledRepositoryWhenOwnerEditorStartsWithAnotherRepo(t *testing.T) {
	c := Choice{Repository: Repository{ID: 1, Name: "example/first"}, LegacyRepository: Repository{ID: 2, Name: "example/enrolled"}, SelectedQueues: []string{"chickadee"}, WorkflowPath: ".github/workflows/ci.yml"}
	if c.RepositorySelected(1) || !c.RepositorySelected(2) || c.WorkflowReferences() != "example/enrolled/.github/workflows/ci.yml@refs/heads/main" {
		t.Fatal("legacy editor silently changed repository boundary")
	}
	c.EnabledWorkflowAccess = "repository"
	if c.WorkflowReferences() != "" {
		t.Fatal("repository-wide policy gained a stale workflow restriction")
	}
}

func TestVersionlessOrganizationCannotCreateOrDuplicateScope(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprint(owner), func(t *testing.T) {
			s := orgAccessFixture(t)
			original := s.http.Transport
			if !owner {
				s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/user/memberships/orgs/example" {
						return response(`{"state":"active","role":"member","organization":{"id":10},"user":{"id":7}}`), nil
					}
					return original.RoundTrip(r)
				})
			}
			form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"}}
			if w := orgPost(s, form); w.Code != 403 || len(s.enrollments) != 0 {
				t.Fatal("versionless new organization bypassed owner setup")
			}
			s.enrollments = []Enrollment{{ID: "org", User: User{8, "other-owner"}, Account: Account{10, "example", "Organization"}, InstallationID: 8, Repository: Repository{ID: 99, Name: "example/one"}, Status: "active", Queues: []string{"chickadee"}, OrganizationAccess: &OrganizationAccess{RepositorySelection: "all"}}}
			form.Set("repository_id", "100")
			if w := orgPost(s, form); w.Code != 403 || len(s.enrollments) != 1 {
				t.Fatal("versionless form duplicated organization scope")
			}
		})
	}
}

func TestOrganizationLookupDistinguishesUnavailableFromDenied(t *testing.T) {
	for _, code := range []int{403, 404, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := orgAccessFixture(t)
			old := s.http.Transport
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/user/memberships/orgs/example" {
					res := response(`{}`)
					res.StatusCode = code
					return res, nil
				}
				return old.RoundTrip(r)
			})
			choices, err := s.choices(t.Context(), s.sessions["session"])
			if code == 503 {
				if err == nil {
					t.Fatal("outage treated as lost permission")
				}
			} else if err != nil || choices[0].OrganizationAdmin {
				t.Fatal("denied role not handled safely")
			}
		})
	}
}

func TestLegacyMigrationKeepsAppliedWorkflowOrRequiresExplicitResolution(t *testing.T) {
	s := orgAccessFixture(t)
	s.enrollments = []Enrollment{{ID: "legacy", User: User{7, "owner"}, Account: Account{10, "example", "Organization"}, InstallationID: 8, Repository: Repository{ID: 99, Name: "example/one"}, Status: "active", Queues: []string{"chickadee"}, EnabledWorkflowAccess: "workflow", EnabledWorkflowPath: ".github/workflows/applied.yml"}}
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "example/one/.github/workflows/applied.yml@refs/heads/main") {
		t.Fatal("known applied workflow was lost in migration editor")
	}
	s.enrollments[0].EnabledWorkflowPath = ""
	form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"}, "organization_access_version": {"1"}, "repository_access": {"all"}}
	if w := orgPost(s, form); w.Code != 400 || s.enrollments[0].OrganizationAccess != nil {
		t.Fatal("unknown legacy workflow policy silently broadened")
	}
	form.Set("confirm_all_workflows", "1")
	if w := orgPost(s, form); w.Code != 303 || s.enrollments[0].OrganizationAccess == nil {
		t.Fatal("explicit workflow resolution was rejected")
	}
}
