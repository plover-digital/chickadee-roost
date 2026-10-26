package github

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestJITDeadlineIsIndependentOfListenerSDKMutex(t *testing.T) {
	entered, release, mainDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
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
		t.Fatal("listener API did not acquire SDK mutex")
	}
	clients := 0
	c := &Client{API: mainAPI, SetID: 7, newJITAPI: func() (jitAPI, error) {
		clients++
		return offlineRemovalClient(t, func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.JIT(ctx, "owned"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired JIT reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("JIT waited on listener SDK mutex past its deadline")
	}
	if clients != 1 {
		t.Fatal("JIT retried client creation")
	}
}
