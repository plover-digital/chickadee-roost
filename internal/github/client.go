package github

import (
	"context"
	"errors"
	"fmt"
	"github.com/actions/scaleset"
	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	API              *scaleset.Client
	SetID            int
	GroupID          int
	SetName          string
	NativeMac        bool
	newRetirementAPI func() (runnerRemovalAPI, error)
	newJITAPI        func() (jitAPI, error)
	statisticsAPI    statisticsAPI
}

// Actions cache archives paths relative to GITHUB_WORKSPACE. Match the hosted
// Linux workspace layout so caches outside the checkout (for example Bun in
// HOME) restore to the same location after switching between runner providers.
const runnerWorkFolder = "/home/runner/work"

func New(ctx context.Context, c dispatch.Queue) (*Client, error) { return newClient(ctx, c, true) }

// Existing never recreates a removed scale set during journal recovery.
func Existing(ctx context.Context, c dispatch.Queue) (*Client, error) {
	return newClient(ctx, c, false)
}

// ErrUnavailable marks an upstream scope failure; local configuration errors stay fatal.
var ErrUnavailable = errors.New("GitHub scope unavailable")

// Recover retains the recorded scale-set identity without an upstream lookup.
// Removal still verifies the runner belongs to this exact scale set.
func Recover(ctx context.Context, q dispatch.Queue, setID int) (*Client, error) {
	if setID <= 0 {
		return nil, fmt.Errorf("invalid recovery scale set")
	}
	c, err := initialize(q)
	if err != nil {
		return nil, err
	}
	c.SetID = setID
	return c, nil
}

func newClient(ctx context.Context, c dispatch.Queue, create bool) (*Client, error) {
	client, err := initialize(c)
	if err != nil {
		return nil, err
	}
	api := client.API
	set, e := api.GetRunnerScaleSet(ctx, c.RunnerGroupID, c.ScaleSet)
	if e != nil {
		return nil, fmt.Errorf("%w: scale set lookup failed", ErrUnavailable)
	}
	if set == nil && !create {
		return client, nil
	}
	if set == nil {
		set, e = api.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: c.ScaleSet, RunnerGroupID: c.RunnerGroupID, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
		if e != nil {
			return nil, fmt.Errorf("%w: scale set creation failed", ErrUnavailable)
		}
	}
	if !set.RunnerSetting.DisableUpdate {
		return nil, fmt.Errorf("existing scale set must disable runner auto-update")
	}
	client.SetID = set.ID
	return client, nil
}
func initialize(c dispatch.Queue) (*Client, error) {
	u, e := url.Parse(c.GitHubURL)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("prototype supports https://github.com org/repo scopes")
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) < 1 || len(p) > 2 || p[0] == "" || p[0] == "enterprises" {
		return nil, fmt.Errorf("use repository or organization scope")
	}
	info, e := os.Stat(c.KeyFile)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("App key must be a regular file with mode 0600")
	}
	key, e := os.ReadFile(c.KeyFile)
	if e != nil {
		return nil, e
	}
	// Do not replay non-idempotent JIT creation after an ambiguous HTTP failure.
	appConfig := scaleset.ClientWithGitHubAppConfig{GitHubConfigURL: c.GitHubURL, GitHubAppAuth: scaleset.GitHubAppAuth{ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: string(key)}, SystemInfo: scaleset.SystemInfo{System: "chickadee", Version: "prototype"}}
	api, e := scaleset.NewClientWithGitHubApp(appConfig, scaleset.WithTimeout(65*time.Second), scaleset.WithRetryMax(0))
	if e != nil {
		return nil, fmt.Errorf("App client initialization failed")
	}
	// Periodic reads have one caller per queue and a dedicated SDK mutex.
	statsAPI, e := scaleset.NewClientWithGitHubApp(appConfig, scaleset.WithTimeout(5*time.Second), scaleset.WithRetryMax(0))
	if e != nil {
		return nil, fmt.Errorf("statistics client initialization failed")
	}
	return &Client{API: api, statisticsAPI: statsAPI, GroupID: c.RunnerGroupID, SetName: c.ScaleSet, NativeMac: c.Machine == "apple-vz", newRetirementAPI: func() (runnerRemovalAPI, error) {
		// The pinned SDK takes a context-unaware mutex around API requests. Use
		// a fresh client per retirement so session/statistics/JIT calls cannot
		// hold its mutex beyond the retirement request's deadline.
		return scaleset.NewClientWithGitHubApp(appConfig, scaleset.WithTimeout(5*time.Second), scaleset.WithRetryMax(0))
	}, newJITAPI: func() (jitAPI, error) {
		return scaleset.NewClientWithGitHubApp(appConfig, scaleset.WithTimeout(5*time.Second), scaleset.WithRetryMax(0))
	}}, nil
}

type jitAPI interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

func (c *Client) JIT(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.newJITAPI == nil {
		return "", fmt.Errorf("JIT client unavailable")
	}
	// Fresh clients keep authentication and JIT outside the listener SDK mutex.
	// This non-idempotent call is never retried, including an ambiguous timeout.
	api, err := c.newJITAPI()
	if err != nil {
		return "", fmt.Errorf("JIT client unavailable")
	}
	workFolder := runnerWorkFolder
	if c.NativeMac {
		workFolder = "/Users/runner/work"
	}
	j, e := api.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: workFolder}, c.SetID)
	if e != nil || j == nil || j.EncodedJITConfig == "" {
		return "", fmt.Errorf("JIT generation failed")
	}
	return j.EncodedJITConfig, nil
}

// RetireIdle asks GitHub to deregister an owned runner. The service atomically
// refuses removal while a job is assigned/running, including a job-start race.
// A successful response is not worker process-exit or disk-cleanup proof.
func (c *Client) RetireIdle(ctx context.Context, name string) error {
	if c.newRetirementAPI == nil {
		return fmt.Errorf("retirement client unavailable")
	}
	api, err := c.newRetirementAPI()
	if err != nil {
		return fmt.Errorf("retirement client unavailable")
	}
	return removeOwnedRunner(ctx, api, c.SetID, name)
}

func (c *Client) Remove(ctx context.Context, name string) error {
	return c.RetireIdle(ctx, name)
}

type runnerRemovalAPI interface {
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

func removeOwnedRunner(ctx context.Context, api runnerRemovalAPI, setID int, name string) error {
	r, e := api.GetRunnerByName(ctx, name)
	if e != nil {
		return fmt.Errorf("runner lookup failed")
	}
	if r == nil {
		return nil
	}
	if setID <= 0 || r.RunnerScaleSetID != setID || r.Name != name || r.ID <= 0 {
		return fmt.Errorf("runner ownership mismatch")
	}
	// Use the ordinary service DELETE, never a force-delete endpoint or a
	// sampled busy=false decision. Busy refusals and ambiguous responses retain
	// the execution and its quota; the broker can retry without killing the VM.
	if e = api.RemoveRunner(ctx, int64(r.ID)); e != nil {
		return fmt.Errorf("runner removal failed")
	}
	return nil
}

// Poll acquires available requests, then scales using statistics, never message counts.
// No job ID is sent to a VM: GitHub selects any matching idle runner.
func (c *Client) Poll(ctx context.Context, owner string, max int, desired chan<- int) error {
	session, e := c.API.MessageSessionClient(ctx, c.SetID, owner)
	if e != nil {
		return fmt.Errorf("message session creation failed")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	n := 0
	if s := session.Session().Statistics; s != nil {
		n = s.TotalAssignedJobs
	}
	select {
	case desired <- min(max, maxInt(0, n)):
	case <-ctx.Done():
		return ctx.Err()
	}
	return pollMessages(ctx, session, max, desired)
}

// A separate interface keeps ordering and cancellation testable without GitHub.
type messageSession interface {
	GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error)
	AcquireJobs(context.Context, []int64) ([]int64, error)
	DeleteMessage(context.Context, int) error
}

// ObservedAt orders local observations, including reads that finish after newer
// listener messages. A statistics read uses its request start time.
type DemandStatistics struct {
	ObservedAt       time.Time
	Assigned         int
	CompletedRunners []string
}

type statisticsAPI interface {
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
}

func (c *Client) Statistics(ctx context.Context) (DemandStatistics, error) {
	observedAt := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.statisticsAPI == nil {
		return DemandStatistics{}, fmt.Errorf("statistics client unavailable")
	}
	set, err := c.statisticsAPI.GetRunnerScaleSet(ctx, c.GroupID, c.SetName)
	if err != nil || set == nil || set.ID != c.SetID || set.Statistics == nil {
		return DemandStatistics{}, fmt.Errorf("scale-set statistics unavailable")
	}
	return DemandStatistics{Assigned: maxInt(0, set.Statistics.TotalAssignedJobs), ObservedAt: observedAt}, nil
}
func (c *Client) PollStatistics(ctx context.Context, owner string, max int, desired chan<- DemandStatistics) error {
	observedAt := time.Now()
	session, err := c.API.MessageSessionClient(ctx, c.SetID, owner)
	if err != nil {
		return fmt.Errorf("message session creation failed")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	emit := func(s DemandStatistics) error {
		if s.ObservedAt.IsZero() {
			s.ObservedAt = time.Now()
		}
		select {
		case desired <- s:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	n := 0
	if stats := session.Session().Statistics; stats != nil {
		n = stats.TotalAssignedJobs
	}
	if err = emit(DemandStatistics{Assigned: min(max, maxInt(0, n)), ObservedAt: observedAt}); err != nil {
		return err
	}
	return pollMessagesStats(ctx, session, max, emit)
}
func pollMessages(ctx context.Context, session messageSession, max int, desired chan<- int) error {
	return pollMessagesStats(ctx, session, max, func(s DemandStatistics) error {
		select {
		case desired <- s.Assigned:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// maxAvailableJobs bounds an upstream batch independently of local VM capacity.
// Oversized batches are rejected without acknowledgement, never truncated.
const maxAvailableJobs = 10000

func pollMessagesStats(ctx context.Context, session messageSession, max int, emit func(DemandStatistics) error) error {
	last := 0
	for {
		m, e := session.GetMessage(ctx, last, max)
		if e != nil {
			return fmt.Errorf("demand poll failed")
		}
		if m == nil {
			continue
		}
		if len(m.JobAvailableMessages) > maxAvailableJobs {
			return fmt.Errorf("availability batch too large")
		}
		// Acquire the entire offered backlog before acknowledging this message.
		// Local capacity bounds runners, not ownership of queued requests.
		var ids []int64
		for _, j := range m.JobAvailableMessages {
			if j == nil || j.RunnerRequestID <= 0 {
				return fmt.Errorf("invalid available job record")
			}
			ids = append(ids, j.RunnerRequestID)
		}
		if m.Statistics != nil {
			stats := DemandStatistics{Assigned: min(max, maxInt(0, m.Statistics.TotalAssignedJobs)), ObservedAt: time.Now()}
			for _, j := range m.JobCompletedMessages {
				if j != nil && j.RunnerName != "" && len(j.RunnerName) <= 100 && len(stats.CompletedRunners) < 50 {
					stats.CompletedRunners = append(stats.CompletedRunners, j.RunnerName)
				}
			}
			if err := emit(stats); err != nil {
				return err
			}
		}
		// Statistics already describe owned demand. Let warm provisioning overlap
		// acquisition; never infer desired runners from available-message counts.
		if len(ids) > 0 {
			started := time.Now()
			if _, e = session.AcquireJobs(ctx, ids); e != nil {
				return fmt.Errorf("job acquisition failed")
			}
			slog.Info("Scale-set acquisition completed", "duration_ms", time.Since(started).Milliseconds())
		}
		if e = session.DeleteMessage(ctx, m.MessageID); e != nil {
			return fmt.Errorf("message acknowledgement failed")
		}
		last = m.MessageID
	}
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
