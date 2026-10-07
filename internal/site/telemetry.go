package site

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// FleetSample contains global counts only; no tenant or worker identities.
type FleetSample struct {
	At            time.Time `json:"at"`
	Ready         int       `json:"ready"`
	Booting       int       `json:"booting"`
	Reserved      int       `json:"reserved"`
	Running       int       `json:"running"`
	Uncertain     int       `json:"uncertain"`
	WorkersOnline int       `json:"workers_online"`
	WorkersTotal  int       `json:"workers_total"`
}

func (s FleetSample) Assigned() int { return s.Reserved + s.Running + s.Uncertain }
func (s FleetSample) Total() int    { return s.Ready + s.Booting + s.Reserved + s.Running + s.Uncertain }
func validSample(s FleetSample, now time.Time) bool {
	if s.At.IsZero() || s.At.Before(now.Add(-24*time.Hour)) || s.At.After(now.Add(time.Minute)) || s.WorkersOnline < 0 || s.WorkersTotal < 0 || s.WorkersTotal > 64 || s.WorkersOnline > s.WorkersTotal {
		return false
	}
	for _, n := range []int{s.Ready, s.Booting, s.Reserved, s.Running, s.Uncertain} {
		if n < 0 || n > 1024 {
			return false
		}
	}
	return s.Total() <= 1024
}
func (s *Server) loadTelemetry() error {
	path := filepath.Join(s.cfg.StateDir, "telemetry.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 512*1024 {
		return errors.New("invalid telemetry store")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var samples []FleetSample
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&samples) != nil || d.Decode(new(any)) != io.EOF || len(samples) > 1440 {
		return errors.New("invalid telemetry store")
	}
	now := time.Now()
	var last time.Time
	for _, sample := range samples {
		// Old samples may remain after downtime, but malformed counts are rejected.
		if !validSample(sample, sample.At) || sample.At.After(now.Add(time.Minute)) || !last.IsZero() && !sample.At.After(last) {
			return errors.New("invalid telemetry store")
		}
		last = sample.At
		if !sample.At.Before(now.Add(-24 * time.Hour)) {
			s.telemetry = append(s.telemetry, sample)
		}
	}
	return nil
}
func (s *Server) importTelemetry(w http.ResponseWriter, r *http.Request) {
	var sample FleetSample
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if d.Decode(&sample) != nil || d.Decode(new(any)) != io.EOF || !validSample(sample, time.Now()) {
		http.Error(w, "Invalid telemetry", 400)
		return
	}
	sample.At = sample.At.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := append([]FleetSample(nil), s.telemetry...)
	if len(entries) > 0 && !sample.At.After(entries[len(entries)-1].At) {
		http.Error(w, "Telemetry must advance", 409)
		return
	}
	if len(entries) > 0 && entries[len(entries)-1].At.Truncate(time.Minute).Equal(sample.At.Truncate(time.Minute)) {
		entries[len(entries)-1] = sample
	} else {
		entries = append(entries, sample)
	}
	for len(entries) > 0 && entries[0].At.Before(time.Now().Add(-24*time.Hour)) {
		entries = entries[1:]
	}
	if len(entries) > 1440 {
		entries = entries[len(entries)-1440:]
	}
	data, err := json.Marshal(entries)
	if err == nil {
		err = s.saveTelemetry(data)
	}
	if err != nil {
		http.Error(w, "Telemetry could not be saved", 500)
		return
	}
	s.telemetry = entries
	w.WriteHeader(204)
}
func (s *Server) saveTelemetry(data []byte) error {
	f, err := os.CreateTemp(s.cfg.StateDir, ".telemetry-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(s.cfg.StateDir, "telemetry.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type telemetryBar struct {
	FleetSample
	Height int
}

func (b telemetryBar) Y() int { return 200 - b.Height }
func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if s.cfg.AdminUserID <= 0 || v.User.ID != s.cfg.AdminUserID {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	samples := []FleetSample{}
	for _, sample := range s.telemetry {
		if !sample.At.Before(time.Now().Add(-24 * time.Hour)) {
			samples = append(samples, sample)
		}
	}
	s.mu.Unlock()
	data := struct {
		User    User
		CSRF    string
		Samples []telemetryBar
		Current *FleetSample
		Stale   bool
	}{User: v.User, CSRF: v.CSRF, Samples: nil}
	max := 1
	for _, sample := range samples {
		if sample.Assigned() > max {
			max = sample.Assigned()
		}
	}
	for _, sample := range samples {
		data.Samples = append(data.Samples, telemetryBar{sample, sample.Assigned() * 200 / max})
	}
	if len(samples) > 0 {
		data.Current = &samples[len(samples)-1]
		data.Stale = time.Since(data.Current.At) > 3*time.Minute
	}
	var b bytes.Buffer
	if s.templates.ExecuteTemplate(&b, "admin-dashboard.html", data) != nil {
		http.Error(w, "Page unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}
