package runtimecomposition

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type agentAdmissionReservationStoreStub struct {
	mu        sync.Mutex
	errors    []error
	calls     int
	lastStale int64
	lastGc    int64
	started   chan struct{}
	release   chan struct{}
}

func (s *agentAdmissionReservationStoreStub) ReapAgentAdmissionReservations(
	ctx context.Context,
	staleSeconds,
	gcSeconds int64,
) (int64, error) {
	s.mu.Lock()
	s.calls++
	s.lastStale = staleSeconds
	s.lastGc = gcSeconds
	var err error
	if len(s.errors) > 0 {
		err = s.errors[0]
		s.errors = s.errors[1:]
	}
	started := s.started
	release := s.release
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 1, err
}

func (s *agentAdmissionReservationStoreStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *agentAdmissionReservationStoreStub) lastWindows() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStale, s.lastGc
}

func TestAgentAdmissionReservationReaperReportsFailureAndContinues(t *testing.T) {
	dependencyErr := errors.New("PostgreSQL reservation maintenance unavailable")
	store := &agentAdmissionReservationStoreStub{errors: []error{dependencyErr, nil}}
	var reported []error
	reaper, err := newAgentAdmissionReservationReaper(
		store,
		agentAdmissionReservationReaperPollInterval,
		func(err error) { reported = append(reported, err) },
	)
	if err != nil {
		t.Fatal(err)
	}
	var waits int
	reaper.wait = func(_ context.Context, delay time.Duration) error {
		if delay != agentAdmissionReservationReaperPollInterval {
			t.Fatalf("poll delay=%s", delay)
		}
		waits++
		if waits == 2 {
			return context.Canceled
		}
		return nil
	}
	if err := reaper.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v", err)
	}
	if store.callCount() != 2 || len(reported) != 1 || !errors.Is(reported[0], dependencyErr) {
		t.Fatalf("calls=%d reported=%v", store.callCount(), reported)
	}
	stale, gc := store.lastWindows()
	if stale != int64(agentAdmissionReservationStaleWindow.Seconds()) ||
		gc != int64(agentAdmissionReservationGcWindow.Seconds()) {
		t.Fatalf("reaper windows stale=%d gc=%d", stale, gc)
	}
}

func TestAgentAdmissionReservationReaperDoesNotOverlapAndCancelsActivePass(t *testing.T) {
	store := &agentAdmissionReservationStoreStub{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	reaper, err := newAgentAdmissionReservationReaper(
		store,
		time.Millisecond,
		func(error) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reaper.Run(ctx) }()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("reaper did not start its first pass")
	}
	select {
	case <-store.started:
		t.Fatal("reaper overlapped a second pass")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reaper did not stop after cancellation")
	}
	if store.callCount() != 1 {
		t.Fatalf("calls=%d", store.callCount())
	}
}

func TestAgentAdmissionReservationReaperRejectsIncompleteComposition(t *testing.T) {
	store := &agentAdmissionReservationStoreStub{}
	tests := []struct {
		store    agentAdmissionReservationStore
		interval time.Duration
		reporter func(error)
	}{
		{interval: time.Minute, reporter: func(error) {}},
		{store: store, reporter: func(error) {}},
		{store: store, interval: time.Minute},
	}
	for _, test := range tests {
		if reaper, err := newAgentAdmissionReservationReaper(
			test.store,
			test.interval,
			test.reporter,
		); err == nil || reaper != nil {
			t.Fatalf("reaper=%#v error=%v", reaper, err)
		}
	}
}
