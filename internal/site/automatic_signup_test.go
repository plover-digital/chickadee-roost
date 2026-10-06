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

func TestAutomaticSignupOrgRequestsKeepWorkflowPolicyOnBackend(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			s := fixture(t)
			s.cfg.AutomaticActivation = true
			s.sessions["session"] = session{User: User{7, "tester"}, Token: "fixture", CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/user/installations/") {
					return response(fmt.Sprintf(`{"total_count":1,"repositories":[{"id":99,"full_name":"example/repo","private":%t,"permissions":{"admin":true}}]}`, private)), nil
				}
				return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":10,"login":"example","type":"Organization"},"permissions":{"organization_self_hosted_runners":"write"}}]}`), nil
			})
			form := url.Values{"csrf": {"csrf"}, "installation_id": {"8"}, "repository_id": {"99"}, "private": {"true"}, "enabled_workflow_access": {"repository"}}
			post := func(origin string) int {
				r := httptest.NewRequest("POST", "/enroll", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Origin", origin)
				r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w.Code
			}

			if post("null") != 403 || post("https://evil.example") != 403 || len(s.enrollments) != 0 {
				t.Fatal("automatic activation bypasses Origin protection")
			}
			form.Set("csrf", "bad")
			if post("https://chickadee.run") != 403 {
				t.Fatal("automatic activation bypasses CSRF")
			}
			form.Set("csrf", "csrf")
			if post("https://chickadee.run") != 303 || len(s.enrollments) != 1 {
				t.Fatal("verified account activation request failed")
			}
			e := s.enrollments[0]
			if !e.Authenticated || e.Status != "approved" || len(e.EnabledQueues) != 0 || e.EnabledWorkflowAccess != "" || len(e.Queues) != 1 || e.Queues[0] != "chickadee" {
				t.Fatal("signup claims applied queues or trusts client workflow mode")
			}
			r := httptest.NewRequest("GET", "/dashboard", nil)
			r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			body := w.Body.String()
			if strings.Contains(body, "awaiting beta approval") || !strings.Contains(body, "Your GitHub access is verified. Runner activation is being processed") {
				t.Fatal("automatic signup claims manual waiting")
			}
			if strings.Contains(body, `name="workflow_path"`) {
				t.Fatal("automatic org signup still requires main-only workflow path")
			}
		})
	}
}
