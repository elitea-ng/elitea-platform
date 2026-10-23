package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkloadSessionsRepository verifies certificate/session/producer bindings
// against the authoritative shared database and its clock. It deliberately
// exposes no process-local registration or fallback allowlist. Phase-one
// sessions are provisioned and revoked by the deployment control plane before
// a worker connects; this runtime never creates authority from caller input.
type WorkloadSessionsRepository struct {
	store sqlExecutor
}

func NewWorkloadSessionsRepository(pool *pgxpool.Pool) (*WorkloadSessionsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newWorkloadSessionsRepository(store)
}

func newWorkloadSessionsRepository(store sqlExecutor) (*WorkloadSessionsRepository, error) {
	if store == nil {
		return nil, errors.New("workload session database is required")
	}
	return &WorkloadSessionsRepository{store: store}, nil
}

func (r *WorkloadSessionsRepository) VerifyActiveSession(ctx context.Context, binding workloadauth.SessionBinding) error {
	if !validSessionBinding(binding) {
		return workloadauth.ErrWorkloadUnauthorized
	}
	var active bool
	err := r.store.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.workload_sessions
    WHERE workload_session_id = $1
      AND workload_identity = $2
      AND producer_id = $3
      AND issued_at <= clock_timestamp()
      AND expires_at > clock_timestamp()
      AND revoked_at IS NULL
)`, binding.WorkloadSessionID, binding.WorkloadIdentity, binding.ProducerID).Scan(&active)
	if err != nil {
		return fmt.Errorf("verify active workload session: %w", err)
	}
	if !active {
		return workloadauth.ErrWorkloadUnauthorized
	}
	return nil
}

func validSessionBinding(binding workloadauth.SessionBinding) bool {
	return boundedSessionPart(binding.WorkloadSessionID, 256) &&
		boundedSessionPart(binding.ProducerID, 256) &&
		boundedSessionPart(binding.WorkloadIdentity, 512)
}

func boundedSessionPart(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsAny(value, "\r\n\x00")
}

var _ workloadauth.SessionBindingVerifier = (*WorkloadSessionsRepository)(nil)
