package github

import (
	"context"
	"errors"
	"strings"
	"testing"

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
