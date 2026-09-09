package repos

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type WebhooksRepo struct {
	pool *pgxpool.Pool
}

func NewWebhooksRepo(pool *pgxpool.Pool) *WebhooksRepo {
	return &WebhooksRepo{pool: pool}
}

func (r *WebhooksRepo) List(ctx context.Context, projectID string) ([]webhook.Webhook, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, project_id, url, events, secret, active, created_at, updated_at
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
		var wh webhook.Webhook
		if err := rows.Scan(&wh.ID, &wh.ProjectID, &wh.URL, &wh.Events, &wh.Secret, &wh.Active, &wh.CreatedAt, &wh.UpdatedAt); err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		items = append(items, wh)
	}
	return items, nil
}

func (r *WebhooksRepo) Get(ctx context.Context, projectID, webhookID string) (webhook.Webhook, error) {
	var wh webhook.Webhook
	err := r.pool.QueryRow(ctx,
		`SELECT id, project_id, url, events, secret, active, created_at, updated_at
		FROM webhooks WHERE project_id = $1 AND id = $2`,
		projectID, webhookID,
	).Scan(&wh.ID, &wh.ProjectID, &wh.URL, &wh.Events, &wh.Secret, &wh.Active, &wh.CreatedAt, &wh.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return webhook.Webhook{}, apierr.NotFound("webhook not found")
		}
		return webhook.Webhook{}, fmt.Errorf("webhooks: get: %w", err)
	}
	return wh, nil
}

func (r *WebhooksRepo) Create(ctx context.Context, projectID string, wh webhook.Webhook) (webhook.Webhook, error) {
	var created webhook.Webhook
	err := r.pool.QueryRow(ctx,
		`INSERT INTO webhooks (project_id, url, events, secret, active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, project_id, url, events, secret, active, created_at, updated_at`,
		projectID, wh.URL, wh.Events, wh.Secret, wh.Active,
	).Scan(&created.ID, &created.ProjectID, &created.URL, &created.Events, &created.Secret, &created.Active, &created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		return webhook.Webhook{}, fmt.Errorf("webhooks: create: %w", err)
	}
	return created, nil
}

func (r *WebhooksRepo) Update(ctx context.Context, projectID, webhookID string, wh webhook.Webhook) (webhook.Webhook, error) {
	var updated webhook.Webhook
	err := r.pool.QueryRow(ctx,
		`UPDATE webhooks SET url = $1, events = $2, secret = $3, active = $4, updated_at = NOW()
		WHERE project_id = $5 AND id = $6
		RETURNING id, project_id, url, events, secret, active, created_at, updated_at`,
		wh.URL, wh.Events, wh.Secret, wh.Active, projectID, webhookID,
	).Scan(&updated.ID, &updated.ProjectID, &updated.URL, &updated.Events, &updated.Secret, &updated.Active, &updated.CreatedAt, &updated.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return webhook.Webhook{}, apierr.NotFound("webhook not found")
		}
		return webhook.Webhook{}, fmt.Errorf("webhooks: update: %w", err)
	}
	return updated, nil
}

func (r *WebhooksRepo) Delete(ctx context.Context, projectID, webhookID string) error {
	ct, err := r.pool.Exec(ctx, `DELETE FROM webhooks WHERE project_id = $1 AND id = $2`, projectID, webhookID)
	if err != nil {
		return fmt.Errorf("webhooks: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("webhook not found")
	}
	return nil
}

func (r *WebhooksRepo) ListByEvent(ctx context.Context, projectID, eventType string) ([]webhook.Webhook, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, project_id, url, events, secret, active, created_at, updated_at
		FROM webhooks WHERE project_id = $1 AND active = true AND $2 = ANY(events)`,
		projectID, eventType)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list by event: %w", err)
	}
	defer rows.Close()

	var items []webhook.Webhook
	for rows.Next() {
		var wh webhook.Webhook
		if err := rows.Scan(&wh.ID, &wh.ProjectID, &wh.URL, &wh.Events, &wh.Secret, &wh.Active, &wh.CreatedAt, &wh.UpdatedAt); err != nil {
			return nil, fmt.Errorf("webhooks: scan: %w", err)
		}
		items = append(items, wh)
	}
	return items, nil
}
