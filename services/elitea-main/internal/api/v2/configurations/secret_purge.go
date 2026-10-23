package configurations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5"
)

// SecretPurger removes hidden secrets from the project vault inside the
// transaction the caller owns (#1204).
//
// It is optional on the sealer. The vault repository the router composes
// implements both, and a sealer that does not purge keeps the earlier
// behaviour: the row goes and the hidden secret stays.
type SecretPurger interface {
	PurgeProjectHiddenSecrets(ctx context.Context, tx pgx.Tx, projectID int64, names []string) error
}

// purgeOrphanedHiddenSecrets deletes, from the project vault, every hidden
// secret in `candidates` that nothing in the tenant schema still references
// (unreferencedHiddenSecrets says what counts).
//
// WHY. A seal mints a random 32-hex name per value, so a hidden secret
// normally belongs to exactly one row. Deleting that row used to leave the
// secret in the vault, readable through the secrets API by name. Rows can
// still SHARE a name — a client may write back an existing `{{secret.NAME}}`
// reference — so a name is reference-counted against the rows left after the
// write instead of being deleted blindly.
//
// It runs in the caller's transaction AFTER the row write, so the row lock is
// taken before the vault lock: the order the seal path uses. The scan reads
// the caller's own uncommitted write, so the deleted (or rewritten) row no
// longer counts.
func (h *Handler) purgeOrphanedHiddenSecrets(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	projectID int64,
	candidates []string,
) error {
	if len(candidates) == 0 {
		return nil
	}
	purger, ok := h.secretSealer.(SecretPurger)
	if !ok {
		slog.WarnContext(ctx, "configuration hidden secrets were not purged: no vault writer is composed",
			"project_id", projectID, "secrets", len(candidates))
		return nil
	}
	orphaned, err := unreferencedHiddenSecrets(ctx, tx, schema, candidates)
	if err != nil {
		return err
	}
	if len(orphaned) == 0 {
		return nil
	}
	if err := purger.PurgeProjectHiddenSecrets(ctx, tx, projectID, orphaned); err != nil {
		return fmt.Errorf("%w: %w", errConfigurationSecretStoreUnavailable, err)
	}
	return nil
}

// unreferencedHiddenSecrets returns the names in `candidates` that no
// settings-bearing json/jsonb column of `schema` mentions.
//
// WHICH COLUMNS. A sealed reference is copied well beyond the configuration
// and toolkit rows: agent version snapshots, a conversation's participant
// settings (and a fork's copy of them), evaluation run snapshots, legacy-era
// tables this service does not know the shape of. That set is open-ended, so
// the scan is too: every json and jsonb column of every table of the tenant
// schema, read from pg_catalog at purge time, so a table added later is
// covered without touching this code. Only the transcript tables are left
// out (settingsFreeTables): they record what a turn did and are never read
// back as settings, and they are the tenant's largest tables by far.
//
// The match is a substring of the column's JSON text, which over-counts on
// purpose: a reference embedded in a longer string ("Bearer {{secret.X}}")
// keeps the secret. Names are 32-hex, so they carry no LIKE or JSON escape.
//
// A catalog read that does not even list configuration.data is not a scan
// this function can trust, so it purges nothing.
func unreferencedHiddenSecrets(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	candidates []string,
) ([]string, error) {
	// schema is the quoted identifier (tenantSchema); to_regnamespace reads
	// it as such. A partition is scanned through its parent, never twice.
	rows, err := tx.Query(ctx, `
		SELECT c.relname::text, a.attname::text
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE c.relnamespace = to_regnamespace($1)
		  AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.atttypid IN ('json'::regtype, 'jsonb'::regtype)
		  AND NOT (c.relname = ANY($2::text[]))
		  AND c.relname NOT LIKE 'chat\_message\_%'
		  AND c.relname NOT LIKE 'chat\_messages\_%'
		ORDER BY 1, 2`, schema, settingsFreeTables)
	if err != nil {
		return nil, err
	}
	type column struct{ table, name string }
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
		var c column
		err := row.Scan(&c.table, &c.name)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	var (
		clauses       strings.Builder
		matches       []string
		table         string
		sawConfigData bool
	)
	flush := func() {
		if table == "" {
			return
		}
		fmt.Fprintf(&clauses, `
			AND NOT EXISTS (SELECT 1 FROM %s x WHERE %s)`,
			schema+"."+pgx.Identifier{table}.Sanitize(), strings.Join(matches, " OR "))
		matches = matches[:0]
	}
	for _, c := range columns {
		if c.table != table {
			flush()
			table = c.table
		}
		if c.table == "configuration" && c.name == "data" {
			sawConfigData = true
		}
		matches = append(matches, fmt.Sprintf(
			`strpos(x.%s::text, '{{secret.' || n || '}}') > 0`, pgx.Identifier{c.name}.Sanitize()))
	}
	flush()
	if !sawConfigData {
		return nil, nil
	}

	query := `SELECT n FROM unnest($1::text[]) AS n WHERE true` + clauses.String() + `
		ORDER BY n`
	found, err := tx.Query(ctx, query, candidates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(found, pgx.RowTo[string])
}

// settingsFreeTables are the tenant tables, besides the chat_message_* and
// chat_messages_* transcript families, whose json columns hold run records
// rather than settings (unreferencedHiddenSecrets).
var settingsFreeTables = []string{
	"chat_canvas_versions",
	"configuration_validation_projection",
	"eval_results",
	"pipeline_trigger_deliveries",
}

// hiddenSecretReferencesIn returns the hidden-secret names one stored `data`
// column references. A column that does not decode references nothing this
// handler can name, so it purges nothing.
func hiddenSecretReferencesIn(encoded []byte) []string {
	if len(encoded) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil
	}
	return configurationapp.CurrentHiddenSecretReferences(decoded)
}

// storedHiddenSecretReferences returns the hidden-secret names the stored row
// references, read under the row lock. A body that writes no `data` replaces
// no reference, so it reads nothing.
func (h *Handler) storedHiddenSecretReferences(
	ctx context.Context,
	tx pgx.Tx,
	body map[string]any,
	schema string,
	configID string,
) ([]string, error) {
	if _, present := body["data"]; !present {
		return nil, nil
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(data, '{}'::jsonb) FROM %s.configuration WHERE %s = $1 FOR UPDATE`,
		schema, configurationIDColumn(configID))
	var data []byte
	if err := tx.QueryRow(ctx, query, configID).Scan(&data); err != nil {
		return nil, err
	}
	return hiddenSecretReferencesIn(data), nil
}
