package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type journal struct {
	Version     int
	BrokerID    string
	Assignments []Assignment
}

func Open(dir, brokerID string, limits Limits) (*Broker, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" || brokerID == "" || limits.MaxVMs < 1 || limits.MaxCPUs < 1 || limits.MaxMemoryMiB < 512 {
		return nil, errors.New("invalid broker configuration")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("broker state must be a private directory")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "broker.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("broker state already owned")
	}
	b := &Broker{dir: dir, id: brokerID, limits: limits, lock: lock}
	file, err := os.OpenFile(filepath.Join(dir, "assignments.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	var data []byte
	if err == nil {
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			file.Close()
			b.Close()
			return nil, errors.New("journal must be a private regular file")
		}
		data, err = io.ReadAll(io.LimitReader(file, (8<<20)+1))
		file.Close()
	}
	if err == nil {
		var state journal
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if len(data) > 8<<20 || decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF || state.Version != 1 || state.BrokerID != brokerID || len(state.Assignments) > 10000 {
			b.Close()
			return nil, errors.New("invalid broker journal")
		}
		seen := map[string]bool{}
		for _, a := range state.Assignments {
			if !validAssignment(a) || a.Worker.BrokerID != brokerID || seen[a.ID] {
				b.Close()
				return nil, errors.New("invalid assignment identity")
			}
			seen[a.ID] = true
		}
		b.assignments = state.Assignments
	} else if !os.IsNotExist(err) {
		b.Close()
		return nil, err
	}
	return b, nil
}
func (b *Broker) Close() error {
	if b.lock == nil {
		return nil
	}
	err := b.lock.Close()
	b.lock = nil
	return err
}
func (b *Broker) save() error {
	state := journal{1, b.id, b.assignments}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("broker journal capacity reached")
	}
	file, err := os.CreateTemp(b.dir, ".assignments-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(b.dir, "assignments.json")); err != nil {
		return err
	}
	directory, err := os.Open(b.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func validAssignment(a Assignment) bool {
	if a.DemandRevision == 0 || a.LastDemandRevision < a.DemandRevision || (a.Phase == Complete || a.Phase == Terminal) && a.CompletedAt.IsZero() || a.ID == "" || a.RunnerName == "" || a.QueueID == "" || a.ScopeURL == "" || a.Label == "" || a.Worker.WorkerID == "" || a.Worker.BrokerID == "" || a.Worker.Generation == 0 || a.Profile.Digest == "" || a.Profile.CPUs < 1 || a.Profile.MemoryMiB < 512 || a.Profile.DiskGiB < 4 || a.ReservedAt.IsZero() || !a.CompletedAt.IsZero() && (a.CompletedAt.Before(a.ReservedAt) || a.CompletedAt.Sub(a.ReservedAt) > 25*time.Hour) {
		return false
	}
	switch a.Phase {
	case Intent, Reserved, SealIntent, JITIntent, Delivered, Uncertain, Terminal, Complete:
		return true
	}
	return false
}
