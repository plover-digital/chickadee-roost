package github

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestStatisticsDeadlineIsIndependentOfListenerSDKMutex(t *testing.T) {
	entered, release, mainDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mainAPI := offlineRemovalClient(t, func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release
		return nil, errors.New("offline fixture")
	})
	go func() { defer close(mainDone); _, _ = mainAPI.GetRunnerScaleSet(context.Background(), 1, "queue") }()
	defer func() { close(release); <-mainDone }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("listener did not acquire SDK mutex")
	}
	readAPI := offlineRemovalClient(t, func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() })
	c := &Client{API: mainAPI, statisticsAPI: readAPI, SetID: 7, GroupID: 1, SetName: "queue"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Statistics(ctx); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired statistics read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("statistics read waited on listener SDK mutex")
	}
}

type statisticsFixture struct {
	set      *scaleset.RunnerScaleSet
	deadline time.Time
}

func (s *statisticsFixture) GetRunnerScaleSet(ctx context.Context, _ int, _ string) (*scaleset.RunnerScaleSet, error) {
	s.deadline, _ = ctx.Deadline()
	return s.set, nil
}
func TestStatisticsRequiresAuthoritativeMatchingCounts(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-id", "missing-statistics", "zero"} {
		t.Run(kind, func(t *testing.T) {
			api := &statisticsFixture{}
			if kind != "missing" {
				api.set = &scaleset.RunnerScaleSet{ID: 7}
			}
			if kind == "wrong-id" {
				api.set.ID = 8
			}
			if kind == "zero" {
				api.set.Statistics = &scaleset.RunnerScaleSetStatistic{}
			}
			c := &Client{SetID: 7, statisticsAPI: api}
			start := time.Now()
			stats, err := c.Statistics(context.Background())
			if api.deadline.IsZero() || api.deadline.Sub(start) > 5*time.Second+time.Millisecond {
				t.Fatal("read has no bounded deadline")
			}
			if kind == "zero" {
				if err != nil || stats.Assigned != 0 || stats.ObservedAt.Before(start) {
					t.Fatal("authoritative zero not published")
				}
			} else if err == nil {
				t.Fatal("unavailable statistics became false zero demand")
			}
		})
	}
}
