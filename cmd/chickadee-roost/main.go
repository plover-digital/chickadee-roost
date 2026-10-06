// chickadee-roost owns GitHub listeners and fleet placement. Workers own VMs.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
	"github.com/plover-digital/chickadee-roost/internal/github"
	"github.com/plover-digital/chickadee-roost/internal/usage"
	"github.com/plover-digital/chickadee/workerapi"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type archivedBackend struct {
	Queue dispatch.Queue
	SetID int
}
type poller struct {
	queue  dispatch.Queue
	client *github.Client
	cancel context.CancelFunc
	done   chan struct{}
}
type update struct {
	key        string
	n          int
	generation uint64
	completed  []string
}

func main() {
	catalog := flag.String("catalog", "/etc/chickadee/config.json", "operator queue catalog")
	path := flag.String("fleet", "/etc/chickadee/fleet.json", "private authenticated fleet configuration")
	check := flag.Bool("check", false, "validate configuration without GitHub or worker operations")
	flag.Parse()
	c, e := dispatch.LoadFleet(*path)
	if e == nil {
		_, _, e = dispatch.LoadCatalog(*catalog, c.ImageDigests)
	}
	if e != nil {
		slog.Error("Invalid fleet configuration", "reason", e.Error())
		os.Exit(1)
	}
	if *check {
		return
	}
	if os.Geteuid() == 0 {
		slog.Error("Run Roost as the unprivileged service account")
		os.Exit(1)
	}
	if e = run(*catalog, c); e != nil && !errors.Is(e, context.Canceled) {
		slog.Error("Fleet broker stopped", "reason", e.Error())
		os.Exit(1)
	}
}
func run(path string, c dispatch.FleetConfig) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	broker, e := fleet.Open(c.StateDir, c.BrokerID, c.Limits())
	if e != nil {
		return e
	}
	defer broker.Close()
	archives := map[string]archivedBackend{}
	if data, err := os.ReadFile(filepath.Join(c.StateDir, "backend-identities.json")); err == nil {
		if len(data) > 2<<20 || json.Unmarshal(data, &archives) != nil {
			return fmt.Errorf("invalid backend identity journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	workers := map[string]fleet.Worker{}
	for _, w := range c.Workers {
		client, e := workerapi.NewClient(w)
		if e != nil {
			return e
		}
		workers[w.Identity.WorkerID] = dispatch.RemoteWorker{Client: client}
	}
	values := make(chan update, 256)
	polls := map[string]poller{}
	backends := map[string]fleet.Backend{}
	demands := map[string]fleet.Demand{}
	for _, a := range broker.Assignments() {
		if a.Phase == fleet.Complete && !(a.CredentialUncertain && time.Since(a.CompletedAt) < 10*time.Minute) {
			continue
		}
		if backends[a.QueueID] != nil {
			continue
		}
		saved, ok := archives[a.QueueID]
		if !ok || saved.Queue.Key != a.QueueID || saved.Queue.Scope != a.ScopeURL || saved.Queue.Label != a.Label || saved.SetID <= 0 {
			return fmt.Errorf("missing or inconsistent recovery backend identity")
		}
		ic, stop := context.WithTimeout(ctx, 65*time.Second)
		client, err := github.Existing(ic, saved.Queue)
		stop()
		if err != nil {
			return err
		}
		if client.SetID == 0 {
			client.SetID = saved.SetID
		}
		backends[a.QueueID] = client
	}
	gens := map[string]uint64{}
	revisions := map[string]uint64{}
	for _, a := range broker.Assignments() {
		if a.LastDemandRevision > revisions[a.QueueID] {
			revisions[a.QueueID] = a.LastDemandRevision
		}
	}
	var generation uint64
	stopAll := func() {
		for _, p := range polls {
			p.cancel()
		}
		for _, p := range polls {
			<-p.done
		}
	}
	defer stopAll()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(signals)
	queues := []dispatch.Queue{}
	draining := false
	start := func(q dispatch.Queue, client *github.Client) {
		generation++
		g := generation
		gens[q.Key] = g
		pc, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		polls[q.Key] = poller{q, client, stop, done}
		backends[q.Key] = client
		go func() {
			refreshDone := make(chan struct{})
			go func() {
				defer close(refreshDone)
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-pc.Done():
						return
					case <-ticker.C:
						sc, stop := context.WithTimeout(pc, 5*time.Second)
						stats, err := client.Statistics(sc)
						stop()
						if err == nil {
							select {
							case values <- update{key: q.Key, n: stats.Assigned, generation: g}:
							case <-pc.Done():
								return
							}
						}
					}
				}
			}()
			defer func() { stop(); <-refreshDone; close(done) }()
			out := make(chan github.DemandStatistics, 16)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				delay := 2 * time.Second
				for pc.Err() == nil {
					_ = client.PollStatistics(pc, q.Label, q.Max, out)
					if pc.Err() != nil {
						return
					}
					slog.Warn("Queue listener retry; existing worker jobs preserved")
					select {
					case out <- github.DemandStatistics{}:
					case <-pc.Done():
						return
					}
					t := time.NewTimer(delay)
					select {
					case <-t.C:
					case <-pc.Done():
						t.Stop()
						return
					}
					delay = min(delay*2, 30*time.Second)
				}
			}()
			for {
				select {
				case n := <-out:
					select {
					case values <- update{key: q.Key, n: n.Assigned, generation: g, completed: n.CompletedRunners}:
					case <-pc.Done():
						<-finished
						return
					}
				case <-finished:
					return
				case <-pc.Done():
					<-finished
					return
				}
			}
		}()
	}
	reload := func() error {
		next, hash, e := dispatch.LoadCatalog(path, c.ImageDigests)
		if e != nil {
			return e
		}
		prepared := map[string]*github.Client{}
		wanted := map[string]dispatch.Queue{}
		for _, q := range next {
			wanted[q.Key] = q
			if old, ok := polls[q.Key]; ok {
				a, b := old.queue, q
				a.Max = b.Max
				a.ScopeMax = b.ScopeMax
				if a != b {
					return fmt.Errorf("existing queue identity or image change requires broker restart")
				}
			} else {
				ic, stop := context.WithTimeout(ctx, 65*time.Second)
				client, e := github.New(ic, q)
				stop()
				if e != nil {
					return e
				}
				prepared[q.Key] = client
			}
		}
		for key, old := range polls {
			q, exists := wanted[key]
			if !exists || q.Max != old.queue.Max {
				old.cancel()
				<-old.done
				delete(polls, key)
				delete(demands, key)
				if exists {
					start(q, old.client)
				} // previous session is closed before replacement
			}
		}
		for _, q := range next {
			if _, exists := polls[q.Key]; !exists {
				start(q, prepared[q.Key])
			}
		}
		for _, q := range next {
			if p, ok := polls[q.Key]; ok {
				archives[q.Key] = archivedBackend{Queue: q, SetID: p.client.SetID}
			}
		}
		if err := writeJSON(c.StateDir, "backend-identities.json", archives); err != nil {
			return err
		}
		queues = next
		return acknowledge(c.StatusDir, hash, "applied")
	}
	if e = reload(); e != nil {
		return e
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sig := <-signals:
			if sig == syscall.SIGUSR1 {
				draining = true
				stopAll()
				polls = map[string]poller{}
				demands = map[string]fleet.Demand{}
				slog.Info("Broker draining; worker jobs continue")
				continue
			}
			if draining {
				continue
			}
			if err := reload(); err != nil {
				raw, _ := os.ReadFile(path)
				sum := sha256.Sum256(raw)
				hash := hex.EncodeToString(sum[:])
				_ = acknowledge(c.StatusDir, hash, "rejected")
				slog.Warn("Queue reload rejected; existing assignments preserved")
			}
		case v := <-values:
			p, exists := polls[v.key]
			if !exists || gens[v.key] != v.generation {
				continue
			}
			q := p.queue
			d := demands[v.key]
			revisions[v.key]++
			d.Revision = revisions[v.key]
			d.QueueID = q.Key
			d.ScopeURL = q.Scope
			d.Label = q.Label
			d.ScopeMax = q.ScopeMax
			d.Assigned = max(0, min(v.n, q.Max))
			d.CompletedRunners = append(d.CompletedRunners, v.completed...)
			if len(d.CompletedRunners) > 256 {
				d.CompletedRunners = d.CompletedRunners[len(d.CompletedRunners)-256:]
			}
			d.Profile = fleet.Profile{Digest: q.ImageDigest, Machine: q.Machine, CPUs: q.CPUs, MemoryMiB: q.MemoryMiB, DiskGiB: q.DiskGiB}
			demands[v.key] = d
		case <-tick.C:
			ds := []fleet.Demand{}
			for _, q := range queues {
				if d, ok := demands[q.Key]; ok {
					ds = append(ds, d)
				}
			}
			if e = broker.Sync(ctx, ds, workers, backends); e != nil {
				slog.Warn("Fleet reconciliation deferred; uncertain capacity retained")
			}
			for key, d := range demands {
				d.CompletedRunners = nil
				demands[key] = d
			}
			assignments := broker.Assignments()
			for _, a := range assignments {
				if a.Phase == fleet.Complete {
					if e = usage.Append(c.StatusDir, usage.Record{ID: a.ID, Scope: a.ScopeURL, Label: a.Label, Reserved: a.ReservedAt, Completed: a.CompletedAt}); e != nil {
						return fmt.Errorf("usage persistence failed")
					}
				}
			}
			if e = writeStatus(c.StatusDir, queues, demands, assignments, draining); e != nil {
				return e
			}
			if draining {
				busy := false
				for _, a := range assignments {
					if a.Phase != fleet.Complete {
						busy = true
					}
				}
				if !busy {
					return nil
				}
			}
		}
	}
}
func writeJSON(dir, name string, value any) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	data, e := json.Marshal(value)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".fleet-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(dir, name)); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func writeStatus(dir string, queues []dispatch.Queue, demands map[string]fleet.Demand, assignments []fleet.Assignment, draining bool) error {
	qs := []map[string]any{}
	for _, q := range queues {
		allocated, spent := 0, 0
		for _, a := range assignments {
			if a.QueueID == q.Key && a.Phase != fleet.Complete {
				allocated++
				if a.Phase == fleet.JITIntent || a.Phase == fleet.Delivered || a.Phase == fleet.Uncertain {
					spent++
				}
			}
		}
		qs = append(qs, map[string]any{"github_url": q.GitHubURL, "label": q.Label, "assigned_demand": demands[q.Key].Assigned, "allocated_vms": allocated, "ready_vms": 0, "credentialed_vms": spent})
	}
	return writeJSON(dir, "status.json", map[string]any{"updated_at": time.Now().UTC(), "draining": draining, "queues": qs})
}

func acknowledge(dir, hash, status string) error {
	return writeJSON(dir, "reload.json", map[string]any{"config_sha256": hash, "status": status, "updated_at": time.Now().UTC()})
}
