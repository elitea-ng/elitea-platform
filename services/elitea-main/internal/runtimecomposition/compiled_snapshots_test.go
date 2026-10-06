package runtimecomposition

import (
	"context"
	"errors"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

type compiledMaintenanceStore struct{ storage.ObjectStore }
type compiledMaintenanceIndex struct {
	calls    int
	failure  error
	deadline time.Time
	owner    string
	limit    int
}

func (i *compiledMaintenanceIndex) ClaimExpired(ctx context.Context, owner string, limit int, lease time.Duration) ([]domain.SnapshotEviction, error) {
	i.calls++
	i.owner = owner
	i.limit = limit
	i.deadline, _ = ctx.Deadline()
	if lease != 30*time.Second {
		return nil, errors.New("wrong eviction lease")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, i.failure
}
func (*compiledMaintenanceIndex) CompleteEviction(context.Context, domain.SnapshotEviction, func() error) error {
	return errors.New("no rows expected")
}
func compiledMaintenanceFixture(t *testing.T) (*executionReplayRetentionJanitor, *compiledMaintenanceIndex, *storage.SandboxBundleStore, *executionReplayPrunerStub) {
	t.Helper()
	p := &executionReplayPrunerStub{}
	janitor, err := newExecutionReplayRetentionJanitor(p, time.Minute, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	if err := os.Chmod(spool, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewSandboxBundleStore(&compiledMaintenanceStore{}, spool, 1)
	if err != nil {
		t.Fatal(err)
	}
	return janitor, &compiledMaintenanceIndex{}, store, p
}
func TestCompiledSnapshotDependenciesDefaultOffAndDistinctAgentState(t *testing.T) {
	if repo, profiles, err := configureCompiledSnapshotAuthority(context.Background(), Config{}, Dependencies{}); err != nil || repo != nil || profiles != nil {
		t.Fatal(repo, profiles, err)
	}
	pool := new(pgxpool.Pool)
	if validateCompiledSnapshotDependencies(Config{}, Dependencies{CompiledSnapshotStatePool: pool}) == nil {
		t.Fatal("disabled feature received pool")
	}
	cfg := compiledTestConfig()
	config := Config{RustCompiledSnapshots: &cfg}
	deps := Dependencies{CompiledSnapshotStatePool: pool, ObjectStore: &compiledMaintenanceStore{}}
	if err := validateCompiledSnapshotDependencies(config, deps); err != nil {
		t.Fatal(err)
	}
	deps.ControlPool = pool
	if validateCompiledSnapshotDependencies(config, deps) == nil {
		t.Fatal("business database used for original receipts")
	}
	deps.ControlPool = nil
	deps.ObjectStore = nil
	if validateCompiledSnapshotDependencies(config, deps) == nil {
		t.Fatal("cache enabled without content backend")
	}
}
func TestCompiledSnapshotMaintenanceUsesExistingSynchronousJanitor(t *testing.T) {
	j, index, store, p := compiledMaintenanceFixture(t)
	first := errors.New("replay unavailable")
	second := errors.New("cache unavailable")
	p.errors = []error{first}
	index.failure = second
	if err := attachCompiledSnapshotMaintenance(j, store, index, "instance-a"); err != nil {
		t.Fatal(err)
	}
	err := j.RunOnce(context.Background())
	if !errors.Is(err, first) || !errors.Is(err, second) || p.callCount() != 1 || index.calls != 1 || index.owner != "instance-a" || index.limit != 32 || time.Until(index.deadline) > 30*time.Second {
		t.Fatal("unjoined/unbounded maintenance", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := j.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("maintenance ignored shutdown", err)
	}
}
func TestCompiledSnapshotMaintenanceRejectsIncompleteComposition(t *testing.T) {
	j, index, store, _ := compiledMaintenanceFixture(t)
	for _, tc := range []struct {
		j     *executionReplayRetentionJanitor
		s     *storage.SandboxBundleStore
		i     domain.RustSnapshotEvictor
		owner string
	}{{nil, store, index, "a"}, {j, nil, index, "a"}, {j, store, nil, "a"}, {j, store, index, ""}} {
		if attachCompiledSnapshotMaintenance(tc.j, tc.s, tc.i, tc.owner) == nil {
			t.Fatal("incomplete cache janitor accepted")
		}
	}
}
