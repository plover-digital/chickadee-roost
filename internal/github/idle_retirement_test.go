package github

import (
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/go-retryablehttp"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type removalAPI struct {
	runner                   *scaleset.RunnerReference
	lookupErr, errorOnDelete error
	afterLookup              func()
	removed                  []int64
}

func (a *removalAPI) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	if a.afterLookup != nil {
		a.afterLookup()
	}
	return a.runner, a.lookupErr
}
func (a *removalAPI) RemoveRunner(_ context.Context, id int64) error {
	a.removed = append(a.removed, id)
	return a.errorOnDelete
}
func TestIdleRetirementUsesServerGuardAcrossJobStartRace(t *testing.T) {
	api := &removalAPI{runner: &scaleset.RunnerReference{ID: 42, Name: "owned", RunnerScaleSetID: 7}}
	// GitHub assigns a job after lookup but before deletion. There is no local
	// busy flag to trust: the DELETE refusal is the authority, and must propagate.
	api.afterLookup = func() { api.errorOnDelete = errors.New("JobStillRunningError private-upstream-fixture") }
	err := removeOwnedRunner(context.Background(), api, 7, "owned")
	if err == nil || len(api.removed) != 1 {
		t.Fatal("job-start race was treated as idle retirement")
	}
	if strings.Contains(err.Error(), "private-upstream-fixture") {
		t.Fatal("raw upstream error leaked")
	}
	api.afterLookup = nil
	api.errorOnDelete = nil
	if err := removeOwnedRunner(context.Background(), api, 7, "owned"); err != nil {
		t.Fatal(err)
	}
}
func TestIdleRetirementFencesIdentityAndRetainsUnknownResults(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-set", "wrong-name", "invalid-id", "invalid-set", "lookup-failed", "delete-failed", "valid"} {
		t.Run(kind, func(t *testing.T) {
			api := &removalAPI{runner: &scaleset.RunnerReference{ID: 42, Name: "owned", RunnerScaleSetID: 7}}
			setID := 7
			switch kind {
			case "missing":
				api.runner = nil
			case "wrong-set":
				api.runner.RunnerScaleSetID = 8
			case "wrong-name":
				api.runner.Name = "foreign"
			case "invalid-id":
				api.runner.ID = 0
			case "invalid-set":
				setID = 0
			case "lookup-failed":
				api.lookupErr = errors.New("private-upstream-fixture")
			case "delete-failed":
				api.errorOnDelete = errors.New("private-upstream-fixture")
			}
			err := removeOwnedRunner(context.Background(), api, setID, "owned")
			wantSuccess := kind == "missing" || kind == "valid"
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected retirement result %v", err)
			}
			wantDelete := kind == "valid" || kind == "delete-failed"
			if (len(api.removed) == 1) != wantDelete {
				t.Fatal("unsafe identity reached deletion")
			}
			if err != nil && strings.Contains(err.Error(), "private-upstream-fixture") {
				t.Fatal("raw upstream error leaked")
			}
		})
	}
}

func offlineRemovalClient(t *testing.T, dial func(context.Context, string, string) (net.Conn, error)) *scaleset.Client {
	t.Helper()
	httpClient := retryablehttp.NewClient()
	httpClient.RetryMax = 0
	httpClient.Logger = nil
	httpClient.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: dial}}
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: "https://github.com/example/repository", PersonalAccessToken: "synthetic-token"}, scaleset.WithRetryableHTTPClint(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func TestRemovalDeadlineIsIndependentOfListenerSDKMutex(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			mainDone := make(chan struct{})
			mainAPI := offlineRemovalClient(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
				close(entered)
				<-release
				return nil, errors.New("offline fixture")
			})
			go func() { defer close(mainDone); _, _ = mainAPI.GetRunnerScaleSet(context.Background(), 1, "queue") }()
			defer func() { close(release); <-mainDone }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("main API did not acquire SDK mutex")
			}
			c := &Client{API: mainAPI, SetID: 7, newRetirementAPI: func() (runnerRemovalAPI, error) {
				return offlineRemovalClient(t, func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }), nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			retired := make(chan error, 1)
			go func() {
				if terminal {
					retired <- c.Remove(ctx, "owned")
				} else {
					retired <- c.RetireIdle(ctx, "owned")
				}
			}()
			select {
			case err := <-retired:
				if err == nil {
					t.Fatal("canceled retirement reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("retirement waited on the listener SDK mutex past its deadline")
			}

		})
	}
}
