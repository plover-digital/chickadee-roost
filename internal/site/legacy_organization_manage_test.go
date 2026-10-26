package site

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyOrganizationLifecycleRecordsVerifiedActingOwner(t *testing.T) {
	for _, tc := range []struct{ action, status string }{{"paused", "active"}, {"disconnected", "active"}, {"active", "active"}, {"active", "paused"}, {"active", "disconnected"}} {
		t.Run(tc.action+"-from-"+tc.status, func(t *testing.T) {
			action := tc.action
			s := orgAccessFixture(t)
			original := Enrollment{ID: "legacy", User: User{8, "former-owner"}, InstallationID: 8, Account: Account{10, "example", "Organization"}, Repository: Repository{ID: 99, Name: "example/one", Permissions: map[string]bool{"admin": true}}, Status: "active", DesiredState: "active", Queues: []string{"chickadee"}, EnabledQueues: []string{"chickadee"}, WorkflowPath: ".github/workflows/restricted.yml", EnabledWorkflowPath: ".github/workflows/restricted.yml", EnabledWorkflowAccess: "workflow"}
			original.Status = tc.status
			s.enrollments = []Enrollment{original}
			form := url.Values{"csrf": {"csrf"}, "id": {"legacy"}, "action": {action}}
			req := httptest.NewRequest("POST", "/manage", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", s.cfg.PublicURL)
			req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("manage: %d %s", w.Code, w.Body.String())
			}
			got := s.enrollments[0]
			if action == "active" && tc.status == "active" {
				if got.User != original.User || got.Authenticated != original.Authenticated {
					t.Fatal("legacy active refresh changed requesting identity")
				}
			} else if got.User != s.sessions["session"].User || !got.Authenticated {
				t.Fatal("legacy lifecycle retained former owner's identity")
			}
			if got.DesiredState != action {
				t.Fatal("stop intent was not stored")
			}
			if got.OrganizationAccess != nil || got.AppliedOrganizationAccess != nil || got.WorkflowPath != original.WorkflowPath || got.EnabledWorkflowPath != original.EnabledWorkflowPath || got.EnabledWorkflowAccess != original.EnabledWorkflowAccess || !reflect.DeepEqual(got.EnabledQueues, original.EnabledQueues) || !reflect.DeepEqual(got.Queues, original.Queues) || got.Status != original.Status {
				t.Fatal("legacy control changed policy or claimed applied state")
			}
		})
	}
}

func TestLegacyOrganizationStopIdentityRequiresFreshOwnerAndAgentAuthority(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "repository-admin", true: "scoped-agent"}[scoped], func(t *testing.T) {
			s := orgAccessFixture(t)
			original := Enrollment{ID: "legacy", User: User{7, "owner"}, InstallationID: 8, Account: Account{10, "example", "Organization"}, Repository: Repository{ID: 99, Name: "example/one"}, Status: "active", DesiredState: "active"}
			if scoped {
				original.User = User{8, "former-owner"}
				v := s.sessions["session"]
				v.AgentRepositories = map[int64]bool{99: true}
				s.sessions["session"] = v
			} else {
				old := s.http.Transport
				s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/user/memberships/orgs/example" {
						return response(`{"state":"active","role":"member","organization":{"id":10},"user":{"id":7}}`), nil
					}
					return old.RoundTrip(r)
				})
			}
			s.enrollments = []Enrollment{original}
			req := httptest.NewRequest("POST", "/manage", strings.NewReader("csrf=csrf&id=legacy&action=disconnected"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", s.cfg.PublicURL)
			req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if scoped {
				if w.Code != http.StatusForbidden || !reflect.DeepEqual(s.enrollments[0], original) {
					t.Fatalf("agent took over legacy organization: %d", w.Code)
				}
			} else if w.Code != http.StatusSeeOther || s.enrollments[0].Authenticated || s.enrollments[0].User != original.User {
				t.Fatalf("repository admin gained owner provenance: %d", w.Code)
			}
		})
	}
}

func TestLegacyOrganizationOwnerLifecycleDoesNotRequireOldRepositoryAnchor(t *testing.T) {
	for _, tc := range []struct{ action, status string }{{"paused", "active"}, {"disconnected", "active"}, {"active", "paused"}, {"active", "disconnected"}} {
		t.Run(tc.action+"-from-"+tc.status, func(t *testing.T) {
			for _, owner := range []bool{true, false} {
				t.Run(map[bool]string{true: "owner", false: "repository-admin"}[owner], func(t *testing.T) {
					s := orgAccessFixture(t)
					// The installation still has other repositories, but no longer has the
					// legacy anchor. Membership independently verifies the organization owner.
					original := Enrollment{ID: "legacy", User: User{7, "owner"}, InstallationID: 8, Account: Account{10, "example", "Organization"}, Repository: Repository{ID: 101, Name: "example/removed", Permissions: map[string]bool{"admin": true}}, Status: tc.status, DesiredState: tc.status, Queues: []string{"chickadee"}, EnabledQueues: []string{"chickadee"}, WorkflowPath: ".github/workflows/restricted.yml", EnabledWorkflowPath: ".github/workflows/restricted.yml", EnabledWorkflowAccess: "workflow"}
					if owner {
						original.User = User{8, "former-owner"}
					} else {
						old := s.http.Transport
						s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
							if r.URL.Path == "/user/memberships/orgs/example" {
								return response(`{"state":"active","role":"member","organization":{"id":10},"user":{"id":7}}`), nil
							}
							return old.RoundTrip(r)
						})
					}
					s.enrollments = []Enrollment{original}
					form := url.Values{"csrf": {"csrf"}, "id": {"legacy"}, "action": {tc.action}}
					req := httptest.NewRequest("POST", "/manage", strings.NewReader(form.Encode()))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.Header.Set("Origin", s.cfg.PublicURL)
					req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
					w := httptest.NewRecorder()
					s.ServeHTTP(w, req)
					if !owner {
						if w.Code != http.StatusForbidden || !reflect.DeepEqual(s.enrollments[0], original) {
							t.Fatalf("repository admin bypassed missing anchor: %d", w.Code)
						}
						return
					}
					if w.Code != http.StatusSeeOther {
						t.Fatalf("verified owner blocked by old anchor: %d %s", w.Code, w.Body.String())
					}
					got := s.enrollments[0]
					if got.User != s.sessions["session"].User || !got.Authenticated || got.DesiredState != tc.action {
						t.Fatal("owner lifecycle identity/intent not stored")
					}
					// Only request provenance, intent, and timestamp may change before ACK.
					got.User = original.User
					got.Authenticated = original.Authenticated
					got.DesiredState = original.DesiredState
					got.Updated = original.Updated
					if !reflect.DeepEqual(got, original) {
						t.Fatal("missing-anchor lifecycle changed legacy policy or applied state")
					}
				})
			}
		})
	}
}

func TestLegacyOrganizationDashboardOwnerLifecycleWithoutAnchor(t *testing.T) {
	for _, status := range []string{"active", "paused", "disconnected", "permission-required"} {
		t.Run(status, func(t *testing.T) {
			for _, owner := range []bool{true, false} {
				t.Run(map[bool]string{true: "owner", false: "repository-admin"}[owner], func(t *testing.T) {
					s := orgAccessFixture(t)
					original := Enrollment{ID: "legacy", User: User{7, "owner"}, InstallationID: 8, Account: Account{10, "example", "Organization"}, Repository: Repository{ID: 101, Name: "example/removed"}, Scope: "organization", Status: status, DesiredState: status, Queues: []string{"chickadee"}, EnabledQueues: []string{"chickadee"}, Usage: []UsageDay{{Date: "2026-10-25", VMSeconds: 987654, VMs: 4321}}}
					if !owner {
						old := s.http.Transport
						s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
							if r.URL.Path == "/user/memberships/orgs/example" {
								return response(`{"state":"active","role":"member","organization":{"id":10},"user":{"id":7}}`), nil
							}
							return old.RoundTrip(r)
						})
					}
					s.enrollments = []Enrollment{original}
					req := httptest.NewRequest("GET", "/dashboard", nil)
					req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
					w := httptest.NewRecorder()
					s.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
					}
					body := w.Body.String()
					if strings.Contains(body, `action="/manage"`) != owner {
						t.Fatal("lifecycle controls did not match verified owner access")
					}
					if owner {
						wantAction := `value="active"`
						if status == "active" {
							wantAction = `value="paused"`
						}
						start := strings.Index(body, `action="/manage"`)
						end := strings.Index(body[start:], "</form>")
						if end < 0 {
							t.Fatal("management form not closed")
						}
						form := body[start : start+end]
						if !strings.Contains(form, wantAction) || !strings.Contains(body, `<span class="status">`+status+`</span>`) {
							t.Fatal("owner lifecycle view lost applied status or offered wrong control")
						}
					}
					if strings.Contains(body, "16460.9") || strings.Contains(body, "4321") || strings.Contains(body, "Only enabled labels are listed") {
						t.Fatal("lifecycle owner view disclosed scrubbed usage or runner setup")
					}
					if !reflect.DeepEqual(s.enrollments[0], original) {
						t.Fatal("dashboard changed persisted service state")
					}
				})
			}
		})
	}
}
