package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
)

// HTTP action rule store failures. None of the messages carries a submitted
// value, so the API answers them verbatim.
var (
	ErrHTTPActionRuleNotFound  = errors.New("HTTP action rule not found")
	ErrHTTPActionRuleNameTaken = errors.New("an HTTP action rule with this name already exists in the project")
	ErrHTTPActionRuleStale     = errors.New("the HTTP action rule changed since it was read; reload it and retry")
	ErrHTTPActionRuleLimit     = fmt.Errorf("the project already holds the maximum of %d HTTP action rules", httpaction.MaxRulesPerProject)
	// ErrHTTPActionRuleStoredInvalid: a stored row fails today's Validate (a
	// row written by an older binary). It is never handed out as a rule.
	ErrHTTPActionRuleStoredInvalid = errors.New("stored HTTP action rule is invalid")
)

// HTTPActionRuleRecord is one stored rule with its write timestamps.
type HTTPActionRuleRecord struct {
	httpaction.Rule
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HTTPActionRuleName is the pipeline editor's view of a rule.
type HTTPActionRuleName struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// HTTPActionRulesRepo stores elitea_runtime.http_action_rules and writes one
// http_action_rule_audit row in the transaction of every accepted write.
type HTTPActionRulesRepo struct {
	store sharedStore
}

func NewHTTPActionRulesRepo(pool *pgxpool.Pool) (*HTTPActionRulesRepo, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &HTTPActionRulesRepo{store: store}, nil
}

const httpActionRuleColumns = `id, project_id, revision, name, origin, path_template, method, query_names,
 configuration_id, toolkit, tool, coalesce(output_bucket, ''), replay, enabled, created_at, updated_at`

func scanHTTPActionRule(row sqlRow) (HTTPActionRuleRecord, error) {
	var record HTTPActionRuleRecord
	rule := &record.Rule
	if err := row.Scan(&rule.ID, &rule.ProjectID, &rule.Revision, &rule.Name, &rule.Origin, &rule.PathTemplate,
		&rule.Method, &rule.QueryNames, &rule.ConfigurationID, &rule.Toolkit, &rule.Tool, &rule.OutputBucket,
		&rule.Replay, &rule.Enabled, &record.CreatedAt, &record.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HTTPActionRuleRecord{}, ErrHTTPActionRuleNotFound
		}
		return HTTPActionRuleRecord{}, err
	}
	rule.SchemaVersion = httpaction.RuleSchema
	if rule.QueryNames == nil {
		rule.QueryNames = []string{}
	}
	if err := rule.Validate(); err != nil {
		return HTTPActionRuleRecord{}, fmt.Errorf("%w: rule %d: %v", ErrHTTPActionRuleStoredInvalid, rule.ID, err)
	}
	return record, nil
}

// readHTTPActionRule reads one rule of the project by name, FOR SHARE when
// lock is set (an effect Begin pins the revision against a concurrent edit).
// A rule of another project is ErrHTTPActionRuleNotFound.
func readHTTPActionRule(ctx context.Context, q sqlExecutor, projectID int64, name string, lock bool) (httpaction.Rule, error) {
	query := `SELECT ` + httpActionRuleColumns + ` FROM elitea_runtime.http_action_rules WHERE project_id = $1 AND name = $2`
	if lock {
		query += ` FOR SHARE`
	}
	record, err := scanHTTPActionRule(q.QueryRow(ctx, query, projectID, name))
	if err != nil {
		return httpaction.Rule{}, err
	}
	return record.Rule, nil
}

// List returns the project's rules ordered by name.
func (r *HTTPActionRulesRepo) List(ctx context.Context, projectID int64) ([]HTTPActionRuleRecord, error) {
	rows, err := r.store.Query(ctx, `SELECT `+httpActionRuleColumns+` FROM elitea_runtime.http_action_rules
WHERE project_id = $1 ORDER BY name LIMIT $2`, projectID, httpaction.MaxRulesPerProject)
	if err != nil {
		return nil, fmt.Errorf("http action rules: list: %w", err)
	}
	defer rows.Close()
	items := []HTTPActionRuleRecord{}
	for rows.Next() {
		record, err := scanHTTPActionRule(rows)
		if err != nil {
			return nil, fmt.Errorf("http action rules: list: %w", err)
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("http action rules: list: %w", err)
	}
	return items, nil
}

// Get returns one rule of the project.
func (r *HTTPActionRulesRepo) Get(ctx context.Context, projectID, ruleID int64) (HTTPActionRuleRecord, error) {
	record, err := scanHTTPActionRule(r.store.QueryRow(ctx, `SELECT `+httpActionRuleColumns+`
FROM elitea_runtime.http_action_rules WHERE project_id = $1 AND id = $2`, projectID, ruleID))
	if err != nil {
		return HTTPActionRuleRecord{}, fmt.Errorf("http action rules: get: %w", err)
	}
	return record, nil
}

// ListNames returns the project's rule names and enabled flags, by name.
func (r *HTTPActionRulesRepo) ListNames(ctx context.Context, projectID int64) ([]HTTPActionRuleName, error) {
	rows, err := r.store.Query(ctx, `SELECT name, enabled FROM elitea_runtime.http_action_rules
WHERE project_id = $1 ORDER BY name LIMIT $2`, projectID, httpaction.MaxRulesPerProject)
	if err != nil {
		return nil, fmt.Errorf("http action rules: list names: %w", err)
	}
	defer rows.Close()
	items := []HTTPActionRuleName{}
	for rows.Next() {
		var item HTTPActionRuleName
		if err := rows.Scan(&item.Name, &item.Enabled); err != nil {
			return nil, fmt.Errorf("http action rules: list names: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("http action rules: list names: %w", err)
	}
	return items, nil
}

// Create stores a new rule at revision 1. Creates of one project are
// serialised by a transaction-scoped advisory lock, so the count check holds
// under any concurrency: a project never exceeds MaxRulesPerProject.
func (r *HTTPActionRulesRepo) Create(ctx context.Context, projectID, actorID int64, rule httpaction.Rule) (HTTPActionRuleRecord, error) {
	if err := checkHTTPActionRuleWrite(projectID, actorID, &rule); err != nil {
		return HTTPActionRuleRecord{}, err
	}
	var created HTTPActionRuleRecord
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('elitea.http_action_rules/' || $1::bigint::text, 0))`, projectID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.http_action_rules WHERE project_id = $1`, projectID).Scan(&count); err != nil {
			return err
		}
		if count >= httpaction.MaxRulesPerProject {
			return ErrHTTPActionRuleLimit
		}
		record, err := scanHTTPActionRule(tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.http_action_rules
 (project_id, name, origin, path_template, method, query_names, configuration_id, toolkit, tool, output_bucket,
  replay, enabled, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, $13, $13)
RETURNING `+httpActionRuleColumns,
			projectID, rule.Name, rule.Origin, rule.PathTemplate, rule.Method, rule.QueryNames, rule.ConfigurationID,
			rule.Toolkit, rule.Tool, rule.OutputBucket, rule.Replay, rule.Enabled, actorID))
		if err != nil {
			return err
		}
		created = record
		return insertHTTPActionRuleAudit(ctx, tx, actorID, "create", record.Rule)
	})
	if err != nil {
		return HTTPActionRuleRecord{}, httpActionRuleWriteError("create", err)
	}
	return created, nil
}

// Update replaces every field of the rule when its stored revision equals
// expectedRevision, and moves the revision on by one.
func (r *HTTPActionRulesRepo) Update(ctx context.Context, projectID, ruleID, actorID int64, rule httpaction.Rule, expectedRevision int64) (HTTPActionRuleRecord, error) {
	if err := checkHTTPActionRuleWrite(projectID, actorID, &rule); err != nil {
		return HTTPActionRuleRecord{}, err
	}
	var updated HTTPActionRuleRecord
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		record, err := scanHTTPActionRule(tx.QueryRow(ctx, `
UPDATE elitea_runtime.http_action_rules SET
 name = $3, origin = $4, path_template = $5, method = $6, query_names = $7, configuration_id = $8, toolkit = $9,
 tool = $10, output_bucket = NULLIF($11, ''), replay = $12, enabled = $13,
 revision = revision + 1, updated_by = $14, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND revision = $15
RETURNING `+httpActionRuleColumns,
			projectID, ruleID, rule.Name, rule.Origin, rule.PathTemplate, rule.Method, rule.QueryNames,
			rule.ConfigurationID, rule.Toolkit, rule.Tool, rule.OutputBucket, rule.Replay, rule.Enabled, actorID,
			expectedRevision))
		if errors.Is(err, ErrHTTPActionRuleNotFound) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.http_action_rules WHERE project_id = $1 AND id = $2)`,
				projectID, ruleID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrHTTPActionRuleStale
			}
			return ErrHTTPActionRuleNotFound
		}
		if err != nil {
			return err
		}
		updated = record
		return insertHTTPActionRuleAudit(ctx, tx, actorID, "update", record.Rule)
	})
	if err != nil {
		return HTTPActionRuleRecord{}, httpActionRuleWriteError("update", err)
	}
	return updated, nil
}

func checkHTTPActionRuleWrite(projectID, actorID int64, rule *httpaction.Rule) error {
	if projectID <= 0 || actorID <= 0 {
		return errors.New("http action rules: project and actor ids must be positive")
	}
	rule.Normalize()
	return rule.Validate()
}

func insertHTTPActionRuleAudit(ctx context.Context, tx sqlExecutor, actorID int64, action string, rule httpaction.Rule) error {
	body, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO elitea_runtime.http_action_rule_audit (rule_id, revision, project_id, actor_id, action, rule_json)
VALUES ($1, $2, $3, $4, $5, $6)`, rule.ID, rule.Revision, rule.ProjectID, actorID, action, body)
	return err
}

func httpActionRuleWriteError(operation string, err error) error {
	var postgres *pgconn.PgError
	switch {
	case errors.As(err, &postgres) && postgres.Code == "23505":
		return ErrHTTPActionRuleNameTaken
	case errors.Is(err, ErrHTTPActionRuleLimit), errors.Is(err, ErrHTTPActionRuleStale), errors.Is(err, ErrHTTPActionRuleNotFound):
		return err
	}
	return fmt.Errorf("http action rules: %s: %w", operation, err)
}
