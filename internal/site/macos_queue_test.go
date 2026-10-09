package site

import (
	"bytes"
	"strings"
	"testing"
)

func TestMacOSQueueCannotBeRequestedBeforeWorkerReadiness(t *testing.T) {
	if _, err := requestedQueues([]string{"chickadee-small-macos-26"}); err == nil {
		t.Fatal("unimplemented Mac queue accepted")
	}
	s := fixture(t)
	p := page{Title: "Your repositories", User: &User{ID: 7}, Choices: []Choice{{Installation: Installation{ID: 8, Account: Account{Type: "User"}}, Repository: Repository{ID: 99, Name: "example/repo"}}}, ExtraQueues: extraQueues}
	var out bytes.Buffer
	if err := s.templates.ExecuteTemplate(&out, "page.html", p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `disabled aria-label="macOS ARM64 queue is not yet available"`) || !strings.Contains(out.String(), "preparing; not available yet") {
		t.Fatal("Mac queue looks runnable before admission")
	}
	if strings.Contains(out.String(), `name="queue" value="chickadee-small-macos-26"`) {
		t.Fatal("placeholder submits unsupported queue")
	}
}
