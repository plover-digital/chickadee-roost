package site

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDisconnectedMissingInstallationHidesCardWithoutMutatingCleanupIntent(t *testing.T) {
	for _, tc := range []struct {
		name, status, desired string
		failed, hidden        bool
	}{{"disconnected removed", "disconnected", "disconnected", false, true}, {"disconnect pending removed", "active", "disconnected", false, true}, {"active access lost", "active", "active", false, false}, {"transient disconnect lookup", "disconnected", "disconnected", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t)
			s.sessions["fixture"] = session{User: User{ID: 7, Login: "tester"}, CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
			s.enrollments = []Enrollment{{ID: "cleanup-record", User: User{ID: 7}, InstallationID: 8, Repository: Repository{ID: 99, Name: "tester/removed"}, Status: tc.status, DesiredState: tc.desired, Queues: []string{"chickadee"}}}
			s.http.Transport = transport(func(*http.Request) (*http.Response, error) {
				if tc.failed {
					return nil, errors.New("temporary unavailable")
				}
				return response(`{"total_count":0,"installations":[]}`), nil
			})
			r := httptest.NewRequest("GET", "/dashboard", nil)
			r.AddCookie(&http.Cookie{Name: s.cookieName("chickadee-session"), Value: "fixture"})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			body := w.Body.String()
			card := strings.Contains(body, `aria-label="Runners for tester/removed"`)
			if card == tc.hidden {
				t.Fatal("unexpected card visibility", body)
			}
			if !tc.hidden && !tc.failed && !strings.Contains(body, "permission-required") {
				t.Fatal("active recovery guidance missing")
			}
			if tc.failed && (strings.Contains(body, "Requested queues") || strings.Contains(body, "GitHub permission approval is needed")) {
				t.Fatal("transient failure revived disconnect intent")
			}
			if len(s.enrollments) != 1 || s.enrollments[0].Status != tc.status || s.enrollments[0].DesiredState != tc.desired {
				t.Fatal("cleanup state mutated")
			}
		})
	}
}
