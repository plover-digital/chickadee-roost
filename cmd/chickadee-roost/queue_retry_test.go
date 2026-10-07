package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/plover-digital/chickadee-roost/internal/dispatch"
	"github.com/plover-digital/chickadee-roost/internal/fleet"
	"github.com/plover-digital/chickadee-roost/internal/github"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnavailableScopeDoesNotBlockHealthyQueueAndRetryIsBounded(t *testing.T) {
	for _, code := range []string{"403", "404"} {
		t.Run(code, func(t *testing.T) {
			retries := map[string]queueRetry{}
			now := time.Now()
			calls := 0
			open := func(ctx context.Context, q dispatch.Queue) (*github.Client, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("unbounded initialization")
				}
				if q.Key == "revoked" {
					return nil, errors.Join(github.ErrUnavailable, errors.New(code))
				}
				return &github.Client{SetID: 7}, nil
			}
			revoked := dispatch.Queue{Key: "revoked"}
			healthy := dispatch.Queue{Key: "healthy"}
			if c, e := prepareQueue(context.Background(), revoked, retries, now, open); c != nil || e != nil {
				t.Fatal("scope failure aborted preparation")
			}
			if c, e := prepareQueue(context.Background(), healthy, retries, now, open); c == nil || e != nil {
				t.Fatal("healthy scope blocked")
			}
			for i := 0; i < 10; i++ {
				prepareQueue(context.Background(), revoked, retries, now, open)
			}
			if calls != 2 {
				t.Fatal("unbounded retry")
			}
			for i := 0; i < 20; i++ {
				now = retries[revoked.Key].Next
				prepareQueue(context.Background(), revoked, retries, now, open)
			}
			if retries[revoked.Key].Delay != 5*time.Minute {
				t.Fatal("retry ceiling")
			}
			recovered := func(context.Context, dispatch.Queue) (*github.Client, error) { return &github.Client{SetID: 9}, nil }
			now = retries[revoked.Key].Next
			if c, e := prepareQueue(context.Background(), revoked, retries, now, recovered); c == nil || e != nil || len(retries) != 0 {
				t.Fatal("scope recovery failed")
			}
		})
	}
}
func TestLocalQueueConfigurationFailureRemainsFatal(t *testing.T) {
	want := errors.New("invalid key permissions")
	_, err := prepareQueue(context.Background(), dispatch.Queue{Key: "bad"}, map[string]queueRetry{}, time.Now(), func(context.Context, dispatch.Queue) (*github.Client, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Fatal("local failure hidden")
	}
}

func TestStatusDistinguishesInitializedFromUnavailableQueues(t *testing.T) {
	dir := t.TempDir()
	queues := []dispatch.Queue{{Key: "up", Label: "chickadee", GitHubURL: "https://github.com/example/up"}, {Key: "down", Label: "chickadee", GitHubURL: "https://github.com/example/down"}}
	if err := writeStatus(dir, queues, nil, nil, false, fleet.Telemetry{}, map[string]bool{"up": true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Queues []struct {
			Initialized bool `json:"queue_initialized"`
		} `json:"queues"`
	}
	if err = json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Queues) != 2 || !status.Queues[0].Initialized || status.Queues[1].Initialized {
		t.Fatal("unavailable queue advertised initialized")
	}
}
