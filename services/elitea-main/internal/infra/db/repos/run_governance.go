package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// RunGovernance answers whether a run carries the governed credential
// profile when the process profile alone cannot say: a continuation inherits
// its scope's governed pin (pinExecutionAdmission), and a nested child its
// parent execution's security context. The connector catalogue freeze reads
// it so the pins it writes are the ones the command gate checks
// (governedRun = SecurityContextRef != nil, #1537 review item 3).
type RunGovernance struct {
	pool *pgxpool.Pool
}

var _ agentexecutionapp.CurrentAgentRunGovernance = (*RunGovernance)(nil)

// NewRunGovernance builds the reader.
func NewRunGovernance(pool *pgxpool.Pool) (*RunGovernance, error) {
	if pool == nil {
		return nil, errors.New("run governance database is required")
	}
	return &RunGovernance{pool: pool}, nil
}

// GovernedContinuationScope reports whether scope has a governed pin.
func (r *RunGovernance) GovernedContinuationScope(ctx context.Context, scope string) (bool, error) {
	if r == nil || strings.TrimSpace(scope) == "" {
		return false, nil
	}
	var governed bool
	if err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.execution_admission_profiles
    WHERE continuation_scope = $1 AND profile = 'elitea.credential-profile.governed.v1'
)`, scope).Scan(&governed); err != nil {
		return false, fmt.Errorf("read continuation governance: %w", err)
	}
	return governed, nil
}

// GovernedExecution reports whether executionID carries a security context.
func (r *RunGovernance) GovernedExecution(ctx context.Context, executionID string) (bool, error) {
	if r == nil || strings.TrimSpace(executionID) == "" {
		return false, nil
	}
	_, found, err := storedSecurityContext(ctx, func(ctx context.Context, sql string, args ...any) sqlRow { return r.pool.QueryRow(ctx, sql, args...) }, executionID)
	if err != nil {
		return false, err
	}
	return found, nil
}
