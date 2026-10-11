package runtimecomposition

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type tombstoneSweeperStub struct {
	deferred bool
	runs     int
}

func (s *tombstoneSweeperStub) Deferred() bool { return s.deferred }
func (s *tombstoneSweeperStub) RunOnce(context.Context) (int, error) {
	s.runs++
	return 0, nil
}

// While vector deletion is deferred the reconciler logs once and idles until
// shutdown: it never runs a sweep, so it neither polls nor counts attempts.
func TestIndexTombstoneReconcilerIdlesWhileVectorDeletionIsDeferred(t *testing.T) {
	var logs bytes.Buffer
	sweeper := &tombstoneSweeperStub{deferred: true}
	reconciler, err := newIndexTombstoneReconciler(sweeper, time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := reconciler.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the context's error", err)
	}
	if sweeper.runs != 0 {
		t.Fatalf("a deferred sweeper ran %d sweeps", sweeper.runs)
	}
	if strings.Count(logs.String(), "deferred") != 1 {
		t.Fatalf("want the deferral logged exactly once, got: %s", logs.String())
	}
}

func TestIndexTombstoneReconcilerSweepsRepeatedlyWhenDeletionIsReal(t *testing.T) {
	sweeper := &tombstoneSweeperStub{}
	reconciler, err := newIndexTombstoneReconciler(sweeper, time.Millisecond, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = reconciler.Run(ctx)
	if sweeper.runs < 2 {
		t.Fatalf("sweeps = %d, want a loop", sweeper.runs)
	}
	if _, err := newIndexTombstoneReconciler(nil, time.Second, slog.Default()); err == nil {
		t.Fatal("a reconciler without a sweeper must be refused")
	}
}
