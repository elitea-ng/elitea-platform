package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
)

// DB action rule store failures. None of the messages carries a submitted
// value, host or credential, so the API answers them verbatim.
var (
	ErrDBActionRuleNotFound  = errors.New("DB action rule not found")
	ErrDBActionRuleNameTaken = errors.New("a DB action rule with this name already exists in the project")
	ErrDBActionRuleStale     = errors.New("the DB action rule changed since it was read; reload it and retry")
	ErrDBActionRuleLimit     = fmt.Errorf("the project already holds the maximum of %d DB action rules", dbaction.MaxRulesPerProject)
	// ErrDBActionRuleConfigurationInvalid: configuration_id does not name an
	// accessible SQL configuration in this project matching the rule dialect.
	// Neutral wording avoids disclosing foreign or other-user private configs.
	ErrDBActionRuleConfigurationInvalid = errors.New("DB action rule configuration_id must reference an accessible SQL configuration in this project")
	// ErrDBActionRuleStoredInvalid: a stored row fails today's Validate (a
	// row written by an older binary). It is never handed out as a rule.
	ErrDBActionRuleStoredInvalid = errors.New("stored DB action rule is invalid")
)

const maxDBConfigurationData = 256 << 10

// DBActionRuleRecord is one stored DB action rule with its write timestamps.
type DBActionRuleRecord struct {
	dbaction.Rule
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DBActionRuleName is the pipeline editor's redacted view of a rule: never
// exposes configuration_id, database_name, marker_table, host, or credentials.
type DBActionRuleName struct {
	Name    string `json:"name"`
	Dialect string `json:"dialect"`
	Mode    string `json:"mode"`
	Enabled bool   `json:"enabled"`
}

// DBActionConfigurationTarget is the resolved TCP destination of a referenced
// `sql` configuration, used for write-time egress allowlist validation.
type DBActionConfigurationTarget struct {
	Host string
	Port uint16
}

// DBActionRulesRepo stores elitea_runtime.db_action_rules and writes one
// db_action_rule_audit row in the transaction of every accepted write.
type DBActionRulesRepo struct {
	store sharedStore
}

func NewDBActionRulesRepo(pool *pgxpool.Pool) (*DBActionRulesRepo, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &DBActionRulesRepo{store: store}, nil
}

const dbActionRuleColumns = `id, project_id, revision, name, configuration_id, dialect, database_name, mode,
 coalesce(marker_table, ''), enabled, updated_by, created_at, updated_at`

func scanDBActionRuleRaw(row sqlRow) (DBActionRuleRecord, error) {
	var record DBActionRuleRecord
	rule := &record.Rule
	if err := row.Scan(&rule.ID, &rule.ProjectID, &rule.Revision, &rule.Name, &rule.ConfigurationID,
		&rule.Dialect, &rule.DatabaseName, &rule.Mode, &rule.MarkerTable, &rule.Enabled,
		&rule.UpdatedBy, &record.CreatedAt, &record.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DBActionRuleRecord{}, ErrDBActionRuleNotFound
		}
		return DBActionRuleRecord{}, err
	}
	rule.SchemaVersion = dbaction.RuleSchema
	return record, nil
}

func scanDBActionRule(row sqlRow) (DBActionRuleRecord, error) {
	record, err := scanDBActionRuleRaw(row)
	if err != nil {
		return DBActionRuleRecord{}, err
	}
	if err := record.Validate(); err != nil {
		return DBActionRuleRecord{}, fmt.Errorf("%w: rule %d: %v", ErrDBActionRuleStoredInvalid, record.ID, err)
	}
	return record, nil
}

// readDBActionRule reads one rule of the project by name, FOR SHARE when lock
// is set. A rule of another project is ErrDBActionRuleNotFound.
func readDBActionRule(ctx context.Context, q sqlExecutor, projectID int64, name string, lock bool) (dbaction.Rule, error) {
	query := `SELECT ` + dbActionRuleColumns + ` FROM elitea_runtime.db_action_rules WHERE project_id = $1 AND name = $2`
	if lock {
		query += ` FOR SHARE`
	}
	record, err := scanDBActionRule(q.QueryRow(ctx, query, projectID, name))
	if err != nil {
		return dbaction.Rule{}, err
	}
	return record.Rule, nil
}

// List returns the project's valid DB action rules ordered by name. A stored
// row failing today's validator is logged and skipped so one legacy/corrupt row
// cannot break listing sibling rules; Update can still replace or disable it.
func (r *DBActionRulesRepo) List(ctx context.Context, projectID int64) ([]DBActionRuleRecord, error) {
	rows, err := r.store.Query(ctx, `SELECT `+dbActionRuleColumns+` FROM elitea_runtime.db_action_rules
WHERE project_id = $1 ORDER BY name LIMIT $2`, projectID, dbaction.MaxRulesPerProject)
	if err != nil {
		return nil, fmt.Errorf("db action rules: list: %w", err)
	}
	defer rows.Close()
	items := []DBActionRuleRecord{}
	for rows.Next() {
		record, err := scanDBActionRuleRaw(rows)
		if err != nil {
			return nil, fmt.Errorf("db action rules: list: %w", err)
		}
		if err := record.Validate(); err != nil {
			slog.WarnContext(ctx, "db action rules: skipping invalid stored rule", "project_id", projectID, "rule_id", record.ID)
			continue
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db action rules: list: %w", err)
	}
	return items, nil
}

// Get returns one DB action rule of the project.
func (r *DBActionRulesRepo) Get(ctx context.Context, projectID, ruleID int64) (DBActionRuleRecord, error) {
	record, err := scanDBActionRule(r.store.QueryRow(ctx, `SELECT `+dbActionRuleColumns+`
FROM elitea_runtime.db_action_rules WHERE project_id = $1 AND id = $2`, projectID, ruleID))
	if err != nil {
		return DBActionRuleRecord{}, fmt.Errorf("db action rules: get: %w", err)
	}
	return record, nil
}

// ListNames returns the project's rule names, dialects, modes, and enabled
// flags, ordered by name. Never returns host, database_name, marker_table, or
// configuration_id.
func (r *DBActionRulesRepo) ListNames(ctx context.Context, projectID int64) ([]DBActionRuleName, error) {
	rows, err := r.store.Query(ctx, `SELECT name, dialect, mode, enabled FROM elitea_runtime.db_action_rules
WHERE project_id = $1 ORDER BY name LIMIT $2`, projectID, dbaction.MaxRulesPerProject)
	if err != nil {
		return nil, fmt.Errorf("db action rules: list names: %w", err)
	}
	defer rows.Close()
	items := []DBActionRuleName{}
	for rows.Next() {
		var item DBActionRuleName
		if err := rows.Scan(&item.Name, &item.Dialect, &item.Mode, &item.Enabled); err != nil {
			return nil, fmt.Errorf("db action rules: list names: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db action rules: list names: %w", err)
	}
	return items, nil
}

// LookupConfigurationTarget resolves the host and port of an accessible project
// `sql` configuration for write-time egress allowlist validation (outside the
// write transaction so DNS resolution never holds Postgres locks).
func (r *DBActionRulesRepo) LookupConfigurationTarget(
	ctx context.Context,
	projectID, actorID int64,
	configurationID int32,
	dialect string,
) (DBActionConfigurationTarget, error) {
	var target DBActionConfigurationTarget
	err := r.store.WithinTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		var err error
		target, err = checkDBActionRuleConfiguration(ctx, tx, projectID, actorID, configurationID, dialect, false)
		return err
	})
	if err != nil {
		return DBActionConfigurationTarget{}, dbActionRuleWriteError("lookup configuration", err)
	}
	return target, nil
}

// Create stores a new DB action rule at revision 1. Creates of one project are
// serialised by a transaction-scoped advisory lock, so the 256-rule cap holds
// under any concurrency.
func (r *DBActionRulesRepo) Create(ctx context.Context, projectID, actorID int64, rule dbaction.Rule) (DBActionRuleRecord, error) {
	if err := checkDBActionRuleWrite(projectID, actorID, &rule); err != nil {
		return DBActionRuleRecord{}, err
	}
	var created DBActionRuleRecord
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		if _, err := checkDBActionRuleConfiguration(ctx, tx, projectID, actorID, rule.ConfigurationID, rule.Dialect, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('elitea.db_action_rules/' || $1::bigint::text, 0))`, projectID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.db_action_rules WHERE project_id = $1`, projectID).Scan(&count); err != nil {
			return err
		}
		if count >= dbaction.MaxRulesPerProject {
			return ErrDBActionRuleLimit
		}
		record, err := scanDBActionRule(tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.db_action_rules
 (project_id, name, configuration_id, dialect, database_name, mode, marker_table, enabled, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $9)
RETURNING `+dbActionRuleColumns,
			projectID, rule.Name, rule.ConfigurationID, rule.Dialect, rule.DatabaseName,
			rule.Mode, rule.MarkerTable, rule.Enabled, actorID))
		if err != nil {
			return err
		}
		created = record
		return insertDBActionRuleAudit(ctx, tx, actorID, "create", record.Rule)
	})
	if err != nil {
		return DBActionRuleRecord{}, dbActionRuleWriteError("create", err)
	}
	return created, nil
}

// Update replaces every field of the rule when its stored revision equals
// expectedRevision, incrementing revision by one and recording an audit row.
func (r *DBActionRulesRepo) Update(ctx context.Context, projectID, ruleID, actorID int64, rule dbaction.Rule, expectedRevision int64) (DBActionRuleRecord, error) {
	if err := checkDBActionRuleWrite(projectID, actorID, &rule); err != nil {
		return DBActionRuleRecord{}, err
	}
	var updated DBActionRuleRecord
	err := r.store.WithinTx(ctx, pgx.TxOptions{}, func(tx sqlExecutor) error {
		if _, err := checkDBActionRuleConfiguration(ctx, tx, projectID, actorID, rule.ConfigurationID, rule.Dialect, true); err != nil {
			return err
		}
		record, err := scanDBActionRule(tx.QueryRow(ctx, `
UPDATE elitea_runtime.db_action_rules SET
 name = $3, configuration_id = $4, dialect = $5, database_name = $6, mode = $7,
 marker_table = NULLIF($8, ''), enabled = $9,
 revision = revision + 1, updated_by = $10, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND revision = $11
RETURNING `+dbActionRuleColumns,
			projectID, ruleID, rule.Name, rule.ConfigurationID, rule.Dialect, rule.DatabaseName,
			rule.Mode, rule.MarkerTable, rule.Enabled, actorID, expectedRevision))
		if errors.Is(err, ErrDBActionRuleNotFound) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.db_action_rules WHERE project_id = $1 AND id = $2)`,
				projectID, ruleID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrDBActionRuleStale
			}
			return ErrDBActionRuleNotFound
		}
		if err != nil {
			return err
		}
		updated = record
		return insertDBActionRuleAudit(ctx, tx, actorID, "update", record.Rule)
	})
	if err != nil {
		return DBActionRuleRecord{}, dbActionRuleWriteError("update", err)
	}
	return updated, nil
}

func checkDBActionRuleConfiguration(
	ctx context.Context,
	tx sqlExecutor,
	projectID, actorID int64,
	configurationID int32,
	dialect string,
	lock bool,
) (DBActionConfigurationTarget, error) {
	if projectID <= 0 || projectID > 2147483647 || actorID <= 0 || configurationID <= 0 {
		return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
	}
	table := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10), "configuration"}.Sanitize()
	query := `SELECT type, data::text, shared, author_id FROM ` + table + ` WHERE id = $1 AND project_id = $2`
	if lock {
		query += ` FOR SHARE`
	}
	var (
		kind   string
		data   string
		shared bool
		author *int64
	)
	err := tx.QueryRow(ctx, query, configurationID, int32(projectID)).Scan(&kind, &data, &shared, &author)
	if errors.Is(err, pgx.ErrNoRows) {
		return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "3F000" || pgErr.Code == "42P01") {
			// Project tenant schema or configuration table does not exist.
			return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
		}
		return DBActionConfigurationTarget{}, err
	}
	if kind != "sql" || (!shared && author != nil && *author != actorID) || len(data) > maxDBConfigurationData {
		return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
	}
	parsed, err := decodeSQLConfigurationJSON([]byte(data))
	if err != nil {
		return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
	}
	host, port, err := dbaction.ParseSQLTarget(dialect, parsed)
	if err != nil {
		return DBActionConfigurationTarget{}, ErrDBActionRuleConfigurationInvalid
	}
	return DBActionConfigurationTarget{Host: host, Port: port}, nil
}

func decodeSQLConfigurationJSON(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil || parsed == nil {
		return nil, ErrDBActionRuleConfigurationInvalid
	}
	return parsed, nil
}

func checkDBActionRuleWrite(projectID, actorID int64, rule *dbaction.Rule) error {
	if projectID <= 0 || actorID <= 0 {
		return errors.New("db action rules: project and actor ids must be positive")
	}
	rule.Normalize()
	return rule.Validate()
}

func insertDBActionRuleAudit(ctx context.Context, tx sqlExecutor, actorID int64, action string, rule dbaction.Rule) error {
	body, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO elitea_runtime.db_action_rule_audit (rule_id, revision, project_id, actor_id, action, rule_json)
VALUES ($1, $2, $3, $4, $5, $6)`, rule.ID, rule.Revision, rule.ProjectID, actorID, action, body)
	return err
}

func dbActionRuleWriteError(operation string, err error) error {
	var postgres *pgconn.PgError
	switch {
	case errors.As(err, &postgres) && postgres.Code == "23505":
		return ErrDBActionRuleNameTaken
	case errors.Is(err, ErrDBActionRuleLimit),
		errors.Is(err, ErrDBActionRuleStale),
		errors.Is(err, ErrDBActionRuleNotFound),
		errors.Is(err, ErrDBActionRuleConfigurationInvalid):
		return err
	}
	return fmt.Errorf("db action rules: %s: %w", operation, err)
}
