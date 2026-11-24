package repos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/webhook"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// WebhookSecretVault is the project vault a webhook's signing secret is
// sealed into (issue 2963). Both methods run inside the transaction that
// writes the webhook row, so the row and the vault commit together.
// *CurrentSecretVaultRepository implements it.
type WebhookSecretVault interface {
	SealProjectHiddenSecrets(ctx context.Context, tx pgx.Tx, projectID int64, mutations []configurationapp.HiddenSecretMutation) error
	PurgeProjectHiddenSecrets(ctx context.Context, tx pgx.Tx, projectID int64, names []string) error
}

type WebhooksRepo struct {
	pool *pgxpool.Pool
	// vault holds every signing secret. nil refuses any write that stores
	// one (503); it never falls back to the plaintext column.
	vault WebhookSecretVault
}

// WebhooksRepoOption configures a WebhooksRepo.
type WebhooksRepoOption func(*WebhooksRepo)

// WithWebhookSecretVault supplies the vault signing secrets are sealed into.
func WithWebhookSecretVault(vault WebhookSecretVault) WebhooksRepoOption {
	return func(r *WebhooksRepo) { r.vault = vault }
}

func NewWebhooksRepo(pool *pgxpool.Pool, opts ...WebhooksRepoOption) *WebhooksRepo {
	r := &WebhooksRepo{pool: pool}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// webhookColumns is every column a read scans, in scanWebhook's order. The
// plaintext `secret` is read only so the dispatcher can still sign a row the
// start-up vault move has not reached; no route returns it.
const webhookColumns = `id, project_id, url, events, secret, secret_ref, active, created_at, updated_at`

func scanWebhook(row rowScanner) (webhook.Webhook, error) {
	var wh webhook.Webhook
	if err := row.Scan(&wh.ID, &wh.ProjectID, &wh.URL, &wh.Events, &wh.Secret, &wh.SecretRef, &wh.Active, &wh.CreatedAt, &wh.UpdatedAt); err != nil {
		return webhook.Webhook{}, err
	}
	wh.HasSecret = wh.Secret != "" || wh.SecretRef != ""
	return wh, nil
}

// errWebhookSecretStoreUnavailable is the 503 a secret write answers when
// this deployment composed no vault.
var errWebhookSecretStoreUnavailable = &apierr.APIError{
	Status:  http.StatusServiceUnavailable,
	Code:    "service_unavailable",
	Message: "the webhook secret store is not available",
}

func (r *WebhooksRepo) List(ctx context.Context, projectID string) ([]webhook.Webhook, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+webhookColumns+`
		FROM webhooks WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()

	// A nil slice marshals to JSON `null`, not `[]` — and the settings page
	// (and this route's own E2E journey, `settings.webhooks.spec.ts`) reads
	// `body.items` and calls array methods (`.find`) on it directly. A
	// project with zero or just-deleted webhooks used to answer
	// `{"items":null}`, which crashed the very "list came back empty" checks
	// create/rotate/disable/delete each end on (#882 CI).
	items := []webhook.Webhook{}
	for rows.Next() {
		wh, err := scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		items = append(items, wh)
	}
	return items, rows.Err()
}

func (r *WebhooksRepo) Get(ctx context.Context, projectID, webhookID string) (webhook.Webhook, error) {
	wh, err := scanWebhook(r.pool.QueryRow(ctx,
		`SELECT `+webhookColumns+`
		FROM webhooks WHERE project_id = $1 AND id = $2`,
		projectID, webhookID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return webhook.Webhook{}, apierr.NotFound("webhook not found")
		}
		return webhook.Webhook{}, fmt.Errorf("webhooks: get: %w", err)
	}
	return wh, nil
}

// Create inserts the row and, when wh.Secret is set, seals it into the
// project vault in the same transaction. The `secret` column is written
// empty: the row holds only the reference.
func (r *WebhooksRepo) Create(ctx context.Context, projectID string, wh webhook.Webhook) (webhook.Webhook, error) {
	var created webhook.Webhook
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		ref, err := r.sealSecret(ctx, tx, projectID, wh.Secret)
		if err != nil {
			return err
		}
		created, err = scanWebhook(tx.QueryRow(ctx,
			`INSERT INTO webhooks (project_id, url, events, secret, secret_ref, active)
			VALUES ($1, $2, $3, '', $4, $5)
			RETURNING `+webhookColumns,
			projectID, wh.URL, wh.Events, ref, wh.Active,
		))
		if err != nil {
			return fmt.Errorf("webhooks: create: %w", err)
		}
		return nil
	})
	return created, err
}

// Update replaces url, events and active. A nil or empty secret leaves the
// stored secret as it is; a non-empty one replaces it. An empty string is
// "keep", not "remove": a client that echoes a read back (which carries no
// secret) must not silently strip the webhook's signature.
func (r *WebhooksRepo) Update(ctx context.Context, projectID, webhookID string, wh webhook.Webhook, secret *string) (webhook.Webhook, error) {
	var updated webhook.Webhook
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		if secret != nil && *secret != "" {
			if err := r.replaceSecret(ctx, tx, projectID, webhookID, *secret); err != nil {
				return err
			}
		}
		var err error
		updated, err = scanWebhook(tx.QueryRow(ctx,
			`UPDATE webhooks SET url = $1, events = $2, active = $3, updated_at = NOW()
			WHERE project_id = $4 AND id = $5
			RETURNING `+webhookColumns,
			wh.URL, wh.Events, wh.Active, projectID, webhookID,
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.NotFound("webhook not found")
			}
			return fmt.Errorf("webhooks: update: %w", err)
		}
		return nil
	})
	return updated, err
}

// RotateSecret replaces the signing secret and removes the previous vault
// value, in one transaction.
func (r *WebhooksRepo) RotateSecret(ctx context.Context, projectID, webhookID, secret string) (webhook.Webhook, error) {
	if secret == "" {
		return webhook.Webhook{}, apierr.BadRequest("a rotated secret must not be empty")
	}
	var rotated webhook.Webhook
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		if err := r.replaceSecret(ctx, tx, projectID, webhookID, secret); err != nil {
			return err
		}
		var err error
		rotated, err = scanWebhook(tx.QueryRow(ctx,
			`UPDATE webhooks SET updated_at = NOW() WHERE project_id = $1 AND id = $2
			RETURNING `+webhookColumns,
			projectID, webhookID,
		))
		if err != nil {
			return fmt.Errorf("webhooks: rotate secret: %w", err)
		}
		return nil
	})
	return rotated, err
}

// Delete removes the row and, best effort, the vault value its reference
// names, in one transaction. A vault that cannot be written does not block
// the delete (see purgeSecret): a broken webhook must stay removable.
func (r *WebhooksRepo) Delete(ctx context.Context, projectID, webhookID string) error {
	return r.withTx(ctx, func(tx pgx.Tx) error {
		var ref string
		err := tx.QueryRow(ctx,
			`DELETE FROM webhooks WHERE project_id = $1 AND id = $2 RETURNING secret_ref`,
			projectID, webhookID,
		).Scan(&ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound("webhook not found")
		}
		if err != nil {
			return fmt.Errorf("webhooks: delete: %w", err)
		}
		r.purgeSecret(ctx, tx, projectID, ref)
		return nil
	})
}

func (r *WebhooksRepo) ListByEvent(ctx context.Context, projectID, eventType string) ([]webhook.Webhook, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+webhookColumns+`
		FROM webhooks WHERE project_id = $1 AND active = true AND $2 = ANY(events)`,
		projectID, eventType)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list by event: %w", err)
	}
	defer rows.Close()

	var items []webhook.Webhook
	for rows.Next() {
		wh, err := scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		items = append(items, wh)
	}
	return items, rows.Err()
}

// WebhookSecretMoveReport counts what one MoveLegacySecretsToVault pass did.
type WebhookSecretMoveReport struct {
	// Moved is how many rows had their plaintext sealed into the vault.
	Moved int
	// Generated is how many rows had NO signing secret and got a new random
	// one sealed into the vault (#2959: every delivery is signed).
	Generated int
	// Skipped is how many rows could not be moved or given a secret. Each one
	// is logged with its webhook id and retried on the next start; until
	// then a row with plaintext keeps signing with it, and a row with no
	// secret is not delivered.
	Skipped int
}

// MoveLegacySecretsToVault seals every plaintext `secret` still in the table
// into its project vault, writes the reference and blanks the column (issue
// 2963, migrations/shared/0267_webhook_secret_reference.sql).
//
// It is Go and not SQL because SQL cannot open the vault: the value is sealed
// with the project's Fernet key. One row per transaction, each re-checked
// under a row lock, so two replicas starting together move each row once, and
// one row that cannot be moved (a project id that names no vault-capable
// project, say) does not hold back the rest. Idempotent: a second pass finds
// nothing to do.
func (r *WebhooksRepo) MoveLegacySecretsToVault(ctx context.Context) (WebhookSecretMoveReport, error) {
	var report WebhookSecretMoveReport
	if r == nil || r.pool == nil {
		return report, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id FROM webhooks WHERE secret <> '' AND secret_ref = '' ORDER BY created_at, id`)
	if err != nil {
		return report, fmt.Errorf("webhooks: list plaintext secrets: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return report, fmt.Errorf("webhooks: list plaintext secrets: %w", err)
	}
	if len(ids) > 0 && r.vault == nil {
		return report, errWebhookSecretStoreUnavailable
	}
	for _, id := range ids {
		moved, err := r.moveOneLegacySecret(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			report.Skipped++
			slog.ErrorContext(ctx, "webhooks: could not move a signing secret into the project vault",
				"webhook", id, "err", err)
			continue
		}
		if moved {
			report.Moved++
		}
	}

	// Every delivery is signed (#2959), so a webhook created before the
	// secret was mandatory gets one now: a new random value, sealed into its
	// project vault like any other. The dispatcher refuses to deliver a row
	// with no secret, so a row this pass cannot reach is never sent
	// unsigned; it is logged and retried on the next start. The owner reads
	// the value with rotate_secret, which returns a new one once.
	rows, err = r.pool.Query(ctx,
		`SELECT id FROM webhooks WHERE secret = '' AND secret_ref = '' ORDER BY created_at, id`)
	if err != nil {
		return report, fmt.Errorf("webhooks: list unsigned webhooks: %w", err)
	}
	unsigned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return report, fmt.Errorf("webhooks: list unsigned webhooks: %w", err)
	}
	if len(unsigned) > 0 && r.vault == nil {
		return report, errWebhookSecretStoreUnavailable
	}
	for _, id := range unsigned {
		generated, err := r.generateOneMissingSecret(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			report.Skipped++
			slog.ErrorContext(ctx, "webhooks: could not give a webhook a signing secret",
				"webhook", id, "err", err)
			continue
		}
		if generated {
			report.Generated++
		}
	}
	return report, nil
}

// generateOneMissingSecret seals a new random signing secret for one webhook
// that has none, under a row lock re-checked inside the transaction, so two
// replicas starting together generate it once.
func (r *WebhooksRepo) generateOneMissingSecret(ctx context.Context, id string) (bool, error) {
	generated := false
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var projectID string
		err := tx.QueryRow(ctx,
			`SELECT project_id FROM webhooks
			WHERE id = $1 AND secret = '' AND secret_ref = ''
			FOR UPDATE SKIP LOCKED`, id,
		).Scan(&projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		secret, err := webhook.NewSigningSecret()
		if err != nil {
			return fmt.Errorf("webhooks: generate a signing secret: %w", err)
		}
		ref, err := r.sealSecret(ctx, tx, projectID, secret)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE webhooks SET secret_ref = $2 WHERE id = $1`, id, ref); err != nil {
			return err
		}
		generated = true
		return nil
	})
	return generated, err
}

func (r *WebhooksRepo) moveOneLegacySecret(ctx context.Context, id string) (bool, error) {
	moved := false
	err := r.withTx(ctx, func(tx pgx.Tx) error {
		var projectID, secret string
		err := tx.QueryRow(ctx,
			`SELECT project_id, secret FROM webhooks
			WHERE id = $1 AND secret <> '' AND secret_ref = ''
			FOR UPDATE SKIP LOCKED`, id,
		).Scan(&projectID, &secret)
		if errors.Is(err, pgx.ErrNoRows) {
			// Moved, deleted or locked by another replica meanwhile.
			return nil
		}
		if err != nil {
			return err
		}
		ref, err := r.sealSecret(ctx, tx, projectID, secret)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE webhooks SET secret = '', secret_ref = $2 WHERE id = $1`, id, ref); err != nil {
			return err
		}
		moved = true
		return nil
	})
	return moved, err
}

// replaceSecret points the row at a new vault value and removes the value it
// pointed at before. It locks the row first, so two
// concurrent rotations cannot both purge the same previous value and leave
// one reference naming nothing.
func (r *WebhooksRepo) replaceSecret(ctx context.Context, tx pgx.Tx, projectID, webhookID, secret string) error {
	var previous string
	err := tx.QueryRow(ctx,
		`SELECT secret_ref FROM webhooks WHERE project_id = $1 AND id = $2 FOR UPDATE`,
		projectID, webhookID,
	).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound("webhook not found")
	}
	if err != nil {
		return fmt.Errorf("webhooks: lock for secret change: %w", err)
	}
	ref, err := r.sealSecret(ctx, tx, projectID, secret)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE webhooks SET secret = '', secret_ref = $3 WHERE project_id = $1 AND id = $2`,
		projectID, webhookID, ref); err != nil {
		return fmt.Errorf("webhooks: store secret reference: %w", err)
	}
	r.purgeSecret(ctx, tx, projectID, previous)
	return nil
}

// sealSecret stores one signing secret as a hidden secret under a new random
// name and returns its `{{secret.NAME}}` reference, or "" for no secret.
func (r *WebhooksRepo) sealSecret(ctx context.Context, tx pgx.Tx, projectID, secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	if r.vault == nil {
		return "", errWebhookSecretStoreUnavailable
	}
	numericProject, err := vaultProjectID(projectID)
	if err != nil {
		return "", err
	}
	name, err := newWebhookSecretName()
	if err != nil {
		return "", fmt.Errorf("webhooks: name the secret: %w", err)
	}
	if err := r.vault.SealProjectHiddenSecrets(ctx, tx, numericProject, []configurationapp.HiddenSecretMutation{{
		Field: "webhook_secret", Name: name, Value: secret,
	}}); err != nil {
		slog.ErrorContext(ctx, "webhooks: seal signing secret", "project", projectID, "err", err)
		return "", errWebhookSecretStoreUnavailable
	}
	return "{{secret." + name + "}}", nil
}

// purgeSecret removes the vault value a reference names, BEST EFFORT. "" is a
// no-op.
//
// A failure is logged and does not fail the caller. The row already stopped
// naming the value, so what is left behind is an unreferenced hidden secret,
// readable by no route. Failing instead would make a webhook in a project
// whose vault cannot be written (absent, or sealed under another master key)
// impossible to delete or rotate. The purge runs under a savepoint, so a
// failed vault statement cannot abort the caller's transaction.
func (r *WebhooksRepo) purgeSecret(ctx context.Context, tx pgx.Tx, projectID, ref string) {
	name, ok := webhookSecretName(ref)
	if !ok {
		return
	}
	logFailure := func(err error) {
		slog.ErrorContext(ctx, "webhooks: could not remove a replaced signing secret from the project vault",
			"project", projectID, "err", err)
	}
	if r.vault == nil {
		logFailure(errWebhookSecretStoreUnavailable)
		return
	}
	numericProject, err := vaultProjectID(projectID)
	if err != nil {
		logFailure(err)
		return
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		logFailure(err)
		return
	}
	if err := r.vault.PurgeProjectHiddenSecrets(ctx, savepoint, numericProject, []string{name}); err != nil {
		_ = savepoint.Rollback(ctx)
		logFailure(err)
		return
	}
	if err := savepoint.Commit(ctx); err != nil {
		logFailure(err)
	}
}

// vaultProjectID is the numeric id a project vault is keyed by. The route
// gate already refuses a non-numeric project id, so a failure here is a row
// that no route could have written.
func vaultProjectID(projectID string) (int64, error) {
	id, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || id <= 0 {
		return 0, apierr.BadRequest("a webhook secret needs a numeric project id")
	}
	return id, nil
}

// webhookSecretName extracts NAME from `{{secret.NAME}}`.
func webhookSecretName(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "{{secret.") || !strings.HasSuffix(ref, "}}") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(ref, "{{secret."), "}}")
	return name, name != ""
}

// newWebhookSecretName returns one hidden-secret name: 32 lowercase
// hexadecimal characters, the shape every other hidden secret uses.
func newWebhookSecretName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (r *WebhooksRepo) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("webhooks: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
