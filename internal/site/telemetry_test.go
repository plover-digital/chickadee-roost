package site

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFleetTelemetryPrivatePersistenceAndAdminIdentity(t *testing.T) {
	c := Config{PublicURL: "http://127.0.0.1:8080", AppSlug: "chickadee-run", StateDir: t.TempDir(), AdminUserID: 38401861}
	os.Chmod(c.StateDir, 0700)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	sample := FleetSample{At: time.Now().UTC().Add(-time.Second), Ready: 2, Running: 1, WorkersOnline: 2, WorkersTotal: 2}
	body, _ := json.Marshal(sample)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/telemetry", bytes.NewReader(body)))
	if w.Code != 404 {
		t.Fatal("public telemetry route exposed", w.Code)
	}
	w = httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/telemetry", bytes.NewReader(body)))
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	restored, err := New(c)
	if err != nil || len(restored.telemetry) != 1 {
		t.Fatal("persistence", err)
	}
	for _, tc := range []struct {
		id    int64
		login string
		want  int
	}{{38401861, "renamed", 200}, {7, "wokuno", 404}} {
		restored.sessions["fixture"] = session{User: User{tc.id, tc.login}, CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
		r := httptest.NewRequest("GET", "/dashboard/admin", nil)
		r.AddCookie(&http.Cookie{Name: restored.cookieName("chickadee-session"), Value: "fixture"})
		w = httptest.NewRecorder()
		restored.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("id %d status %d body %s", tc.id, w.Code, w.Body.String())
		}
		if tc.want == 200 && !strings.Contains(w.Body.String(), "Running") {
			t.Fatal("missing counts")
		}
	}
	sample.Ready = -1
	body, _ = json.Marshal(sample)
	w = httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/telemetry", bytes.NewReader(body)))
	if w.Code != 400 {
		t.Fatal("negative accepted")
	}
	sample.Ready = 1
	sample.At = time.Now().Add(-25 * time.Hour)
	body, _ = json.Marshal(sample)
	w = httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/telemetry", bytes.NewReader(body)))
	if w.Code != 400 {
		t.Fatal("expired accepted")
	}
}

func TestTelemetryFullRetentionRoundtrip(t *testing.T) {
	c := Config{PublicURL: "http://127.0.0.1:8080", AppSlug: "chickadee-run", StateDir: t.TempDir()}
	os.Chmod(c.StateDir, 0700)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]FleetSample, 1440)
	now := time.Now().UTC()
	for i := range samples {
		samples[i] = FleetSample{At: now.Add(time.Duration(i-1439) * time.Minute), Ready: 100, Running: 100, WorkersOnline: 64, WorkersTotal: 64}
	}
	data, _ := json.Marshal(samples)
	if err = s.saveTelemetry(data); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(c.StateDir, "telemetry.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private store", err)
	}
	restored, err := New(c)
	if err != nil || len(restored.telemetry) != 1440 {
		t.Fatal("full history restart", err)
	}
}
