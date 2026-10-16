package main

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeDatabasePoolsKeepReplayAndTerminalEffectsAvailableUnderMixedLoad(
	t *testing.T,
) {
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL runtime-pool integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pools, err := openRuntimeDatabasePools(
		ctx,
		databaseURL,
		runtimecomposition.PhaseOneDatabasePoolLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()

	var heldConnections []*pgxpool.Conn
	defer func() { releaseRuntimeTestConnections(heldConnections) }()
	heldConnections = acquireEveryConnection(t, pools.Replay)
	assertPoolAcquireTimesOut(t, pools.Replay)
	assertPoolQuery(t, pools.TerminalEffects)

	releaseRuntimeTestConnections(heldConnections)
	heldConnections = acquireEveryConnection(t, pools.TerminalEffects)
	assertPoolAcquireTimesOut(t, pools.TerminalEffects)
	assertPoolQuery(t, pools.Replay)

	releaseRuntimeTestConnections(heldConnections)
	heldConnections = nil
	pools.Replay.Close()
	assertPoolQuery(t, pools.TerminalEffects)
}

func acquireEveryConnection(t *testing.T, pool *pgxpool.Pool) []*pgxpool.Conn {
	t.Helper()
	connections := make([]*pgxpool.Conn, 0, pool.Config().MaxConns)
	for range pool.Config().MaxConns {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		connection, err := pool.Acquire(ctx)
		cancel()
		if err != nil {
			releaseRuntimeTestConnections(connections)
			t.Fatalf("acquire bounded pool connection: %v", err)
		}
		connections = append(connections, connection)
	}
	return connections
}

func assertPoolAcquireTimesOut(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	connection, err := pool.Acquire(ctx)
	if connection != nil {
		connection.Release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("saturated pool acquire error=%v, want=%v", err, context.DeadlineExceeded)
	}
}

func assertPoolQuery(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("isolated pool query failed: %v", err)
	}
	if one != 1 {
		t.Fatalf("isolated pool query returned %d", one)
	}
}

func releaseRuntimeTestConnections(connections []*pgxpool.Conn) {
	for _, connection := range connections {
		connection.Release()
	}
}
