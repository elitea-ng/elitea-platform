package repos

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// EvalDatasetsRepo stores Agent Evaluation datasets and their cases in the
// project's own tenant schema (tenant/0132_eval_datasets_runs.sql).
type EvalDatasetsRepo struct {
	pool *pgxpool.Pool
}

func NewEvalDatasetsRepo(pool *pgxpool.Pool) *EvalDatasetsRepo {
	return &EvalDatasetsRepo{pool: pool}
}

// datasetColumns is the ONE projection every dataset read uses, and it carries
// `case_count` as a correlated subquery rather than as a stored counter.
//
// A counter column would need every case write to maintain it, and the day one
// path forgets is the day the list badge says 3 and the detail shows 4 — with
// a 200 at every step. Counting is cheap here because MaxCasesPerDataset caps
// the table at ten rows per dataset.
const datasetColumns = `
	d.id::text,
	COALESCE(d.uuid::text, ''),
	d.name,
	COALESCE(d.description, ''),
	d.application_id,
	d.is_shared,
	(SELECT count(*) FROM %[1]s.eval_dataset_cases AS c WHERE c.dataset_id = d.id)::int,
	d.created_at,
	d.updated_at`

func scanDataset(row pgx.Row) (evaluation.Dataset, error) {
	var dataset evaluation.Dataset
	var createdAt, updatedAt time.Time
	err := row.Scan(
		&dataset.ID,
		&dataset.UUID,
		&dataset.Name,
		&dataset.Description,
		&dataset.ApplicationID,
		&dataset.IsShared,
		&dataset.CaseCount,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return evaluation.Dataset{}, err
	}
	dataset.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	dataset.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return dataset, nil
}

const caseColumns = `
	id::text,
	dataset_id::text,
	input,
	variables,
	expected_output,
	source_type,
	order_index,
	created_at,
	updated_at`

func scanCase(row pgx.Row) (evaluation.DatasetCase, error) {
	var datasetCase evaluation.DatasetCase
	var variables []byte
	var createdAt, updatedAt time.Time
	err := row.Scan(
		&datasetCase.ID,
		&datasetCase.DatasetID,
		&datasetCase.Input,
		&variables,
		&datasetCase.ExpectedOutput,
		&datasetCase.SourceType,
		&datasetCase.OrderIndex,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	// NEVER nil on the wire. The reference spreads this object, and a client
	// that receives `null` where it expects `{}` crashes rather than rendering
	// an empty map.
	datasetCase.Variables = map[string]any{}
	if len(variables) > 0 {
		if err := json.Unmarshal(variables, &datasetCase.Variables); err != nil {
			return evaluation.DatasetCase{}, fmt.Errorf("eval datasets: case variables are not an object: %w", err)
		}
	}
	datasetCase.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	datasetCase.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return datasetCase, nil
}

// ListDatasets returns this project's datasets, optionally widened with ONE
// agent's own.
//
// The predicate is "project-wide datasets, plus this agent's" and NOT
// "everything in the schema": a dataset authored on another agent is that
// agent's, and listing it here would put every agent's evaluation cases in
// every other agent's editor — the same reasoning the dimension list applies to
// `agent_adhoc` rows.
//
// An error is RETURNED and not swallowed into an empty slice. Several listings
// in this package answer `[]` on a query failure, and that is why a missing
// table renders as "you have no datasets" instead of as a fault.
func (r *EvalDatasetsRepo) ListDatasets(
	ctx context.Context,
	projectID string,
	filter evaluation.DatasetListFilter,
) ([]evaluation.Dataset, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf(`
		SELECT `+datasetColumns+`
		FROM %[1]s.eval_datasets AS d
		WHERE d.application_id IS NULL
		   OR ($1::int IS NOT NULL AND d.application_id = $1::int)
		ORDER BY d.name ASC, d.id ASC`, schema)

	rows, err := r.pool.Query(ctx, query, filter.ApplicationID)
	if err != nil {
		return nil, fmt.Errorf("eval datasets: list: %w", err)
	}
	defer rows.Close()

	datasets := []evaluation.Dataset{}
	for rows.Next() {
		dataset, scanErr := scanDataset(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval datasets: list scan: %w", scanErr)
		}
		datasets = append(datasets, dataset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval datasets: list: %w", err)
	}
	return datasets, nil
}

// GetDataset reads one dataset with a page of its cases.
func (r *EvalDatasetsRepo) GetDataset(
	ctx context.Context,
	projectID, datasetID string,
	page evaluation.CasePage,
) (evaluation.DatasetDetail, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.DatasetDetail{}, err
	}
	id, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return evaluation.DatasetDetail{}, err
	}

	dataset, err := scanDataset(r.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT `+datasetColumns+`
		FROM %[1]s.eval_datasets AS d
		WHERE d.id = $1`, schema), id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.DatasetDetail{}, apierr.NotFound("dataset not found")
		}
		return evaluation.DatasetDetail{}, fmt.Errorf("eval datasets: get: %w", err)
	}

	cases, err := r.casesPage(ctx, schema, id, page)
	if err != nil {
		return evaluation.DatasetDetail{}, err
	}
	return evaluation.DatasetDetail{
		Dataset: dataset,
		Cases:   cases,
		// `cases_truncated` is computed from the STORED count and the window,
		// not from `len(cases) == limit`. The two differ on the exact boundary
		// — a dataset with exactly `limit` cases is NOT truncated — and the
		// reference's pager trusts this flag to decide whether to ask for more.
		CasesTruncated: page.Offset+len(cases) < dataset.CaseCount,
	}, nil
}

func (r *EvalDatasetsRepo) casesPage(
	ctx context.Context,
	schema string,
	datasetID int,
	page evaluation.CasePage,
) ([]evaluation.DatasetCase, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = evaluation.DefaultCasePageLimit
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT `+caseColumns+`
		FROM %s.eval_dataset_cases
		WHERE dataset_id = $1
		ORDER BY order_index ASC, id ASC
		LIMIT $2 OFFSET $3`, schema), datasetID, limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("eval datasets: cases: %w", err)
	}
	defer rows.Close()

	cases := []evaluation.DatasetCase{}
	for rows.Next() {
		datasetCase, scanErr := scanCase(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval datasets: case scan: %w", scanErr)
		}
		cases = append(cases, datasetCase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval datasets: cases: %w", err)
	}
	return cases, nil
}

func (r *EvalDatasetsRepo) CreateDataset(
	ctx context.Context,
	projectID string,
	input evaluation.DatasetWriteInput,
) (evaluation.Dataset, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Dataset{}, err
	}

	// RETURNING the full projection and not the id. The row that comes back is
	// the row that was stored, including every server-applied default, so the
	// client's cache after a create is what a reload would show. A create that
	// echoed the request body would report success for whatever the database
	// silently altered.
	created, err := scanDataset(r.pool.QueryRow(ctx, fmt.Sprintf(`
		WITH inserted AS (
			INSERT INTO %[1]s.eval_datasets (name, description, application_id, is_shared)
			VALUES ($1, NULLIF($2, ''), $3::int, $4)
			RETURNING *
		)
		SELECT `+datasetColumns+` FROM inserted AS d`, schema),
		input.Name, input.Description, input.ApplicationID, input.IsShared))
	if err != nil {
		return evaluation.Dataset{}, wrapEvalWrite("dataset create", err)
	}
	return created, nil
}

func (r *EvalDatasetsRepo) UpdateDataset(
	ctx context.Context,
	projectID, datasetID string,
	input evaluation.DatasetWriteInput,
) (evaluation.Dataset, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Dataset{}, err
	}
	id, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return evaluation.Dataset{}, err
	}

	// `application_id` is NOT in the SET list. Scope is set once, at authoring:
	// moving a dataset between agents after runs have been scored against it
	// would re-file that history under an agent it never ran on.
	updated, err := scanDataset(r.pool.QueryRow(ctx, fmt.Sprintf(`
		WITH changed AS (
			UPDATE %[1]s.eval_datasets
			SET name = $1,
			    description = NULLIF($2, ''),
			    is_shared = $3,
			    updated_at = now()
			WHERE id = $4
			RETURNING *
		)
		SELECT `+datasetColumns+` FROM changed AS d`, schema),
		input.Name, input.Description, input.IsShared, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Dataset{}, apierr.NotFound("dataset not found")
		}
		return evaluation.Dataset{}, wrapEvalWrite("dataset update", err)
	}
	return updated, nil
}

// DeleteDataset removes a dataset and its cases, and REFUSES while a run
// references it.
//
// tenant/0132 gives eval_runs.dataset_id an ON DELETE CASCADE, because the
// chain from `applications` has to reach all the way down or an agent with an
// evaluated dataset becomes undeletable (the pair of defects tenant/0131 had to
// repair). So the database would happily destroy a finished run's scores here.
// The refusal is what stops a casual tidy-up from deleting evidence, and it
// names the count so the caller knows what they are being protected from.
func (r *EvalDatasetsRepo) DeleteDataset(ctx context.Context, projectID, datasetID string) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return err
	}

	var runCount int
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*)::int FROM %s.eval_runs WHERE dataset_id = $1`, schema), id).
		Scan(&runCount); err != nil {
		return fmt.Errorf("eval datasets: delete precheck: %w", err)
	}
	if runCount > 0 {
		return apierr.Conflict(fmt.Sprintf(
			"this dataset has %d run(s) scored against it: deleting it would destroy their results", runCount))
	}

	tag, err := r.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.eval_datasets WHERE id = $1`, schema), id)
	if err != nil {
		return fmt.Errorf("eval datasets: delete: %w", err)
	}
	// RowsAffected is the only thing that distinguishes "deleted" from "was
	// never there". A 204 for an id that never existed teaches a client to
	// trust a status code that means nothing.
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("dataset not found")
	}
	return nil
}

// AddCase appends one case, refusing past the cap.
//
// The count and the insert are ONE STATEMENT. Checking first and inserting
// second is a race two concurrent writers win together, and the cap exists to
// bound model spend — an over-count is not cosmetic. The `WHERE (SELECT
// count(*) ...) < cap` predicate makes PostgreSQL evaluate both in the same
// snapshot, and a zero-row insert is the refusal.
func (r *EvalDatasetsRepo) AddCase(
	ctx context.Context,
	projectID, datasetID string,
	input evaluation.CaseWriteInput,
) (evaluation.DatasetCase, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	id, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	variables, err := json.Marshal(input.Variables)
	if err != nil {
		return evaluation.DatasetCase{}, apierr.BadRequest("variables must be a JSON object")
	}

	// The dataset is confirmed to exist FIRST, so "no such dataset" answers 404
	// and "the dataset is full" answers 409 — a single zero-row insert cannot
	// tell the two apart, and answering the same code for both sends the caller
	// looking in the wrong place.
	var exists bool
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS (SELECT 1 FROM %s.eval_datasets WHERE id = $1)`, schema), id).
		Scan(&exists); err != nil {
		return evaluation.DatasetCase{}, fmt.Errorf("eval datasets: add case: %w", err)
	}
	if !exists {
		return evaluation.DatasetCase{}, apierr.NotFound("dataset not found")
	}

	created, err := scanCase(r.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s.eval_dataset_cases
			(dataset_id, input, variables, expected_output, source_type, order_index)
		SELECT $1, $2, $3::jsonb, $4, $5,
		       COALESCE((SELECT max(order_index) + 1 FROM %[1]s.eval_dataset_cases WHERE dataset_id = $1), 0)
		WHERE (SELECT count(*) FROM %[1]s.eval_dataset_cases WHERE dataset_id = $1) < $6
		RETURNING `+caseColumns, schema),
		id, input.Input, string(variables), input.ExpectedOutput,
		evaluation.CaseSourceManual, evaluation.MaxCasesPerDataset))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.DatasetCase{}, apierr.Conflict(fmt.Sprintf(
				"a dataset holds at most %d cases: a run spends two model calls per case, so the cap is what stands between a large paste and an unauthorised bill",
				evaluation.MaxCasesPerDataset))
		}
		return evaluation.DatasetCase{}, wrapEvalWrite("case create", err)
	}
	return created, nil
}

func (r *EvalDatasetsRepo) UpdateCase(
	ctx context.Context,
	projectID, datasetID, caseID string,
	input evaluation.CaseWriteInput,
) (evaluation.DatasetCase, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	datasetKey, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	caseKey, err := parseEvalID(caseID, "case")
	if err != nil {
		return evaluation.DatasetCase{}, err
	}
	variables, err := json.Marshal(input.Variables)
	if err != nil {
		return evaluation.DatasetCase{}, apierr.BadRequest("variables must be a JSON object")
	}

	// `dataset_id = $5` is in the predicate and not only in the URL. Without
	// it, a caller who knows any case id can edit it through any dataset's
	// path, and the route's own 404 would never fire.
	updated, err := scanCase(r.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.eval_dataset_cases
		SET input = $1,
		    variables = $2::jsonb,
		    expected_output = $3,
		    updated_at = now()
		WHERE id = $4 AND dataset_id = $5
		RETURNING `+caseColumns, schema),
		input.Input, string(variables), input.ExpectedOutput, caseKey, datasetKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.DatasetCase{}, apierr.NotFound("case not found in this dataset")
		}
		return evaluation.DatasetCase{}, wrapEvalWrite("case update", err)
	}
	return updated, nil
}

func (r *EvalDatasetsRepo) DeleteCase(ctx context.Context, projectID, datasetID, caseID string) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	datasetKey, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return err
	}
	caseKey, err := parseEvalID(caseID, "case")
	if err != nil {
		return err
	}

	tag, err := r.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.eval_dataset_cases WHERE id = $1 AND dataset_id = $2`, schema),
		caseKey, datasetKey)
	if err != nil {
		return fmt.Errorf("eval datasets: delete case: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("case not found in this dataset")
	}
	return nil
}

// parseEvalID turns a path segment into a key, or a 400 naming which segment
// was wrong. `strconv.Atoi` on its own gives an error message with the raw
// value in it and no idea which of two path parameters it came from.
func parseEvalID(raw, what string) (int, error) {
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, apierr.BadRequest(what + " id must be a positive integer")
	}
	return parsed, nil
}

// wrapEvalWrite turns the tables' own CHECK and FK refusals into a 400.
//
// tenant/0132 repeats every rule the handlers validate, so a write refused by a
// constraint is a caller error the handler failed to catch. A 500 there would
// blame the server for the caller's body AND hide the fact that the two layers
// disagree — which is the interesting failure, because a handler that accepts
// what the table refuses is a 500 in production and a table that accepts what
// the handler refuses is a rule with a hole in it for every other writer.
func wrapEvalWrite(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514":
			return apierr.BadRequest("rejected by a stored constraint: " + pgErr.ConstraintName)
		case "23503":
			return apierr.BadRequest("this row references something that does not exist in this project: " + pgErr.ConstraintName)
		case "23505":
			return apierr.Conflict("this row already exists: " + pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("eval: %s: %w", op, err)
}
