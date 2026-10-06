package budgetwriteback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// recordingHandler keeps every log record, for "logged once per outage".
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) count(level slog.Level, contains string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, contains) {
			n++
		}
	}
	return n
}

// blockingFetcher idles until ctx ends: an attached consumer on a quiet stream.
type blockingFetcher struct{}

func (blockingFetcher) Fetch(ctx context.Context) ([]Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// lostFetcher reports the consumer gone on its first fetch.
type lostFetcher struct{ err error }

func (f lostFetcher) Fetch(context.Context) ([]Message, error) { return nil, f.err }

// R1: a scheduler that starts before the bootstrap (or NATS) keeps trying,
// with capped exponential backoff, logs the outage once at WARN, and attaches
// when the consumer appears — instead of binding once and giving up.
func TestSupervisorRetriesUntilAttachedAndLogsOncePerOutage(t *testing.T) {
	logs := &recordingHandler{}
	status := NewStatus()
	var (
		mu      sync.Mutex
		calls   int
		delays  []time.Duration
		running = make(chan struct{})
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := &Supervisor{
		Bind: func(context.Context) (*Consumer, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls <= 5 {
				return nil, fmt.Errorf("%w: not yet", ErrConsumerMissing)
			}
			close(running)
			return NewConsumer(blockingFetcher{}, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger()), nil
		},
		Status:     status,
		Logger:     slog.New(logs),
		MinBackoff: time.Second,
		MaxBackoff: 4 * time.Second,
	}
	sup.sleep = func(_ context.Context, d time.Duration) bool {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		if snap := status.Snapshot(); snap.State != StateWaiting || !strings.Contains(snap.LastError, "not yet") {
			t.Errorf("status while retrying = %+v, want waiting with the bind error", snap)
		}
		return true
	}
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never attached")
	}
	waitFor(t, func() bool { return status.Snapshot().State == StateAttached })
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Errorf("backoff = %v, want %v (doubling, capped at MaxBackoff)", delays, want)
	}
	if snap := status.Snapshot(); snap.Attempts != 6 || snap.LastError != "" {
		t.Errorf("attached status = %+v, want 6 attempts and no error", snap)
	}
	if n := logs.count(slog.LevelWarn, "cannot attach"); n != 1 {
		t.Errorf("logged the outage %d times at WARN, want once", n)
	}
	if n := logs.count(slog.LevelInfo, "after an outage"); n != 1 {
		t.Errorf("logged the recovery %d times at INFO, want once", n)
	}
}

// A consumer lost while draining (deleted, or its store gone) ends that
// drain loop, and the supervisor binds again.
func TestSupervisorReattachesWhenTheConsumerIsLost(t *testing.T) {
	status := NewStatus()
	var mu sync.Mutex
	calls := 0
	reattached := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := &Supervisor{
		Bind: func(context.Context) (*Consumer, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return NewConsumer(lostFetcher{err: jetstream.ErrConsumerDeleted}, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger()), nil
			}
			close(reattached)
			return NewConsumer(blockingFetcher{}, NewStore(&fakeDB{tx: &fakeTx{}}), quietLogger()), nil
		},
		Status: status,
		Logger: quietLogger(),
	}
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	select {
	case <-reattached:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not bind again after the consumer was lost")
	}
	cancel()
	<-done
}

// Run returns when ctx ends, also while it is waiting between attempts.
func TestSupervisorStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	status := NewStatus()
	sup := &Supervisor{
		Bind:       func(context.Context) (*Consumer, error) { return nil, errors.New("down") },
		Status:     status,
		Logger:     quietLogger(),
		MinBackoff: time.Hour,
	}
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	waitFor(t, func() bool { return status.Snapshot().State == StateWaiting })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when ctx ended during a backoff")
	}
}

func TestStatusServesTheState(t *testing.T) {
	s := NewStatus()
	for _, tc := range []struct {
		set  func()
		code int
		want string
	}{
		{func() {}, http.StatusOK, StateDisabled},
		{func() { s.set(StateWaiting, 3, errors.New("consumer missing")) }, http.StatusServiceUnavailable, StateWaiting},
		{func() { s.set(StateAttached, 4, nil) }, http.StatusOK, StateAttached},
		{func() { s.SetMisconfigured(errors.New("half an identity")) }, http.StatusServiceUnavailable, StateMisconfigured},
	} {
		tc.set()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz/budget-writeback", nil))
		var got Snapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tc.code || got.State != tc.want {
			t.Errorf("state %s: HTTP %d %+v, want %d", tc.want, rec.Code, got, tc.code)
		}
	}
}

func TestConsumerLost(t *testing.T) {
	for _, err := range []error{
		jetstream.ErrConsumerDeleted, jetstream.ErrConsumerNotFound, jetstream.ErrStreamNotFound,
		nats.ErrNoResponders, fmt.Errorf("fetch: %w", jetstream.ErrConsumerDeleted),
	} {
		if !consumerLost(err) {
			t.Errorf("%v: not treated as a lost consumer", err)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, nats.ErrTimeout, nats.ErrConnectionClosed, errors.New("x")} {
		if consumerLost(err) {
			t.Errorf("%v: treated as a lost consumer; a retry of the same handle can succeed", err)
		}
	}
}
