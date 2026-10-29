package repos

// The recertification pass (ADR-0040 decision 5). It runs as the
// main.agent_recertification routine and moves registry rows:
//
//   - certified → due: recertify_by is within DueWindow, the published
//     release is no longer the certified one (a new release or a
//     rollback), or the certified release drifted (a pinned toolkit or
//     configuration changed, release_toolkit_drifted).
//   - certified or due → lapsed: recertify_by passed. With lapse_action
//     suspend the row becomes suspended and an agent-scope suspension of
//     source certification (ADR-0041) refuses its runs with
//     agent_certification_lapsed.
//
// The pass is idempotent: it re-asserts the suspension of every suspended
// row (a crash between the state move and the suspension, or a manual
// resume), and when lapse_action goes back to warn it moves suspended rows
// to lapsed and resumes their certification suspensions.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// ReleaseDriftChecker reports whether a recorded release drifted.
// *AgentReleases satisfies it.
type ReleaseDriftChecker interface {
	ReleaseDrifted(ctx context.Context, projectID, applicationID, releaseID int64) (bool, error)
}

// CertificationSuspender writes the kill switch rows of lapsed agents.
// *ExecutionSuspensionRepository satisfies it.
type CertificationSuspender interface {
	CreateSuspension(ctx context.Context, s safety.NewSuspension) (safety.Suspension, error)
}

// RecertificationDeps are the collaborators of one pass.
type RecertificationDeps struct {
	Drift     ReleaseDriftChecker
	Suspender CertificationSuspender
	// Notify is told every suspension the pass wrote or resumed (the gate's
	// cache invalidation).
	Notify func(id int64)
}

// DriftBatch bounds the releases one pass reconstructs.
const DriftBatch = 50

// RecertificationSummary counts what one pass moved.
type RecertificationSummary struct {
	Due, Lapsed, Suspended, Unsuspended, DriftChecked int
}

func (s RecertificationSummary) String() string {
	return fmt.Sprintf("due=%d lapsed=%d suspended=%d unsuspended=%d drift_checked=%d",
		s.Due, s.Lapsed, s.Suspended, s.Unsuspended, s.DriftChecked)
}

type registryKey struct{ projectID, applicationID int64 }

// RecertificationPass runs one pass.
func (s *AgentLifecycle) RecertificationPass(ctx context.Context, deps RecertificationDeps) (RecertificationSummary, error) {
	var summary RecertificationSummary
	policy, err := s.currentPolicy(ctx)
	if err != nil {
		return summary, err
	}
	now := s.now()

	// certified → due: the window, and a published release that moved.
	if err := s.moveRows(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'due', state_reason = $2, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE certification_state = 'certified' AND recertify_by IS NOT NULL AND recertify_by <= $1
RETURNING tenant_project_id, application_id`, []any{now.Add(agentlifecycle.DueWindow), agentlifecycle.ReasonRecertifyWindow},
		agentlifecycle.ActionCertificationDue, agentlifecycle.ReasonRecertifyWindow, &summary.Due); err != nil {
		return summary, err
	}
	if err := s.moveRows(ctx, `
UPDATE elitea_lifecycle.agent_registry AS registry
SET certification_state = 'due', state_reason = $1, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
FROM elitea_lifecycle.application_release_heads AS head
WHERE registry.certification_state = 'certified'
  AND head.tenant_project_id = registry.tenant_project_id AND head.application_id = registry.application_id
  AND head.release_id IS DISTINCT FROM registry.certified_release_id
RETURNING registry.tenant_project_id, registry.application_id`, []any{agentlifecycle.ReasonNewRelease},
		agentlifecycle.ActionCertificationDue, agentlifecycle.ReasonNewRelease, &summary.Due); err != nil {
		return summary, err
	}
	if deps.Drift != nil {
		checked, due, err := s.driftPass(ctx, deps.Drift)
		summary.DriftChecked += checked
		summary.Due += due
		if err != nil {
			return summary, err
		}
	}

	// certified or due → lapsed (or suspended).
	lapseTo := "lapsed"
	if policy.LapseAction == agentlifecycle.LapseSuspend {
		lapseTo = "suspended"
	}
	var lapsed []registryKey
	if err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = $2, state_reason = $3, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE certification_state IN ('certified', 'due') AND recertify_by IS NOT NULL AND recertify_by <= $1
RETURNING tenant_project_id, application_id`, now, lapseTo, agentlifecycle.ReasonRecertifyByPassed)
		if err != nil {
			return fmt.Errorf("agent lifecycle: lapse the certifications: %w", err)
		}
		lapsed, err = collectKeys(rows)
		if err != nil {
			return err
		}
		reason := agentlifecycle.ReasonLapseWarn
		if lapseTo == "suspended" {
			reason = agentlifecycle.ReasonLapseSuspend
		}
		for _, key := range lapsed {
			if err := s.append(ctx, tx, lifecycleEvent(key.projectID, agentlifecycle.ActionCertificationLapsed, 0,
				"agent", agentRef(key.applicationID), agentlifecycle.ReasonRecertifyByPassed, reason)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return summary, err
	}
	summary.Lapsed += len(lapsed)

	if policy.LapseAction == agentlifecycle.LapseSuspend {
		// A lapsed row of the warn era becomes suspended too.
		var promoted []registryKey
		if err := s.inTx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'suspended', state_reason = $1, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE certification_state = 'lapsed'
RETURNING tenant_project_id, application_id`, agentlifecycle.ReasonLapseSuspend)
			if err != nil {
				return fmt.Errorf("agent lifecycle: suspend the lapsed agents: %w", err)
			}
			promoted, err = collectKeys(rows)
			return err
		}); err != nil {
			return summary, err
		}
		_ = promoted // their suspension and audit event come from assertSuspensions
		suspended, err := s.assertSuspensions(ctx, deps)
		summary.Suspended += suspended
		if err != nil {
			return summary, err
		}
	} else {
		unsuspended, err := s.releaseSuspensions(ctx, deps)
		summary.Unsuspended += unsuspended
		if err != nil {
			return summary, err
		}
	}
	return summary, nil
}

func collectKeys(rows pgx.Rows) ([]registryKey, error) {
	defer rows.Close()
	var keys []registryKey
	for rows.Next() {
		var key registryKey
		if err := rows.Scan(&key.projectID, &key.applicationID); err != nil {
			return nil, fmt.Errorf("agent lifecycle: scan a registry key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// moveRows runs one state-moving statement and audits every row it moved.
func (s *AgentLifecycle) moveRows(ctx context.Context, statement string, args []any, action, reason string, counter *int) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, statement, args...)
		if err != nil {
			return fmt.Errorf("agent lifecycle: move the registry rows (%s): %w", reason, err)
		}
		keys, err := collectKeys(rows)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := s.append(ctx, tx, lifecycleEvent(key.projectID, action, 0, "agent", agentRef(key.applicationID), reason)); err != nil {
				return err
			}
		}
		*counter += len(keys)
		return nil
	})
}

// driftPass reconstructs the certified release of the certified agents
// checked longest ago, at most DriftBatch of them.
func (s *AgentLifecycle) driftPass(ctx context.Context, drift ReleaseDriftChecker) (int, int, error) {
	rows, err := s.pool.Query(ctx, `
SELECT tenant_project_id, application_id, certified_release_id
FROM elitea_lifecycle.agent_registry
WHERE certification_state = 'certified' AND certified_release_id IS NOT NULL
ORDER BY drift_checked_at NULLS FIRST, tenant_project_id, application_id
LIMIT $1`, DriftBatch)
	if err != nil {
		return 0, 0, fmt.Errorf("agent lifecycle: read the drift batch: %w", err)
	}
	type candidate struct {
		key       registryKey
		releaseID int64
	}
	var batch []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.key.projectID, &c.key.applicationID, &c.releaseID); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("agent lifecycle: scan the drift batch: %w", err)
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	checked, due := 0, 0
	for _, c := range batch {
		drifted, err := drift.ReleaseDrifted(ctx, c.key.projectID, c.key.applicationID, c.releaseID)
		if err != nil {
			// One unreadable project does not stop the pass; it is checked
			// again next time.
			continue
		}
		checked++
		err = s.inTx(ctx, func(tx pgx.Tx) error {
			if !drifted {
				_, err := tx.Exec(ctx, `UPDATE elitea_lifecycle.agent_registry SET drift_checked_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, c.key.projectID, c.key.applicationID)
				return err
			}
			tag, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'due', state_reason = $4, state_changed_at = clock_timestamp(),
    drift_checked_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2 AND certification_state = 'certified' AND certified_release_id = $3`,
				c.key.projectID, c.key.applicationID, c.releaseID, agentlifecycle.ReasonToolkitDrifted)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			due++
			return s.append(ctx, tx, lifecycleEvent(c.key.projectID, agentlifecycle.ActionCertificationDue, 0,
				"agent", agentRef(c.key.applicationID), agentlifecycle.ReasonToolkitDrifted))
		})
		if err != nil {
			return checked, due, fmt.Errorf("agent lifecycle: record the drift check: %w", err)
		}
	}
	return checked, due, nil
}

// assertSuspensions writes the certification suspension of every
// suspended row that has no active agent suspension.
func (s *AgentLifecycle) assertSuspensions(ctx context.Context, deps RecertificationDeps) (int, error) {
	if deps.Suspender == nil {
		return 0, errors.New("agent lifecycle: lapse_action is suspend but no suspension writer is composed")
	}
	rows, err := s.pool.Query(ctx, `
SELECT registry.tenant_project_id, registry.application_id
FROM elitea_lifecycle.agent_registry AS registry
WHERE registry.certification_state = 'suspended'
  AND NOT EXISTS (
      SELECT 1 FROM elitea_runtime.execution_suspensions AS suspension
      WHERE suspension.scope = 'agent' AND suspension.project_id = registry.tenant_project_id
        AND suspension.scope_key = registry.application_id::text AND suspension.resumed_at IS NULL)`)
	if err != nil {
		return 0, fmt.Errorf("agent lifecycle: read the agents to suspend: %w", err)
	}
	keys, err := collectKeys(rows)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, key := range keys {
		created, err := deps.Suspender.CreateSuspension(ctx, safety.NewSuspension{
			Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: key.projectID, ApplicationID: key.applicationID},
			Reason: "The agent's certification lapsed (ADR-0040). Recertify it to run it again.",
			Source: safety.SourceCertification, ResumeRequires: safety.ResumeGovernanceRole,
		})
		var already *safety.AlreadySuspendedError
		if errors.As(err, &already) {
			continue
		}
		if err != nil {
			return written, fmt.Errorf("agent lifecycle: suspend agent %d: %w", key.applicationID, err)
		}
		written++
		if deps.Notify != nil {
			deps.Notify(created.ID)
		}
		if err := s.inTx(ctx, func(tx pgx.Tx) error {
			return s.append(ctx, tx, lifecycleEvent(key.projectID, agentlifecycle.ActionCertificationSuspended, 0,
				"agent", agentRef(key.applicationID), agentlifecycle.ReasonLapseSuspend,
				"suspension:"+strconv.FormatInt(created.ID, 10)))
		}); err != nil {
			return written, err
		}
	}
	return written, nil
}

// releaseSuspensions undoes lapse_action suspend after it went back to
// warn: suspended rows become lapsed and their certification suspensions
// are resumed. A manual suspension is not touched.
func (s *AgentLifecycle) releaseSuspensions(ctx context.Context, deps RecertificationDeps) (int, error) {
	var resumed []int64
	var keys []registryKey
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'lapsed', state_reason = $1, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE certification_state = 'suspended'
RETURNING tenant_project_id, application_id`, agentlifecycle.ReasonLapseWarn)
		if err != nil {
			return fmt.Errorf("agent lifecycle: move the suspended agents to lapsed: %w", err)
		}
		keys, err = collectKeys(rows)
		if err != nil {
			return err
		}
		for _, key := range keys {
			var id int64
			err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.execution_suspensions
SET resumed_at = greatest(clock_timestamp(), created_at), resumed_by = 0, resume_reason = $3
WHERE scope = 'agent' AND project_id = $1 AND scope_key = $2 AND source = 'certification' AND resumed_at IS NULL
RETURNING id`, key.projectID, agentRef(key.applicationID),
				"lapse_action is warn: a lapsed certification no longer suspends the agent.").Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("agent lifecycle: resume a certification suspension: %w", err)
			}
			resumed = append(resumed, id)
			if err := s.append(ctx, tx, lifecycleEvent(key.projectID, agentlifecycle.ActionCertificationLapsed, 0,
				"agent", agentRef(key.applicationID), agentlifecycle.ReasonLapseWarn,
				"suspension_resumed:"+strconv.FormatInt(id, 10))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if deps.Notify != nil {
		for _, id := range resumed {
			deps.Notify(id)
		}
	}
	return len(keys), nil
}

// lifecycleClock lets a test move the pass's clock.
func (s *AgentLifecycle) lifecycleClock(now func() time.Time) { s.now = now }
