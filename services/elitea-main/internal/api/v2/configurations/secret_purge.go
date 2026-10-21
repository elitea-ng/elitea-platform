package configurations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
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
// secret in `candidates` that no configuration or toolkit row of the tenant
// schema still references.
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
// configuration row and no toolkit row of `schema` mentions.
//
// The match is a substring of the row's JSON text, which over-counts on
// purpose: a reference embedded in a longer string ("Bearer {{secret.X}}")
// keeps the secret. Names are 32-hex, so they carry no LIKE or JSON escape.
//
// elitea_tools is pylon-owned and absent from a database that never ran
// pylon's schema, so it is consulted only when it exists.
func unreferencedHiddenSecrets(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	candidates []string,
) ([]string, error) {
	var toolkitsPresent bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, schema+".elitea_tools",
	).Scan(&toolkitsPresent); err != nil {
		return nil, err
	}
	toolkitClause := ""
	if toolkitsPresent {
		toolkitClause = fmt.Sprintf(`
			AND NOT EXISTS (
				SELECT 1 FROM %s.elitea_tools t
				WHERE strpos(t.settings::text, '{{secret.' || n || '}}') > 0)`, schema)
	}
	query := fmt.Sprintf(`
		SELECT n FROM unnest($1::text[]) AS n
		WHERE NOT EXISTS (
			SELECT 1 FROM %s.configuration c
			WHERE strpos(c.data::text, '{{secret.' || n || '}}') > 0)%s
		ORDER BY n`, schema, toolkitClause)
	rows, err := tx.Query(ctx, query, candidates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
