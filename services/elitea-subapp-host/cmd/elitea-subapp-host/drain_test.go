package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The SIGTERM path's drain: a run that ends inside the timeout ends the
// drain with it, and a zero timeout waits for nothing (server.Stop then
// cancels, as before the drain existed).
func TestDrainInvocations(t *testing.T) {
	manager := spi.NewManager(nil, time.Hour, slog.Default())
	t.Cleanup(manager.Stop)
	release := make(chan struct{})
	if _, err := manager.Submit(context.Background(), "Echo", "echo",
		func(context.Context, *spi.Context) (map[string]any, error) {
			<-release
			return map[string]any{}, nil
		}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	drainInvocations(manager, 0, slog.Default())
	if manager.InFlight() != 1 || time.Since(start) > time.Second {
		t.Fatalf("a zero drain waited: in flight %d after %s", manager.InFlight(), time.Since(start))
	}
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	start = time.Now()
	drainInvocations(manager, 10*time.Second, slog.Default())
	if manager.InFlight() != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("the drain did not end with the run: in flight %d after %s", manager.InFlight(), time.Since(start))
	}
}
