package epoch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// ContextKeys are the epoch keys a stored security context pins: the agent
// binding, the mandate and the delegation it names, at the epochs admission
// recorded. Absent references add no key.
func ContextKeys(m *runtimev1.ExecutionSecurityContextV1) []SecurityEpoch {
	var keys []SecurityEpoch
	if id := m.GetAgentBindingId(); id != "" {
		keys = append(keys, SecurityEpoch{Kind: KindBinding, ID: id, Value: m.GetAgentBindingEpoch()})
	}
	if id := m.GetMandateId(); id != "" {
		keys = append(keys, SecurityEpoch{Kind: KindMandate, ID: id, Value: m.GetMandateEpoch()})
	}
	if id := m.GetDelegationId(); id != "" {
		keys = append(keys, SecurityEpoch{Kind: KindDelegation, ID: id, Value: m.GetDelegationEpoch()})
	}
	return keys
}

// StoredContext is an execution's stored security context bytes.
type StoredContext struct {
	Bytes  []byte
	Digest [32]byte
}

// ContextLoader reads an execution's stored security context; found is
// false for an execution that has none (it is not governed).
type ContextLoader interface {
	LoadSecurityContext(ctx context.Context, executionID string) (stored StoredContext, found bool, err error)
}

// PostgresContextLoader reads elitea_runtime.execution_security_contexts.
type PostgresContextLoader struct {
	db interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

// NewPostgresContextLoader builds the loader.
func NewPostgresContextLoader(db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) (*PostgresContextLoader, error) {
	if db == nil {
		return nil, errors.New("epoch: a database is required")
	}
	return &PostgresContextLoader{db: db}, nil
}

// LoadSecurityContext implements ContextLoader.
func (l *PostgresContextLoader) LoadSecurityContext(ctx context.Context, executionID string) (StoredContext, bool, error) {
	var stored StoredContext
	var digest []byte
	err := l.db.QueryRow(ctx, `
SELECT context_bytes, digest FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`,
		executionID).Scan(&stored.Bytes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredContext{}, false, nil
	}
	if err != nil {
		return StoredContext{}, false, fmt.Errorf("epoch: read the execution security context: %w", err)
	}
	if len(digest) != len(stored.Digest) {
		return StoredContext{}, false, errors.New("epoch: the stored security context digest is malformed")
	}
	copy(stored.Digest[:], digest)
	return stored, true, nil
}

// ErrContextInvalid is a stored context that does not verify. The caller
// fails closed.
var ErrContextInvalid = errors.New("epoch: the stored security context does not verify")

// RunChecker answers whether a governed run's authority is still current:
// the epochs its stored security context pins. A run with no stored context
// is not governed and is never checked. Stored contexts are immutable, so
// the keys of an execution are remembered (bounded) and a renewal pays one
// statement: the epoch read.
type RunChecker struct {
	loader  ContextLoader
	checker Checker

	mu    sync.Mutex
	memo  map[string]runKeys
	order []string
}

type runKeys struct {
	governed bool
	keys     []SecurityEpoch
}

const maxRememberedRuns = 4096

// NewRunChecker builds the checker.
func NewRunChecker(loader ContextLoader, checker Checker) (*RunChecker, error) {
	if loader == nil || checker == nil {
		return nil, errors.New("epoch: a context loader and a checker are required")
	}
	return &RunChecker{loader: loader, checker: checker, memo: map[string]runKeys{}}, nil
}

// Check answers governed=false for a run with no stored context. For a
// governed run it answers the stale keys (none: still current).
func (c *RunChecker) Check(ctx context.Context, executionID string) (governed bool, stale []SecurityEpoch, err error) {
	run, err := c.keys(ctx, executionID)
	if err != nil {
		return false, nil, err
	}
	if !run.governed {
		return false, nil, nil
	}
	if len(run.keys) == 0 {
		return true, nil, nil
	}
	stale, err = c.checker.Current(ctx, run.keys)
	return true, stale, err
}

func (c *RunChecker) keys(ctx context.Context, executionID string) (runKeys, error) {
	c.mu.Lock()
	run, ok := c.memo[executionID]
	c.mu.Unlock()
	if ok {
		return run, nil
	}
	stored, found, err := c.loader.LoadSecurityContext(ctx, executionID)
	if err != nil {
		return runKeys{}, err
	}
	if found {
		message, err := securitycontext.Verify(stored.Bytes, securitycontext.Reference{Digest: runtimedomain.Digest(stored.Digest)})
		if err != nil {
			return runKeys{}, ErrContextInvalid
		}
		run = runKeys{governed: true, keys: ContextKeys(message)}
	}
	c.mu.Lock()
	if _, ok := c.memo[executionID]; !ok {
		if len(c.order) >= maxRememberedRuns {
			delete(c.memo, c.order[0])
			c.order = c.order[1:]
		}
		c.memo[executionID] = run
		c.order = append(c.order, executionID)
	}
	c.mu.Unlock()
	return run, nil
}

// PostgresAuthorityRevoker durably cancels a governed run whose authority
// was revoked: the same desired-state primitive every cancel uses. The
// worker observes CANCELLED, stops, and settlement writes the terminal
// state. A run already settled or no longer running is left alone.
type PostgresAuthorityRevoker struct {
	db interface {
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	}
}

// NewPostgresAuthorityRevoker builds the revoker.
func NewPostgresAuthorityRevoker(db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}) (*PostgresAuthorityRevoker, error) {
	if db == nil {
		return nil, errors.New("epoch: a database is required")
	}
	return &PostgresAuthorityRevoker{db: db}, nil
}

// RevokeAuthority implements the control server's AuthorityRevoker.
func (r *PostgresAuthorityRevoker) RevokeAuthority(ctx context.Context, executionID string, generation uint64) error {
	if executionID == "" || generation == 0 {
		return errors.New("epoch: an execution and a generation are required")
	}
	if _, err := r.db.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1 AND generation = $2
  AND desired_state = 'RUNNING'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`, executionID, int64(generation)); err != nil {
		return fmt.Errorf("epoch: cancel the revoked run: %w", err)
	}
	return nil
}
