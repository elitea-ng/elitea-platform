package repos

// The evaluation release gate's storage (ADR-0040 decision 4, issue #1497,
// shared 0203): gate suite config, gates and their runs in
// elitea_lifecycle, and the tenant reads the gate needs (the dataset and
// version owners). Evaluation runs themselves stay in the tenant's
// eval_runs; release_gate_runs names them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
)

// maxGateListRows bounds one gate listing.
const maxGateListRows = 100

// ReleaseGates stores release gates.
type ReleaseGates struct {
	pool *pgxpool.Pool
}

// NewReleaseGates builds the store over pool.
func NewReleaseGates(pool *pgxpool.Pool) (*ReleaseGates, error) {
	if pool == nil {
		return nil, errors.New("release gates: database pool is required")
	}
	return &ReleaseGates{pool: pool}, nil
}

var _ releasegate.Store = (*ReleaseGates)(nil)

const suiteColumns = `application_id, dataset_id, regression, criteria, reevaluate_every_days, updated_by, updated_at`

func scanSuite(row pgx.Row) (releasegate.SuiteConfig, error) {
	var suite releasegate.SuiteConfig
	var criteria []byte
	if err := row.Scan(&suite.ApplicationID, &suite.DatasetID, &suite.Regression, &criteria,
		&suite.ReevaluateEveryDays, &suite.UpdatedBy, &suite.UpdatedAt); err != nil {
		return releasegate.SuiteConfig{}, err
	}
	if err := json.Unmarshal(criteria, &suite.Criteria); err != nil {
		return releasegate.SuiteConfig{}, fmt.Errorf("release gates: unreadable criteria: %w", err)
	}
	return suite, nil
}

// ListSuites answers the agent's gate suites in dataset order. A suite
// whose dataset no longer exists is answered with DatasetMissing.
func (s *ReleaseGates) ListSuites(ctx context.Context, tenantProjectID, applicationID int64) ([]releasegate.SuiteConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+suiteColumns+`
		FROM elitea_lifecycle.release_gate_suites
		WHERE tenant_project_id = $1 AND application_id = $2
		ORDER BY dataset_id`, tenantProjectID, applicationID)
	if err != nil {
		return nil, fmt.Errorf("release gates: list suites: %w", err)
	}
	suites, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasegate.SuiteConfig, error) {
		return scanSuite(row)
	})
	if err != nil {
		return nil, fmt.Errorf("release gates: list suites: %w", err)
	}
	if len(suites) == 0 {
		return []releasegate.SuiteConfig{}, nil
	}

	ids := make([]int64, 0, len(suites))
	for _, suite := range suites {
		ids = append(ids, suite.DatasetID)
	}
	existing, err := s.existingDatasets(ctx, tenantProjectID, ids)
	if err != nil {
		return nil, err
	}
	for index := range suites {
		suites[index].DatasetMissing = !existing[suites[index].DatasetID]
	}
	return suites, nil
}

// existingDatasets answers which of the datasets exist in the tenant. A
// project whose schema or dataset table is gone has none.
func (s *ReleaseGates) existingDatasets(ctx context.Context, tenantProjectID int64, ids []int64) (map[int64]bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(tenantProjectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`SELECT id::bigint FROM %s.eval_datasets WHERE id = ANY($1::bigint[])`, schema), ids)
	if err != nil {
		if missingRelation(err) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("release gates: read datasets: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		if missingRelation(err) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("release gates: read datasets: %w", err)
	}
	existing := make(map[int64]bool, len(found))
	for _, id := range found {
		existing[id] = true
	}
	return existing, nil
}

// PutSuite stores a gate suite (insert or replace).
func (s *ReleaseGates) PutSuite(ctx context.Context, tenantProjectID int64, suite releasegate.SuiteConfig) (releasegate.SuiteConfig, error) {
	criteria, err := json.Marshal(suite.Criteria)
	if err != nil {
		return releasegate.SuiteConfig{}, fmt.Errorf("release gates: encode criteria: %w", err)
	}
	stored, err := scanSuite(s.pool.QueryRow(ctx, `
		INSERT INTO elitea_lifecycle.release_gate_suites (
			tenant_project_id, application_id, dataset_id, regression, criteria,
			reevaluate_every_days, updated_by)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
		ON CONFLICT (tenant_project_id, application_id, dataset_id) DO UPDATE SET
			regression = EXCLUDED.regression,
			criteria = EXCLUDED.criteria,
			reevaluate_every_days = EXCLUDED.reevaluate_every_days,
			updated_by = EXCLUDED.updated_by,
			updated_at = clock_timestamp()
		RETURNING `+suiteColumns,
		tenantProjectID, suite.ApplicationID, suite.DatasetID, suite.Regression, string(criteria),
		suite.ReevaluateEveryDays, suite.UpdatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "release_gate_suites_one_regression" {
			return releasegate.SuiteConfig{}, releasegate.ErrRegressionTaken
		}
		return releasegate.SuiteConfig{}, fmt.Errorf("release gates: store suite: %w", err)
	}
	return stored, nil
}

// DeleteSuite removes a gate suite. It answers whether one was removed.
func (s *ReleaseGates) DeleteSuite(ctx context.Context, tenantProjectID, applicationID, datasetID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM elitea_lifecycle.release_gate_suites
		WHERE tenant_project_id = $1 AND application_id = $2 AND dataset_id = $3`,
		tenantProjectID, applicationID, datasetID)
	if err != nil {
		return false, fmt.Errorf("release gates: delete suite: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DatasetApplication answers the agent of a dataset (nil for a
// project-wide dataset).
func (s *ReleaseGates) DatasetApplication(ctx context.Context, tenantProjectID, datasetID int64) (*int64, error) {
	schema, err := tenantSchema(strconv.FormatInt(tenantProjectID, 10))
	if err != nil {
		return nil, err
	}
	var owner *int64
	err = s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id::bigint FROM %s.eval_datasets WHERE id = $1`, schema), datasetID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || missingRelation(err) {
		return nil, releasegate.ErrDatasetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("release gates: read dataset: %w", err)
	}
	return owner, nil
}

// VersionApplication answers the agent of a version.
func (s *ReleaseGates) VersionApplication(ctx context.Context, tenantProjectID, versionID int64) (int64, error) {
	schema, err := tenantSchema(strconv.FormatInt(tenantProjectID, 10))
	if err != nil {
		return 0, err
	}
	var owner int64
	err = s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id::bigint FROM %s.application_versions WHERE id = $1`, schema), versionID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || missingRelation(err) {
		return 0, releasegate.ErrVersionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("release gates: read version: %w", err)
	}
	return owner, nil
}

const gateReleaseColumns = `release.id, release.application_version_id,
	COALESCE(release.definition ->> 'instructions', ''),
	COALESCE(release.definition -> 'llm_settings' ->> 'model_name', '')`

func scanGateRelease(row pgx.Row) (releasegate.Release, error) {
	var release releasegate.Release
	err := row.Scan(&release.ID, &release.ApplicationVersionID, &release.Instructions, &release.ModelName)
	return release, err
}

// Release answers a release of the agent.
func (s *ReleaseGates) Release(ctx context.Context, tenantProjectID, applicationID, releaseID int64) (releasegate.Release, error) {
	release, err := scanGateRelease(s.pool.QueryRow(ctx, `
		SELECT `+gateReleaseColumns+`
		FROM elitea_lifecycle.application_releases AS release
		WHERE release.tenant_project_id = $1 AND release.application_id = $2 AND release.id = $3`,
		tenantProjectID, applicationID, releaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return releasegate.Release{}, releasegate.ErrReleaseNotFound
	}
	if err != nil {
		return releasegate.Release{}, fmt.Errorf("release gates: read release: %w", err)
	}
	return release, nil
}

// CurrentRelease answers the agent's published release, or nil.
func (s *ReleaseGates) CurrentRelease(ctx context.Context, tenantProjectID, applicationID int64) (*releasegate.Release, error) {
	release, err := scanGateRelease(s.pool.QueryRow(ctx, `
		SELECT `+gateReleaseColumns+`
		FROM elitea_lifecycle.application_release_heads AS head
		JOIN elitea_lifecycle.application_releases AS release
		  ON release.id = head.release_id
		 AND release.tenant_project_id = head.tenant_project_id
		 AND release.application_id = head.application_id
		WHERE head.tenant_project_id = $1 AND head.application_id = $2`,
		tenantProjectID, applicationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release gates: read the current release: %w", err)
	}
	return &release, nil
}

// CreateGate stores a gate and its runs in one transaction. A Starting
// gate is stored with runs_started_at NULL (Gate.start adds its runs as
// they start, then FinishStart).
func (s *ReleaseGates) CreateGate(ctx context.Context, gate releasegate.GateResult) (int64, error) {
	suites, outcome, failures, err := encodeGateJSON(gate)
	if err != nil {
		return 0, err
	}
	var id int64
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO elitea_lifecycle.release_gates (
				tenant_project_id, application_id, kind, candidate_version_id,
				current_release_id, baseline_version_id, state, suites, outcome,
				failures, created_by, evaluated_at, runs_started_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb, $11,
			        CASE WHEN $7 = 'pending' THEN NULL ELSE clock_timestamp() END,
			        CASE WHEN $12 THEN NULL ELSE clock_timestamp() END)
			RETURNING id`,
			gate.TenantProjectID, gate.ApplicationID, gate.Kind, gate.CandidateVersionID,
			gate.CurrentReleaseID, gate.BaselineVersionID, gate.State, suites, outcome,
			failures, gate.CreatedBy, gate.Starting && gate.State == releasegate.StatePending).Scan(&id); err != nil {
			return err
		}
		for _, run := range gate.Runs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO elitea_lifecycle.release_gate_runs (
					gate_id, dataset_id, role, application_version_id, eval_run_id, reused)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				id, run.DatasetID, run.Role, run.ApplicationVersionID, run.EvalRunID, run.Reused); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("release gates: create gate: %w", err)
	}
	return id, nil
}

// AddGateRuns records runs a starting gate started.
func (s *ReleaseGates) AddGateRuns(ctx context.Context, tenantProjectID, gateID int64, runs []releasegate.GateRun) error {
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		for _, run := range runs {
			tag, err := tx.Exec(ctx, `
				INSERT INTO elitea_lifecycle.release_gate_runs (
					gate_id, dataset_id, role, application_version_id, eval_run_id, reused)
				SELECT id, $3, $4, $5, $6, $7 FROM elitea_lifecycle.release_gates
				WHERE tenant_project_id = $1 AND id = $2`,
				tenantProjectID, gateID, run.DatasetID, run.Role, run.ApplicationVersionID, run.EvalRunID, run.Reused)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return releasegate.ErrGateNotFound
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("release gates: record gate runs: %w", err)
	}
	return nil
}

// FinishStart marks every run of a starting gate started: from now on the
// gate is evaluated.
func (s *ReleaseGates) FinishStart(ctx context.Context, tenantProjectID, gateID int64) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE elitea_lifecycle.release_gates SET runs_started_at = clock_timestamp()
		WHERE tenant_project_id = $1 AND id = $2 AND runs_started_at IS NULL`, tenantProjectID, gateID); err != nil {
		return fmt.Errorf("release gates: finish the gate start: %w", err)
	}
	return nil
}

func encodeGateJSON(gate releasegate.GateResult) (string, string, string, error) {
	encode := func(value any, empty string) (string, error) {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("release gates: encode gate: %w", err)
		}
		if string(raw) == "null" {
			return empty, nil
		}
		return string(raw), nil
	}
	suites, err := encode(gate.Suites, "[]")
	if err != nil {
		return "", "", "", err
	}
	outcome, err := encode(gate.Outcome, "[]")
	if err != nil {
		return "", "", "", err
	}
	failures, err := encode(gate.Failures, "[]")
	if err != nil {
		return "", "", "", err
	}
	return suites, outcome, failures, nil
}

const gateColumns = `id, tenant_project_id, application_id, kind, candidate_version_id,
	current_release_id, baseline_version_id, state, suites, outcome, failures,
	created_by, created_at, evaluated_at, overridden_by, overridden_at, override_reason,
	runs_started_at IS NULL`

func scanGate(row pgx.Row) (releasegate.GateResult, error) {
	var gate releasegate.GateResult
	var suites, outcome, failures []byte
	var overriddenBy *int64
	var overriddenAt *time.Time
	var reason *string
	if err := row.Scan(&gate.ID, &gate.TenantProjectID, &gate.ApplicationID, &gate.Kind,
		&gate.CandidateVersionID, &gate.CurrentReleaseID, &gate.BaselineVersionID, &gate.State,
		&suites, &outcome, &failures, &gate.CreatedBy, &gate.CreatedAt, &gate.EvaluatedAt,
		&overriddenBy, &overriddenAt, &reason, &gate.Starting); err != nil {
		return releasegate.GateResult{}, err
	}
	for _, field := range []struct {
		raw    []byte
		target any
	}{{suites, &gate.Suites}, {outcome, &gate.Outcome}, {failures, &gate.Failures}} {
		if err := json.Unmarshal(field.raw, field.target); err != nil {
			return releasegate.GateResult{}, fmt.Errorf("release gates: unreadable gate: %w", err)
		}
	}
	if gate.Suites == nil {
		gate.Suites = []releasegate.SuiteConfig{}
	}
	if gate.Outcome == nil {
		gate.Outcome = []releasegate.SuiteOutcome{}
	}
	if gate.Failures == nil {
		gate.Failures = []releasegate.GateFailure{}
	}
	if overriddenBy != nil && overriddenAt != nil {
		gate.Override = &releasegate.Override{ActorID: *overriddenBy, At: *overriddenAt}
		if reason != nil {
			gate.Override.Reason = *reason
		}
	}
	gate.Runs = []releasegate.GateRun{}
	return gate, nil
}

// GetGate answers one gate of the project with its runs.
func (s *ReleaseGates) GetGate(ctx context.Context, tenantProjectID, gateID int64) (releasegate.GateResult, error) {
	gate, err := scanGate(s.pool.QueryRow(ctx, `
		SELECT `+gateColumns+`
		FROM elitea_lifecycle.release_gates
		WHERE tenant_project_id = $1 AND id = $2`, tenantProjectID, gateID))
	if errors.Is(err, pgx.ErrNoRows) {
		return releasegate.GateResult{}, releasegate.ErrGateNotFound
	}
	if err != nil {
		return releasegate.GateResult{}, fmt.Errorf("release gates: read gate: %w", err)
	}
	gates := []releasegate.GateResult{gate}
	if err := s.attachRuns(ctx, gates); err != nil {
		return releasegate.GateResult{}, err
	}
	return gates[0], nil
}

// ListGates answers the agent's gates, newest first.
func (s *ReleaseGates) ListGates(ctx context.Context, tenantProjectID, applicationID int64, limit int) ([]releasegate.GateResult, error) {
	if limit <= 0 || limit > maxGateListRows {
		limit = maxGateListRows
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+gateColumns+`
		FROM elitea_lifecycle.release_gates
		WHERE tenant_project_id = $1 AND application_id = $2
		ORDER BY id DESC
		LIMIT $3`, tenantProjectID, applicationID, limit)
	if err != nil {
		return nil, fmt.Errorf("release gates: list gates: %w", err)
	}
	gates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasegate.GateResult, error) {
		return scanGate(row)
	})
	if err != nil {
		return nil, fmt.Errorf("release gates: list gates: %w", err)
	}
	if err := s.attachRuns(ctx, gates); err != nil {
		return nil, err
	}
	if gates == nil {
		gates = []releasegate.GateResult{}
	}
	return gates, nil
}

func (s *ReleaseGates) attachRuns(ctx context.Context, gates []releasegate.GateResult) error {
	if len(gates) == 0 {
		return nil
	}
	index := make(map[int64]int, len(gates))
	ids := make([]int64, 0, len(gates))
	for position, gate := range gates {
		index[gate.ID] = position
		ids = append(ids, gate.ID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT gate_id, dataset_id, role, application_version_id, eval_run_id, reused
		FROM elitea_lifecycle.release_gate_runs
		WHERE gate_id = ANY($1::bigint[])
		ORDER BY gate_id, dataset_id, role DESC`, ids)
	if err != nil {
		return fmt.Errorf("release gates: read gate runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gateID int64
		var run releasegate.GateRun
		if err := rows.Scan(&gateID, &run.DatasetID, &run.Role, &run.ApplicationVersionID, &run.EvalRunID, &run.Reused); err != nil {
			return fmt.Errorf("release gates: read gate runs: %w", err)
		}
		position := index[gateID]
		gates[position].Runs = append(gates[position].Runs, run)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("release gates: read gate runs: %w", err)
	}
	return nil
}

// SettleGate stores the outcome of a pending gate; a settled gate is left
// as it is.
func (s *ReleaseGates) SettleGate(ctx context.Context, tenantProjectID, gateID int64, state string, outcome []releasegate.SuiteOutcome, failures []releasegate.GateFailure) error {
	_, encodedOutcome, encodedFailures, err := encodeGateJSON(releasegate.GateResult{Outcome: outcome, Failures: failures})
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE elitea_lifecycle.release_gates
		SET state = $3, outcome = $4::jsonb, failures = $5::jsonb, evaluated_at = clock_timestamp(),
		    runs_started_at = COALESCE(runs_started_at, clock_timestamp())
		WHERE tenant_project_id = $1 AND id = $2 AND state = 'pending'`,
		tenantProjectID, gateID, state, encodedOutcome, encodedFailures); err != nil {
		return fmt.Errorf("release gates: settle gate: %w", err)
	}
	return nil
}

// OverrideGate records the override of a failed gate that has none.
func (s *ReleaseGates) OverrideGate(ctx context.Context, tenantProjectID, gateID, actorID int64, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE elitea_lifecycle.release_gates
		SET overridden_by = $3, overridden_at = clock_timestamp(), override_reason = $4
		WHERE tenant_project_id = $1 AND id = $2 AND state = 'failed' AND overridden_by IS NULL`,
		tenantProjectID, gateID, actorID, reason)
	if err != nil {
		return fmt.Errorf("release gates: override gate: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	gate, err := s.GetGate(ctx, tenantProjectID, gateID)
	if err != nil {
		return err
	}
	if gate.Override != nil {
		return releasegate.ErrAlreadyOverridden
	}
	return releasegate.ErrGateNotFailed
}

// PendingGates answers pending gates of every project, oldest first.
func (s *ReleaseGates) PendingGates(ctx context.Context, limit int) ([]releasegate.GateKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_project_id, id
		FROM elitea_lifecycle.release_gates
		WHERE state = 'pending'
		ORDER BY id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("release gates: pending gates: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasegate.GateKey, error) {
		var key releasegate.GateKey
		err := row.Scan(&key.TenantProjectID, &key.GateID)
		return key, err
	})
	if err != nil {
		return nil, fmt.Errorf("release gates: pending gates: %w", err)
	}
	return keys, nil
}

// DueScheduled answers the agents whose scheduled re-evaluation is due:
// a published release, at least one suite with reevaluate_every_days, no
// pending scheduled gate, and no scheduled gate of the published release
// within the shortest cadence of its suites. The agents that waited
// longest come first.
func (s *ReleaseGates) DueScheduled(ctx context.Context, now time.Time, limit int) ([]releasegate.ScheduledDue, error) {
	rows, err := s.pool.Query(ctx, `
		WITH cadence AS (
			SELECT tenant_project_id, application_id,
			       min(reevaluate_every_days) AS days,
			       (array_agg(updated_by ORDER BY updated_at DESC))[1] AS actor
			FROM elitea_lifecycle.release_gate_suites
			WHERE reevaluate_every_days IS NOT NULL
			GROUP BY tenant_project_id, application_id
		)
		SELECT cadence.tenant_project_id, cadence.application_id, head.release_id, cadence.actor
		FROM cadence
		JOIN elitea_lifecycle.application_release_heads AS head
		  ON head.tenant_project_id = cadence.tenant_project_id
		 AND head.application_id = cadence.application_id
		LEFT JOIN LATERAL (
			SELECT max(gate.created_at) AS last_at,
			       bool_or(gate.state = 'pending') AS pending
			FROM elitea_lifecycle.release_gates AS gate
			WHERE gate.tenant_project_id = cadence.tenant_project_id
			  AND gate.application_id = cadence.application_id
			  AND gate.kind = 'scheduled'
			  AND (gate.current_release_id = head.release_id OR gate.state = 'pending')
		) AS last ON true
		WHERE NOT COALESCE(last.pending, false)
		  AND (last.last_at IS NULL OR last.last_at <= $1::timestamptz - make_interval(days => cadence.days))
		ORDER BY last.last_at ASC NULLS FIRST, cadence.tenant_project_id, cadence.application_id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("release gates: due scheduled gates: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasegate.ScheduledDue, error) {
		var item releasegate.ScheduledDue
		err := row.Scan(&item.TenantProjectID, &item.ApplicationID, &item.ReleaseID, &item.ActorID)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("release gates: due scheduled gates: %w", err)
	}
	return due, nil
}

// ReferenceRuns answers the candidate runs of the earliest passed
// scheduled gate of the release.
func (s *ReleaseGates) ReferenceRuns(ctx context.Context, tenantProjectID, applicationID, releaseID int64) ([]releasegate.GateRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run.dataset_id, run.role, run.application_version_id, run.eval_run_id, run.reused
		FROM elitea_lifecycle.release_gate_runs AS run
		WHERE run.role = 'candidate'
		  AND run.gate_id = (
			SELECT gate.id FROM elitea_lifecycle.release_gates AS gate
			WHERE gate.tenant_project_id = $1 AND gate.application_id = $2
			  AND gate.kind = 'scheduled' AND gate.current_release_id = $3
			  AND gate.state = 'passed'
			ORDER BY gate.id
			LIMIT 1)
		ORDER BY run.dataset_id`, tenantProjectID, applicationID, releaseID)
	if err != nil {
		return nil, fmt.Errorf("release gates: reference runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasegate.GateRun, error) {
		var run releasegate.GateRun
		err := row.Scan(&run.DatasetID, &run.Role, &run.ApplicationVersionID, &run.EvalRunID, &run.Reused)
		return run, err
	})
	if err != nil {
		return nil, fmt.Errorf("release gates: reference runs: %w", err)
	}
	return runs, nil
}

// ScheduledGatesSince answers the scheduled gates of the release created
// at or after since, newest first.
func (s *ReleaseGates) ScheduledGatesSince(ctx context.Context, tenantProjectID, applicationID, releaseID int64, since time.Time, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM elitea_lifecycle.release_gates
		WHERE tenant_project_id = $1 AND application_id = $2
		  AND kind = 'scheduled' AND current_release_id = $3 AND created_at >= $4
		ORDER BY id DESC
		LIMIT $5`, tenantProjectID, applicationID, releaseID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("release gates: scheduled gates: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("release gates: scheduled gates: %w", err)
	}
	return ids, nil
}
