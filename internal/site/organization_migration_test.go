package site

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOrganizationOwnerMigratesCanonicalRecordBeforeFailedLegacyRows(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%t", versioned), func(t *testing.T) {
			s := orgAccessFixture(t)
			account := Account{10, "example", "Organization"}
			canonical := Enrollment{ID: "established", User: User{8, "previous-owner"}, InstallationID: 8, Account: account, Repository: Repository{ID: 100, Name: "example/two"}, Status: "active", Queues: []string{"chickadee", "chickadee-small-rocky-102"}, EnabledQueues: []string{"chickadee", "chickadee-small-rocky-102"}, WorkflowPath: ".github/workflows/kept.yml", EnabledWorkflowPath: ".github/workflows/kept.yml", EnabledWorkflowAccess: "workflow", Created: time.Now().Add(-time.Hour)}
			if versioned {
				canonical.OrganizationAccess = &OrganizationAccess{RepositorySelection: "selected", RepositoryIDs: []int64{100}, Workflows: []string{"example/two/.github/workflows/kept.yml@refs/heads/main"}}
				canonical.AppliedOrganizationAccess = canonical.OrganizationAccess
			}
			failed := Enrollment{ID: "failed", User: User{7, "owner"}, InstallationID: 8, Account: account, Repository: Repository{ID: 99, Name: "example/one"}, Status: "error", Queues: []string{"chickadee"}, WorkflowPath: ".github/workflows/failed.yml"}
			otherInstallation := failed
			otherInstallation.ID = "other-installation"
			otherInstallation.InstallationID = 9
			otherAccount := failed
			otherAccount.ID = "other-account"
			otherAccount.Account = Account{11, "other", "Organization"}
			s.enrollments = []Enrollment{failed, canonical, otherInstallation, otherAccount}
			req := httptest.NewRequest("GET", "/dashboard", nil)
			req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
			}
			html := w.Body.String()
			start := strings.Index(html, `action="/enroll"`)
			if start < 0 {
				t.Fatal("organization editor missing")
			}
			end := strings.Index(html[start:], "</form>")
			if end < 0 {
				t.Fatal("organization editor not closed")
			}
			editor := html[start : start+end]
			for _, want := range []string{`value="100" checked`, `value="chickadee-small-rocky-102" checked`, "example/two/.github/workflows/kept.yml@refs/heads/main"} {
				if !strings.Contains(editor, want) {
					t.Fatalf("canonical editor missing %q: %s", want, editor)
				}
			}
			if strings.Contains(editor, "failed.yml") || strings.Contains(editor, `value="99" checked`) {
				t.Fatal("failed legacy policy replaced canonical editor defaults")
			}
			form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"}, "organization_access_version": {"1"}, "repository_access": {"all"}}
			if w := orgPost(s, form); w.Code != 303 {
				t.Fatalf("owner update: %d %s", w.Code, w.Body.String())
			}
			if len(s.enrollments) != 3 {
				t.Fatalf("same-scope duplicate not consolidated: %#v", s.enrollments)
			}
			var updated *Enrollment
			preserved := map[string]bool{}
			for i := range s.enrollments {
				e := &s.enrollments[i]
				if e.ID == canonical.ID {
					updated = e
				}
				preserved[e.ID] = true
			}
			if updated == nil || !preserved[otherInstallation.ID] || !preserved[otherAccount.ID] || preserved[failed.ID] {
				t.Fatal("owner migration changed unrelated identities or kept failed row")
			}
			if updated.User.ID != 7 || updated.Repository.ID != 99 || updated.OrganizationAccess.RepositorySelection != "all" {
				t.Fatal("canonical request was not updated by owner")
			}
			if !reflect.DeepEqual(updated.EnabledQueues, canonical.EnabledQueues) || !reflect.DeepEqual(updated.AppliedOrganizationAccess, canonical.AppliedOrganizationAccess) || !updated.WorkflowChangesPending() {
				t.Fatal("request changed applied policy/queues before acknowledgment")
			}
		})
	}
}

func TestOrganizationEnrollmentAcceptsBoundedLargeNativeForm(t *testing.T) {
	s := orgAccessFixture(t)
	repos := make([]Repository, 100)
	ids := make([]string, 100)
	for i := range repos {
		repos[i] = Repository{ID: int64(1000000000 + i), Name: fmt.Sprintf("example/project-%d", i), Permissions: map[string]bool{"admin": true}}
		ids[i] = strconv.FormatInt(repos[i].ID, 10)
	}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/user/installations":
			return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":10,"login":"example","type":"Organization"},"permissions":{"organization_self_hosted_runners":"write","members":"read"}}]}`), nil
		case "/user/memberships/orgs/example":
			return response(`{"state":"active","role":"admin","organization":{"id":10},"user":{"id":7}}`), nil
		default:
			b, err := json.Marshal(map[string]any{"total_count": 100, "repositories": repos})
			if err != nil {
				t.Fatal(err)
			}
			return response(string(b)), nil
		}
	})
	refs := make([]string, 20)
	for i := range refs {
		refs[i] = fmt.Sprintf("example/project-%d/.github/workflows/build.yml@refs/heads/main", i)
	}
	form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {ids[0]}, "organization_access_version": {"1"}, "repository_access": {"selected"}, "selected_repository": ids, "allowed_workflows": {strings.Join(refs, "\n")}}
	if len(form.Encode()) <= 4096 {
		t.Fatal("fixture does not exercise expanded form bound")
	}
	if w := orgPost(s, form); w.Code != 303 {
		t.Fatalf("large valid native form: %d %s", w.Code, w.Body.String())
	}
	if len(s.enrollments) != 1 || len(s.enrollments[0].OrganizationAccess.RepositoryIDs) != 100 || len(s.enrollments[0].OrganizationAccess.Workflows) != 20 {
		t.Fatal("large policy was not preserved")
	}
	form.Set("padding", strings.Repeat("x", 64<<10))
	if w := orgPost(s, form); w.Code != 403 {
		t.Fatalf("oversized enrollment accepted: %d", w.Code)
	}
	manage := url.Values{"csrf": {"csrf"}, "id": {s.enrollments[0].ID}, "action": {"paused"}, "padding": {strings.Repeat("x", 4096)}}
	req := httptest.NewRequest("POST", "/manage", strings.NewReader(manage.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.cfg.PublicURL)
	req.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("manage body limit broadened: %d", w.Code)
	}
}
