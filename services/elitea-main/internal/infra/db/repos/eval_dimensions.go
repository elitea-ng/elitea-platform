package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// EvalDimensionsRepo stores the Agent Evaluation dimension library in the
// project's own tenant schema (tenant/0130_eval_dimensions.sql).
type EvalDimensionsRepo struct {
	pool *pgxpool.Pool
}

func NewEvalDimensionsRepo(pool *pgxpool.Pool) *EvalDimensionsRepo {
	return &EvalDimensionsRepo{pool: pool}
}

// dimensionColumns is the ONE projection every read in this file uses.
//
// Written once rather than per query so a column added to the table cannot be
// selected by the create path and forgotten by the list path — which is how a
// field round-trips on save and then disappears on reload, with a 200 at every
// step.
const dimensionColumns = `
	id::text,
	COALESCE(uuid::text, ''),
	name,
	COALESCE(description, ''),
	tier,
	application_id,
	allowed_engines,
	scale_type,
	scale_min,
	scale_max,
	polarity,
	default_weight,
	default_target,
	COALESCE(default_target_operator, ''),
	COALESCE(code, ''),
	COALESCE(return_contract, ''),
	created_at,
	updated_at`

func scanDimension(row pgx.Row) (evaluation.Dimension, error) {
	var d evaluation.Dimension
	var createdAt, updatedAt time.Time
	err := row.Scan(
		&d.ID,
		&d.UUID,
		&d.Name,
		&d.Description,
		&d.Tier,
		&d.ApplicationID,
		&d.AllowedEngines,
		&d.ScaleType,
		&d.ScaleMin,
		&d.ScaleMax,
		&d.Polarity,
		&d.DefaultWeight,
		&d.DefaultTarget,
		&d.DefaultTargetOperator,
		&d.Code,
		&d.ReturnContract,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return evaluation.Dimension{}, err
	}
	d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	d.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return d, nil
}

// List returns the project library, optionally widened with ONE agent's ad-hoc
// dimensions.
//
// The predicate is deliberately not "everything in this schema". `agent_adhoc`
// rows belong to the agent named in `application_id`, so listing them without
// the filter would put every agent's private rubrics in every other agent's
// editor.
//
// An error is RETURNED rather than swallowed into an empty slice. Several
// listings in this package answer `[]` on a query failure, and that choice is
// why a missing table renders as "you have no dimensions" instead of as a
// fault — the exact reading tenant/0126's header records for the folder
// sidebar.
func (r *EvalDimensionsRepo) List(
	ctx context.Context,
	projectID string,
	filter evaluation.ListFilter,
) ([]evaluation.Dimension, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}

	q := fmt.Sprintf(`
		SELECT %s
		FROM %s.eval_dimensions
		WHERE tier = $1
		   OR ($2::boolean AND tier = $3)
		   OR (tier = $4 AND application_id IS NOT NULL AND application_id = $5::int)
		ORDER BY name ASC, id ASC`, dimensionColumns, s)

	rows, err := r.pool.Query(ctx, q,
		evaluation.TierProject,
		filter.IncludePlatform,
		evaluation.TierPlatform,
		evaluation.TierAgentAdhoc,
		filter.ApplicationID,
	)
	if err != nil {
		return nil, fmt.Errorf("eval dimensions: list: %w", err)
	}
	defer rows.Close()

	items := []evaluation.Dimension{}
	for rows.Next() {
		d, scanErr := scanDimension(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval dimensions: list scan: %w", scanErr)
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval dimensions: list: %w", err)
	}
	return items, nil
}

func (r *EvalDimensionsRepo) Create(
	ctx context.Context,
	projectID string,
	dimension evaluation.Dimension,
) (evaluation.Dimension, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Dimension{}, err
	}

	// RETURNING the full projection, not the id. The row that comes back is
	// the row that was stored, including every server-applied default, so the
	// client's cache after a create is what a reload would show. A create that
	// echoed the request body back would report success for whatever the
	// database silently altered.
	q := fmt.Sprintf(`
		INSERT INTO %s.eval_dimensions (
			name, description, tier, application_id, allowed_engines,
			scale_type, scale_min, scale_max, polarity, default_weight,
			default_target, default_target_operator, code, return_contract
		) VALUES (
			$1, NULLIF($2, ''), $3, $4::int, $5::text[],
			$6, $7, $8, $9, $10,
			$11, NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, '')
		)
		RETURNING %s`, s, dimensionColumns)

	created, err := scanDimension(r.pool.QueryRow(ctx, q,
		dimension.Name,
		dimension.Description,
		dimension.Tier,
		dimension.ApplicationID,
		dimension.AllowedEngines,
		dimension.ScaleType,
		dimension.ScaleMin,
		dimension.ScaleMax,
		dimension.Polarity,
		dimension.DefaultWeight,
		dimension.DefaultTarget,
		dimension.DefaultTargetOperator,
		dimension.Code,
		dimension.ReturnContract,
	))
	if err != nil {
		return evaluation.Dimension{}, wrapDimensionWrite("create", err)
	}
	return created, nil
}

// Update rewrites the authored fields of one dimension, and its tier when the
// caller asks for a PROMOTION.
//
// The stored row is read and locked FIRST, in the same transaction as the
// write, because the tier rule compares against the stored tier:
// evaluation.ResolveTierUpdate allows `agent_adhoc` -> `project` (and clears
// `application_id`), refuses `project` -> `agent_adhoc` with a 409, and keeps
// the stored tier when the body carried none. A check outside the transaction
// would race a concurrent promotion.
//
// A `platform` row is refused outright: those are materialised from a registry
// this release does not serve, and the UI renders them read-only.
func (r *EvalDimensionsRepo) Update(
	ctx context.Context,
	projectID, dimensionID string,
	dimension evaluation.Dimension,
) (evaluation.Dimension, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Dimension{}, err
	}
	id, err := strconv.Atoi(dimensionID)
	if err != nil {
		return evaluation.Dimension{}, apierr.BadRequest("dimension id must be an integer")
	}

	var updated evaluation.Dimension
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var storedTier string
		if err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT tier FROM %s.eval_dimensions WHERE id = $1 AND tier <> $2 FOR UPDATE`, s),
			id, evaluation.TierPlatform).Scan(&storedTier); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.NotFound("dimension not found")
			}
			return fmt.Errorf("eval dimensions: update: read tier: %w", err)
		}
		tier, err := evaluation.ResolveTierUpdate(storedTier, dimension.Tier)
		if err != nil {
			return err
		}

		q := fmt.Sprintf(`
			UPDATE %s.eval_dimensions
			SET name = $1,
			    description = NULLIF($2, ''),
			    allowed_engines = $3::text[],
			    scale_type = $4,
			    scale_min = $5,
			    scale_max = $6,
			    polarity = $7,
			    default_weight = $8,
			    default_target = $9,
			    default_target_operator = NULLIF($10, ''),
			    code = NULLIF($11, ''),
			    return_contract = NULLIF($12, ''),
			    tier = $14::text,
			    application_id = CASE WHEN $14::text = $15::text THEN NULL ELSE application_id END,
			    updated_at = now()
			WHERE id = $13
			RETURNING %s`, s, dimensionColumns)

		row, scanErr := scanDimension(tx.QueryRow(ctx, q,
			dimension.Name,
			dimension.Description,
			dimension.AllowedEngines,
			dimension.ScaleType,
			dimension.ScaleMin,
			dimension.ScaleMax,
			dimension.Polarity,
			dimension.DefaultWeight,
			dimension.DefaultTarget,
			dimension.DefaultTargetOperator,
			dimension.Code,
			dimension.ReturnContract,
			id,
			tier,
			evaluation.TierProject,
		))
		if scanErr != nil {
			return wrapDimensionWrite("update", scanErr)
		}
		updated = row
		return nil
	})
	if err != nil {
		return evaluation.Dimension{}, err
	}
	return updated, nil
}

// Delete removes one dimension, and reports a miss as a 404 rather than as a
// silent success.
//
// A DELETE that answers 204 for an id that was never there is how a client
// learns to trust a status code that means nothing. RowsAffected is the only
// thing that distinguishes the two.
func (r *EvalDimensionsRepo) Delete(ctx context.Context, projectID, dimensionID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := strconv.Atoi(dimensionID)
	if err != nil {
		return apierr.BadRequest("dimension id must be an integer")
	}

	tag, err := r.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.eval_dimensions WHERE id = $1 AND tier <> $2`, s),
		id, evaluation.TierPlatform)
	if err != nil {
		return fmt.Errorf("eval dimensions: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("dimension not found")
	}
	return nil
}

// wrapDimensionWrite turns the table's own CHECK refusals into a 400.
//
// tenant/0130 repeats every rule the handler validates, so a write that
// reaches PostgreSQL and is refused by a constraint is a caller error the
// handler failed to catch — a 500 would blame the server for the caller's
// body, and would hide the fact that the two layers disagree. 23514 is
// check_violation; 23503 is a foreign-key violation, which here means the
// named agent does not exist in this project.
func wrapDimensionWrite(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514":
			return apierr.BadRequest("dimension rejected by a stored constraint: " + pgErr.ConstraintName)
		case "23503":
			return apierr.BadRequest("the agent this dimension is scoped to does not exist in this project")
		}
	}
	return fmt.Errorf("eval dimensions: %s: %w", op, err)
}
