package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/config"
	natsinfra "github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// A hanging dial must not block NATS() readers, and must end at the timeout.
func TestRedialNATS_HangingDialDoesNotBlockReaders(t *testing.T) {
	cfg := config.Config{HTTPAddr: "127.0.0.1:0", InitialPoolSize: 1, ProviderConcurrency: 1, NATSURL: "nats://127.0.0.1:4222"}
	hang := false
	srv, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), new(slog.LevelVar), nil, http.NewServeMux(),
		WithRedialTimeout(150*time.Millisecond),
		WithNATSConnector(func(ctx context.Context, _ natsinfra.Config) (NATSClient, error) {
			if !hang {
				return nil, errors.New("boot dial refused")
			}
			<-ctx.Done() // black hole: only the deadline ends it
			return nil, ctx.Err()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	hang = true

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, e := srv.RedialNATS(context.Background()); done <- e }()
	time.Sleep(30 * time.Millisecond) // dial is in flight

	read := make(chan struct{})
	go func() { _ = srv.NATS(); close(read) }()
	select {
	case <-read:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("NATS() blocked behind a hanging dial")
	}
	select {
	case e := <-done:
		if e == nil || time.Since(start) > 2*time.Second {
			t.Fatalf("redial err=%v after %v, want timeout error", e, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial ignored its timeout")
	}
}
