package site

import (
	"fmt"
	"github.com/plover-digital/chickadee/workerapi"
	"sort"
	"time"
)

type ResourceRun struct {
	ID          string                    `json:"id"`
	Label       string                    `json:"label"`
	CompletedAt time.Time                 `json:"completed_at"`
	CPUs        int                       `json:"cpus"`
	MemoryMiB   int                       `json:"memory_mib"`
	Summary     workerapi.ResourceSummary `json:"summary"`
}

func validResourceRuns(runs []ResourceRun, now time.Time) bool {
	if len(runs) > 10 {
		return false
	}
	seen := map[string]bool{}
	for _, r := range runs {
		label := knownQueue(r.Label)
		for _, q := range extraQueues {
			if r.Label == q {
				label = true
			}
		}
		if !label || len(r.ID) < 1 || len(r.ID) > 64 || seen[r.ID] || r.CompletedAt.IsZero() || r.CompletedAt.Before(now.AddDate(0, 0, -7)) || r.CompletedAt.After(now.Add(time.Minute)) || r.CPUs < 1 || r.CPUs > 256 || r.MemoryMiB < 512 || r.MemoryMiB > 1<<20 || !r.Summary.Valid() {
			return false
		}
		seen[r.ID] = true
	}
	return true
}
func (r ResourceRun) CPUAverage() string {
	if r.Summary.DurationMillis == 0 {
		return "unavailable"
	}
	return fmt.Sprintf("%.1f%%", float64(r.Summary.CPUUsec)/float64(r.Summary.DurationMillis*1000)/float64(r.CPUs)*100)
}
func (r ResourceRun) MemoryPeak() string {
	return fmt.Sprintf("%.0f MiB", float64(r.Summary.PeakMemoryBytes)/(1<<20))
}
func (r ResourceRun) MemoryLimit() string {
	return fmt.Sprintf("%.0f MiB", float64(r.Summary.MemoryLimitBytes)/(1<<20))
}
func (r ResourceRun) ReadMiB() string {
	return fmt.Sprintf("%.1f", float64(r.Summary.ReadBytes)/(1<<20))
}
func (r ResourceRun) WriteMiB() string {
	return fmt.Sprintf("%.1f", float64(r.Summary.WriteBytes)/(1<<20))
}
func (r ResourceRun) ReadRate() string {
	return fmt.Sprintf("%.1f", float64(r.Summary.PeakReadBPS)/(1<<20))
}
func (r ResourceRun) WriteRate() string {
	return fmt.Sprintf("%.1f", float64(r.Summary.PeakWriteBPS)/(1<<20))
}
func (r ResourceRun) ThrottleSeconds() string {
	return fmt.Sprintf("%.3f", float64(r.Summary.CPUThrottledUsec)/1e6)
}
func (r ResourceRun) DurationSeconds() string {
	return fmt.Sprintf("%.1f", float64(r.Summary.DurationMillis)/1000)
}
func (p page) ResourceRuns() []ResourceRun {
	out := []ResourceRun{}
	seen := map[string]bool{}
	for _, a := range p.AccountUsage {
		if !a.authorized(p) {
			continue
		}
		for _, r := range a.Resources {
			if !seen[r.ID] {
				out = append(out, r)
				seen[r.ID] = true
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CompletedAt.After(out[j].CompletedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
