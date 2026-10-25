package github

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestHostedHomeCacheRestoresToRunnerHome(t *testing.T) {
	// @actions/cache resolvePaths computes this relative archive member on a
	// hosted runner, then tar extracts it relative to our GITHUB_WORKSPACE.
	hostedWorkspace := "/home/runner/work/project/project"
	bun := "/home/runner/.bun/bin/bun"
	member, err := filepath.Rel(hostedWorkspace, bun)
	if err != nil {
		t.Fatal(err)
	}
	guestWorkspace := filepath.Join(runnerWorkFolder, "project", "project")
	if got := filepath.Clean(filepath.Join(guestWorkspace, member)); got != bun {
		t.Fatalf("hosted HOME cache restores to %q instead of %q", got, bun)
	}
}

type delayedSession struct {
	message      *scaleset.RunnerScaleSetMessage
	fetched      bool
	acquiring    chan []int64
	release      chan struct{}
	acquireErr   error
	acknowledged chan struct{}
}

func (s *delayedSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if !s.fetched {
		s.fetched = true
		return s.message, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *delayedSession) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	s.acquiring <- ids
	select {
	case <-s.release:
		return ids, s.acquireErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *delayedSession) DeleteMessage(context.Context, int) error {
	if s.acknowledged != nil {
		s.acknowledged <- struct{}{}
	}
	return nil
}

func TestAssignedDemandDoesNotWaitForAcquisition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 42}}, {JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 43}}}}, acquiring: make(chan []int64, 1), release: make(chan struct{})}
	desired := make(chan int)
	done := make(chan error, 1)
	go func() { done <- pollMessages(ctx, s, 2, desired) }()
	select {
	case n := <-desired:
		if n != 1 {
			t.Fatalf("wanted statistics-derived demand 1, got %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("assigned demand blocked behind acquisition")
	}
	select {
	case ids := <-s.acquiring:
		if !reflect.DeepEqual(ids, []int64{42, 43}) {
			t.Fatalf("offered requests dropped: %v", ids)
		}
	case <-time.After(time.Second):
		t.Fatal("acquisition never started")
	}
	// The acquisition is still blocked, but the assigned-demand signal arrived.
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("poll failed to cancel")
	}
}

func TestAvailabilityDoesNotBecomeAssignedDemand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{}, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 42}}}}, acquiring: make(chan []int64, 1), release: make(chan struct{})}
	desired := make(chan int)
	done := make(chan error, 1)
	go func() { done <- pollMessages(ctx, s, 2, desired) }()
	select {
	case n := <-desired:
		if n != 0 {
			t.Fatalf("availability turned into credentials: demand %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no statistics signal")
	}
	<-s.acquiring
	close(s.release)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("poll failed to stop")
	}
}

func TestCompletionMetadataDoesNotBecomeDemand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "chickadee-old"}}}, acquiring: make(chan []int64, 1), release: make(chan struct{})}
	err := pollMessagesStats(ctx, s, 2, func(stats DemandStatistics) error {
		if stats.Assigned != 1 || !reflect.DeepEqual(stats.CompletedRunners, []string{"chickadee-old"}) {
			t.Fatalf("bad statistics/lifecycle separation: %#v", stats)
		}
		cancel()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("cancellation lost")
	}
}

func TestBurstAcquisitionPreservesBacklogAndCompletionMetadata(t *testing.T) {
	for _, assigned := range []int{0, 1, 5} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("assigned-%d/failure-%t", assigned, fail), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "completed-fixture"}}}, acquiring: make(chan []int64, 1), release: make(chan struct{}), acknowledged: make(chan struct{}, 1)}
				for _, id := range []int64{41, 42, 43} {
					s.message.JobAvailableMessages = append(s.message.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id}})
				}
				if fail {
					s.acquireErr = errors.New("fixture failure")
				}
				stats, done := make(chan DemandStatistics, 1), make(chan error, 1)
				go func() {
					done <- pollMessagesStats(ctx, s, 1, func(value DemandStatistics) error { stats <- value; return nil })
				}()
				select {
				case value := <-stats:
					if value.Assigned != min(1, assigned) || !reflect.DeepEqual(value.CompletedRunners, []string{"completed-fixture"}) {
						t.Fatal("demand cap or completion metadata changed", value)
					}
				case <-time.After(time.Second):
					t.Fatal("no statistics")
				}
				select {
				case ids := <-s.acquiring:
					if !reflect.DeepEqual(ids, []int64{41, 42, 43}) {
						t.Fatal("burst truncated", ids)
					}
				case <-time.After(time.Second):
					t.Fatal("no acquisition")
				}
				select {
				case <-s.acknowledged:
					t.Fatal("ack before acquisition completed")
				default:
				}
				close(s.release)
				if fail {
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("lost acquisition error")
						}
					case <-time.After(time.Second):
						t.Fatal("failure not returned")
					}
					select {
					case <-s.acknowledged:
						t.Fatal("failed acquisition acknowledged")
					default:
					}
				} else {
					select {
					case <-s.acknowledged:
					case <-time.After(time.Second):
						t.Fatal("message not acknowledged")
					}
					cancel()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("poll did not stop")
					}
				}
			})
		}
	}
}

func TestInvalidAvailableJobIsRejectedBeforeAcquisitionOrAcknowledgement(t *testing.T) {
	for _, invalid := range []*scaleset.JobAvailable{nil, {}, {JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: -1}}} {
		for _, assigned := range []int{0, 1, 5} {
			t.Run(fmt.Sprint(assigned), func(t *testing.T) {
				s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 41}}, invalid}}, acquiring: make(chan []int64, 1), release: make(chan struct{}), acknowledged: make(chan struct{}, 1)}
				err := pollMessagesStats(context.Background(), s, 1, func(DemandStatistics) error { t.Fatal("invalid message emitted demand"); return nil })
				if err == nil {
					t.Fatal("invalid availability accepted")
				}
				select {
				case <-s.acquiring:
					t.Fatal("invalid availability partially acquired")
				default:
				}
				select {
				case <-s.acknowledged:
					t.Fatal("invalid availability acknowledged")
				default:
				}
			})
		}
	}
}

func TestOversizedAvailabilityRejectedBeforeSideEffects(t *testing.T) {
	s := &delayedSession{message: &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobAvailableMessages: make([]*scaleset.JobAvailable, maxAvailableJobs+1)}, acquiring: make(chan []int64, 1), release: make(chan struct{}), acknowledged: make(chan struct{}, 1)}
	job := &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 1}}
	for i := range s.message.JobAvailableMessages {
		s.message.JobAvailableMessages[i] = job
	}
	close(s.release)
	err := pollMessagesStats(context.Background(), s, 1, func(DemandStatistics) error { t.Fatal("oversized batch emitted demand"); return nil })
	if err == nil || err.Error() != "availability batch too large" {
		t.Fatal("oversized batch accepted", err)
	}
	if len(s.acquiring) != 0 || len(s.acknowledged) != 0 {
		t.Fatal("oversized batch acquired or acknowledged")
	}
}

// A full local runner slot must not cause the next offered batch to disappear
// when the listener acknowledges its message and polls for subsequent work.
type backlogSession struct {
	messages []*scaleset.RunnerScaleSetMessage
	acquired []int64
	acked    []int
}

func (s *backlogSession) GetMessage(ctx context.Context, _ int, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if len(s.messages) == 0 {
		return nil, errors.New("fixture complete")
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}
func (s *backlogSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.acquired = append(s.acquired, ids...)
	return ids, nil
}
func (s *backlogSession) DeleteMessage(_ context.Context, id int) error {
	if id == 1 && !reflect.DeepEqual(s.acquired, []int64{41, 42, 43}) {
		return errors.New("acknowledged unacquired backlog")
	}
	s.acked = append(s.acked, id)
	return nil
}
func TestFullQueuePreservesOfferedBacklogAcrossMessages(t *testing.T) {
	s := &backlogSession{messages: []*scaleset.RunnerScaleSetMessage{
		{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 41}},
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 42}},
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 43}},
		}},
		{MessageID: 2, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "completed-fixture"}}},
	}}
	var updates []DemandStatistics
	_ = pollMessagesStats(context.Background(), s, 1, func(d DemandStatistics) error { updates = append(updates, d); return nil })
	if !reflect.DeepEqual(s.acquired, []int64{41, 42, 43}) || !reflect.DeepEqual(s.acked, []int{1, 2}) {
		t.Fatalf("backlog did not survive acknowledgement: acquired=%v acked=%v", s.acquired, s.acked)
	}
	if len(updates) != 2 || updates[0].Assigned != 1 || updates[1].Assigned != 1 || !reflect.DeepEqual(updates[1].CompletedRunners, []string{"completed-fixture"}) {
		t.Fatal("runner cap or completion metadata changed", updates)
	}
}
