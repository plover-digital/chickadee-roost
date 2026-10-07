package site

import (
	"bytes"
	"strings"
	"testing"
)

func TestInactiveRunnerCardsHideQueueIntentButPendingKeepsIt(t *testing.T) {
	s := fixture(t)
	for _, status := range []string{"disconnected", "paused", "pending"} {
		t.Run(status, func(t *testing.T) {
			e := Enrollment{ID: "request", User: User{ID: 7}, InstallationID: 8, Repository: Repository{ID: 99, Name: "tester/repo"}, Status: status, DesiredState: status, Queues: []string{"chickadee"}}
			p := page{Title: "Your repositories", User: &User{ID: 7}, Enrollments: []Enrollment{e}, Choices: []Choice{{Installation: Installation{ID: 8}, Repository: e.Repository}}}
			var out bytes.Buffer
			if err := s.templates.ExecuteTemplate(&out, "page.html", p); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			start := strings.Index(html, "<h2>Current runners</h2>")
			if start < 0 {
				t.Fatal("runner card absent")
			}
			end := strings.Index(html[start:], "</article>")
			if start < 0 || end < 0 {
				t.Fatal("runner card absent")
			}
			card := html[start : start+end]
			for _, label := range []string{"Requested queues", "Enabled queues", "None yet"} {
				if strings.Contains(card, label) != (status == "pending") {
					t.Fatalf("status %s label %s", status, label)
				}
			}
			if status == "disconnected" && !strings.Contains(card, ">Reconnect</button>") {
				t.Fatal("reconnect action missing")
			}
			if !strings.Contains(card, `class="status">`+status) {
				t.Fatal("applied status missing")
			}
		})
	}
}
