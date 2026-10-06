package main

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func compiledPoolConfig(t *testing.T, dsn string) runtimecomposition.Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "dsn")
	if err := os.WriteFile(path, []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	return runtimecomposition.Config{RustCompiledSnapshots: &runtimecomposition.CompiledSnapshotConfig{ProfilesFile: filepath.Join(root, "profiles.json"), ProfilesSHA256: strings.Repeat("a", 64), AgentStateDSNFile: path, Quota: repos.SnapshotQuota{GlobalEntries: 10, GlobalBytes: 1 << 30, TenantEntries: 2, TenantBytes: 1 << 28, PublishingTTL: time.Minute, ReadyTTL: time.Hour}}}
}
func TestCompiledSnapshotStatePoolDisabledNeverOpensOrReads(t *testing.T) {
	c := runtimecomposition.Config{}
	r, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		t.Fatal("disabled feature opened pool")
		return runtimePoolResource{}, nil
	})
	if err != nil || r.pool != nil || r.close != nil {
		t.Fatal(err)
	}
}
func TestCompiledSnapshotStatePoolOwnsExactBoundedOriginalDatabase(t *testing.T) {
	c := compiledPoolConfig(t, "postgres://fixture-only\n")
	closed := 0
	r, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(ctx context.Context, dsn string, spec runtimePoolSpec) (runtimePoolResource, error) {
		if dsn != "postgres://fixture-only" || spec.role != "compiled-snapshot-agentstate" || spec.maxConns != 4 {
			t.Fatal("wrong owning pool")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("unbounded constructor")
		}
		return runtimePoolResource{pool: new(pgxpool.Pool), close: func() { closed++ }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if closed != 1 {
		t.Fatal(closed)
	}
}
func TestCompiledSnapshotStatePoolRejectsUnsafeMaterialBeforeFactory(t *testing.T) {
	for _, dsn := range []string{"", "line1\nline2", "line\r\n", "line\x00"} {
		c := compiledPoolConfig(t, dsn)
		if _, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
			t.Fatal("unsafe material reached constructor")
			return runtimePoolResource{}, nil
		}); err == nil {
			t.Fatal("malformed material accepted")
		}
	}
	c := compiledPoolConfig(t, "postgres://fixture-only")
	if err := os.Chmod(c.RustCompiledSnapshots.AgentStateDSNFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		t.Fatal("public private material reached constructor")
		return runtimePoolResource{}, nil
	}); err == nil {
		t.Fatal("public DSN accepted")
	}
}
func TestCompiledSnapshotStatePoolSanitizesFailuresAndClosesPartialOwnership(t *testing.T) {
	c := compiledPoolConfig(t, "postgres://synthetic-password@fixture")
	closed := 0
	if _, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		return runtimePoolResource{close: func() { closed++ }}, errors.New("synthetic-password")
	}); err == nil || strings.Contains(err.Error(), "synthetic-password") {
		t.Fatal("constructor error was not sanitized")
	}
	if closed != 1 {
		t.Fatal("partial failed pool was not closed")
	}
	if _, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		return runtimePoolResource{close: func() { closed++ }}, nil
	}); err == nil {
		t.Fatal("incomplete resource accepted")
	}
	if closed != 2 {
		t.Fatal("incomplete pool was not closed")
	}
}

func TestCompiledSnapshotStatePoolPreservesCancellation(t *testing.T) {
	c := compiledPoolConfig(t, "postgres://fixture-only")
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		_, err := openCompiledSnapshotStatePoolWithFactory(context.Background(), c, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
			return runtimePoolResource{}, cause
		})
		if !errors.Is(err, cause) {
			t.Fatal("constructor lost cancellation identity", err)
		}
	}
}
