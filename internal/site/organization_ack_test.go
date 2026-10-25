package site

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func enrollLargestOrganizationPolicy(t *testing.T) (*Server, *OrganizationAccess) {
	t.Helper()
	s := orgAccessFixture(t)
	original := s.http.Transport
	var repositories []Repository
	form := url.Values{
		"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"},
		"organization_access_version": {"1"}, "repository_access": {"selected"},
	}
	for i := range 100 {
		id := int64(99 + i)
		repositories = append(repositories, Repository{
			ID: id, Name: fmt.Sprintf("example/repo-%d", i),
			Permissions: map[string]bool{"admin": true},
		})
		form.Add("selected_repository", strconv.FormatInt(id, 10))
	}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/user/installations/8/repositories" {
			body, err := json.Marshal(struct {
				Total        int          `json:"total_count"`
				Repositories []Repository `json:"repositories"`
			}{100, repositories})
			if err != nil {
				t.Fatal(err)
			}
			return response(string(body)), nil
		}
		return original.RoundTrip(r)
	})
	var workflows []string
	for i := range 50 {
		prefix := fmt.Sprintf("example/repo-0/.github/workflows/ci%d.yml@refs/heads/", i)
		workflows = append(workflows, prefix+strings.Repeat("x", 300-len(prefix)))
	}
	form.Set("allowed_workflows", strings.Join(workflows, "\n"))
	if w := orgPost(s, form); w.Code != http.StatusSeeOther {
		t.Fatalf("maximum policy enrollment: %d %s", w.Code, w.Body.String())
	}
	if len(s.enrollments) != 1 {
		t.Fatalf("got %d enrollment records", len(s.enrollments))
	}
	e := s.enrollments[0]
	if e.AppliedOrganizationAccess != nil || len(e.EnabledQueues) != 0 || !e.WorkflowChangesPending() {
		t.Fatal("enrollment claimed applied organization policy before acknowledgment")
	}
	if !validOrganizationAccess(e.OrganizationAccess) || len(e.OrganizationAccess.RepositoryIDs) != 100 || len(e.OrganizationAccess.Workflows) != 50 {
		t.Fatal("maximum policy was not retained")
	}
	return s, e.OrganizationAccess
}

func organizationAcknowledgmentBody(t *testing.T, s *Server, policy *OrganizationAccess) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id": s.enrollments[0].ID, "status": "active",
		"enabled_queues": []string{"chickadee"}, "applied_organization_access": policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 8192 || len(body) >= 32<<10 {
		t.Fatalf("policy acknowledgment has unexpected size %d", len(body))
	}
	return body
}

func TestOrganizationStatusAcknowledgesMaximumWorkflowAndRepositoryPolicy(t *testing.T) {
	s, policy := enrollLargestOrganizationPolicy(t)
	body := organizationAcknowledgmentBody(t, s, policy)
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/status", bytes.NewReader(body)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("maximum policy acknowledgment: %d %s", w.Code, w.Body.String())
	}
	e := s.enrollments[0]
	if e.Status != "active" || !reflect.DeepEqual(e.AppliedOrganizationAccess, policy) || e.WorkflowChangesPending() {
		t.Fatal("successful acknowledgment did not record the applied policy")
	}
	if !reflect.DeepEqual(e.EnabledQueues, []string{"chickadee"}) {
		t.Fatal("successful acknowledgment did not record the enabled queue")
	}
}

func TestOrganizationStatusRejectsOver32KiBWithoutApplyingPolicy(t *testing.T) {
	s, policy := enrollLargestOrganizationPolicy(t)
	body := organizationAcknowledgmentBody(t, s, policy)
	// Valid trailing JSON whitespace exercises the transport limit rather than
	// an unrelated invalid field or malformed policy rejection.
	body = append(body, bytes.Repeat([]byte(" "), (32<<10)+1-len(body))...)
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/status", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized policy acknowledgment: %d %s", w.Code, w.Body.String())
	}
	e := s.enrollments[0]
	if e.Status == "active" || e.AppliedOrganizationAccess != nil || len(e.EnabledQueues) != 0 {
		t.Fatal("oversized acknowledgment changed applied state")
	}
}
