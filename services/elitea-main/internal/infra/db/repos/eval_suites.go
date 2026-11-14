package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// EvalSuitesRepo stores evaluation suites (run settings and limits) in the
// project's tenant schema (tenant/0244_eval_suites_human_scores.sql).
type EvalSuitesRepo struct {
	pool *pgxpool.Pool
}

func NewEvalSuitesRepo(pool *pgxpool.Pool) *EvalSuitesRepo { return &EvalSuitesRepo{pool: pool} }

const evalSuiteColumns = `
	id::text,
	COALESCE(uuid::text, ''),
	name,
	COALESCE(description, ''),
	token_limit_per_case::bigint,
	cost_limit_per_case::float8,
	token_limit_per_run::bigint,
	cost_limit_per_run::float8,
	on_run_limit,
	created_at,
	updated_at`

func scanEvalSuite(row pgx.Row) (evaluation.Suite, error) {
	var suite evaluation.Suite
	var createdAt, updatedAt time.Time
	err := row.Scan(&suite.ID, &suite.UUID, &suite.Name, &suite.Description,
		&suite.TokenLimitPerCase, &suite.CostLimitPerCase, &suite.TokenLimitPerRun, &suite.CostLimitPerRun,
		&suite.OnRunLimit, &createdAt, &updatedAt)
	if err != nil {
		return evaluation.Suite{}, err
	}
	suite.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	suite.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return suite, nil
}

func (r *EvalSuitesRepo) ListSuites(ctx context.Context, projectID string) ([]evaluation.Suite, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(
		`SELECT `+evalSuiteColumns+` FROM %s.eval_suites ORDER BY lower(name), id`, schema))
	if err != nil {
		return nil, fmt.Errorf("eval suites: list: %w", err)
	}
	defer rows.Close()
	suites := []evaluation.Suite{}
	for rows.Next() {
		suite, scanErr := scanEvalSuite(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval suites: scan: %w", scanErr)
		}
		suites = append(suites, suite)
	}
	return suites, rows.Err()
}

func (r *EvalSuitesRepo) GetSuite(ctx context.Context, projectID, suiteID string) (evaluation.Suite, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Suite{}, err
	}
	id, err := parseEvalID(suiteID, "suite")
	if err != nil {
		return evaluation.Suite{}, err
	}
	suite, err := scanEvalSuite(r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT `+evalSuiteColumns+` FROM %s.eval_suites WHERE id = $1`, schema), id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Suite{}, apierr.NotFound("suite not found")
		}
		return evaluation.Suite{}, fmt.Errorf("eval suites: get: %w", err)
	}
	return suite, nil
}

func (r *EvalSuitesRepo) CreateSuite(ctx context.Context, projectID string, in evaluation.SuiteWrite) (evaluation.Suite, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Suite{}, err
	}
	suite, err := scanEvalSuite(r.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_suites (
			name, description, token_limit_per_case, cost_limit_per_case,
			token_limit_per_run, cost_limit_per_run, on_run_limit
		) VALUES ($1, NULLIF($2, ''), $3::int, $4::float8, $5::int, $6::float8, $7)
		RETURNING `+evalSuiteColumns, schema),
		in.Name, in.Description, in.TokenLimitPerCase, in.CostLimitPerCase,
		in.TokenLimitPerRun, in.CostLimitPerRun, in.OnRunLimit))
	if err != nil {
		return evaluation.Suite{}, wrapEvalWrite("suite create", err)
	}
	return suite, nil
}

func (r *EvalSuitesRepo) UpdateSuite(ctx context.Context, projectID, suiteID string, in evaluation.SuiteWrite) (evaluation.Suite, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Suite{}, err
	}
	id, err := parseEvalID(suiteID, "suite")
	if err != nil {
		return evaluation.Suite{}, err
	}
	suite, err := scanEvalSuite(r.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.eval_suites
		SET name = $2, description = NULLIF($3, ''),
		    token_limit_per_case = $4::int, cost_limit_per_case = $5::float8,
		    token_limit_per_run = $6::int, cost_limit_per_run = $7::float8,
		    on_run_limit = $8, updated_at = now()
		WHERE id = $1
		RETURNING `+evalSuiteColumns, schema),
		id, in.Name, in.Description, in.TokenLimitPerCase, in.CostLimitPerCase,
		in.TokenLimitPerRun, in.CostLimitPerRun, in.OnRunLimit))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Suite{}, apierr.NotFound("suite not found")
		}
		return evaluation.Suite{}, wrapEvalWrite("suite update", err)
	}
	return suite, nil
}

func (r *EvalSuitesRepo) DeleteSuite(ctx context.Context, projectID, suiteID string) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := parseEvalID(suiteID, "suite")
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.eval_suites WHERE id = $1`, schema), id)
	if err != nil {
		return fmt.Errorf("eval suites: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("suite not found")
	}
	return nil
}
