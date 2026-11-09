package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type scriptedDiscoveryPass struct {
	mu     sync.Mutex
	script []error
	calls  int
}

func (p *scriptedDiscoveryPass) next() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.script) == 0 {
		return nil
	}
	err := p.script[0]
	p.script = p.script[1:]
	return err
}

func (p *scriptedDiscoveryPass) ReclaimExpiredToolkitAvailableToolsRuns(context.Context, int) (int, error) {
	return 0, p.next()
}

func (p *scriptedDiscoveryPass) SweepOrphanToolkitDiscoveryArtifacts(context.Context, int) (int, error) {
	return 0, nil
}

// On main the runner returned its first database error and so ended Main
// (FU-275). It now logs each failed pass, backs off within bounds, resumes its
// normal interval after a clean pass, and returns only its owner's error.
func TestToolkitDiscoveryHousekeepingSurvivesADatabaseOutage(t *testing.T) {
	pgBouncerLoginRetry := fmt.Errorf("begin toolkit discovery retirement transaction: %w",
		&pgconn.PgError{Severity: "FATAL", Code: "08P01", Message: "server login has been failing, cached error: the database system is shutting down (server_login_retry)"})
	passes := &scriptedDiscoveryPass{script: []error{pgBouncerLoginRetry, pgBouncerLoginRetry, pgBouncerLoginRetry}}
	records := &logRecords{}
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	runtime := &currentToolkitDiscoveryRuntime{
		jobs:      passes,
		artifacts: passes,
		logger:    slog.New(slog.NewJSONHandler(records, nil)),
		jitter:    func(time.Duration) time.Duration { return 0 },
		wait: func(ctx context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			if len(delays) == 5 {
				cancel()
			}
			return ctx.Err()
		},
	}

	if err := runtime.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want only the owner's cancellation", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, toolkitDiscoveryPassInterval, toolkitDiscoveryPassInterval}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", delays, want)
	}
	entries := records.entries(t)
	if len(entries) != 3 {
		t.Fatalf("log lines = %d, want one per failed pass (3)", len(entries))
	}
	for i, entry := range entries {
		msg, _ := entry["msg"].(string)
		if entry["level"] != "ERROR" || !strings.HasPrefix(msg, "The toolkit discovery housekeeping pass failed; retrying in ") {
			t.Fatalf("line %d = %v", i, entry)
		}
		if entry["cause"] != "server login has been failing, cached error: the database system is shutting down (server_login_retry) (SQLSTATE 08P01)" {
			t.Fatalf("line %d cause = %v", i, entry["cause"])
		}
	}
}

func TestToolkitDiscoveryHousekeepingStopsAtOnceWhenItsOwnerEnds(t *testing.T) {
	passes := &scriptedDiscoveryPass{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := &currentToolkitDiscoveryRuntime{jobs: passes, artifacts: passes}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want the owner's cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop when its owner ended")
	}
}
