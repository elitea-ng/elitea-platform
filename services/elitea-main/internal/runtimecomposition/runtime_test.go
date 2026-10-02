package runtimecomposition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRuntimeCloseRemovesOnlyOwnedSandboxStaging(t *testing.T) {
	parent := t.TempDir()
	owned := filepath.Join(parent, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, "other")
	if err := os.WriteFile(other, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{sandboxSpoolDir: owned}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned staging remains: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

type publisherRunnerStub struct {
	started chan struct{}
	stopped chan struct{}
	err     error
}

func (s *publisherRunnerStub) Run(ctx context.Context) error {
	close(s.started)
	if s.err != nil {
		close(s.stopped)
		return s.err
	}
	<-ctx.Done()
	close(s.stopped)
	return ctx.Err()
}

type privateRunnerStub struct {
	started chan struct{}
	stopped chan struct{}
	err     error
	stop    chan struct{}
	once    sync.Once
}

func (s *privateRunnerStub) Serve(ctx context.Context) error {
	close(s.started)
	if s.err != nil {
		close(s.stopped)
		return s.err
	}
	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	close(s.stopped)
	return ctx.Err()
}

func (s *privateRunnerStub) Shutdown(context.Context) error {
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
	})
	return nil
}

func TestRuntimeRunDrainsBothComponentsOnOwningCancellation(t *testing.T) {
	publisher := &publisherRunnerStub{started: make(chan struct{}), stopped: make(chan struct{})}
	private := &privateRunnerStub{started: make(chan struct{}), stopped: make(chan struct{}), stop: make(chan struct{})}
	runtime := &Runtime{publisher: publisher, private: private}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-publisher.started
	<-private.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	<-publisher.stopped
	<-private.stopped
}

func TestRuntimeRunPropagatesFailureAndCancelsSibling(t *testing.T) {
	want := errors.New("private listener failed")
	publisher := &publisherRunnerStub{started: make(chan struct{}), stopped: make(chan struct{})}
	private := &privateRunnerStub{started: make(chan struct{}), stopped: make(chan struct{}), stop: make(chan struct{}), err: want}
	runtime := &Runtime{publisher: publisher, private: private}
	err := runtime.Run(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
	<-publisher.stopped
	<-private.stopped
}

func TestRuntimeShutdownWaitsForEveryOwnedRunner(t *testing.T) {
	publisher := &publisherRunnerStub{started: make(chan struct{}), stopped: make(chan struct{})}
	private := &privateRunnerStub{started: make(chan struct{}), stopped: make(chan struct{}), stop: make(chan struct{})}
	runtime := &Runtime{publisher: publisher, private: private}
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(context.Background()) }()
	<-publisher.started
	<-private.started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want shutdown cancellation", err)
	}
	select {
	case <-publisher.stopped:
	default:
		t.Fatal("Shutdown returned before publisher exit")
	}
	select {
	case <-private.stopped:
	default:
		t.Fatal("Shutdown returned before private listener exit")
	}
}
