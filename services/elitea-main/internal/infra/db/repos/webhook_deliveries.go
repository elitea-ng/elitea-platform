package repos

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// WebhookDeliveriesRepo is webhook.DeliveryRepository's Postgres
// implementation, over the public.webhook_deliveries table
// migrations/shared/0122_webhooks_and_deliveries.sql creates.
type WebhookDeliveriesRepo struct {
	pool *pgxpool.Pool
}

func NewWebhookDeliveriesRepo(pool *pgxpool.Pool) *WebhookDeliveriesRepo {
	return &WebhookDeliveriesRepo{pool: pool}
}

func (r *WebhookDeliveriesRepo) Create(ctx context.Context, d webhook.Delivery) (webhook.Delivery, error) {
	var redeliveryOf any
	if d.RedeliveryOf != "" {
		redeliveryOf = d.RedeliveryOf
	}
	var created webhook.Delivery
	err := r.pool.QueryRow(ctx,
		`INSERT INTO webhook_deliveries
			(webhook_id, project_id, event, status, attempts, response_code, last_error, payload, redelivery_of)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, webhook_id, project_id, event, status, attempts, response_code,
			COALESCE(last_error, ''), payload, COALESCE(redelivery_of::text, ''), created_at, updated_at`,
		d.WebhookID, d.ProjectID, d.Event, string(d.Status), d.Attempts, d.ResponseCode, nullableString(d.LastError), d.Payload, redeliveryOf,
	).Scan(&created.ID, &created.WebhookID, &created.ProjectID, &created.Event, &created.Status,
		&created.Attempts, &created.ResponseCode, &created.LastError, &created.Payload, &created.RedeliveryOf,
		&created.CreatedAt, &created.UpdatedAt)
	if err != nil {
		return webhook.Delivery{}, fmt.Errorf("webhook_deliveries: create: %w", err)
	}
	return created, nil
}

func (r *WebhookDeliveriesRepo) ListRecent(ctx context.Context, projectID, webhookID string, limit int) ([]webhook.Delivery, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, webhook_id, project_id, event, status, attempts, response_code,
			COALESCE(last_error, ''), payload, COALESCE(redelivery_of::text, ''), created_at, updated_at
		FROM webhook_deliveries
		WHERE project_id = $1 AND webhook_id = $2
		ORDER BY created_at DESC
		LIMIT $3`, projectID, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("webhook_deliveries: list recent: %w", err)
	}
	defer rows.Close()

	var items []webhook.Delivery
	for rows.Next() {
		var d webhook.Delivery
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.ProjectID, &d.Event, &d.Status,
			&d.Attempts, &d.ResponseCode, &d.LastError, &d.Payload, &d.RedeliveryOf,
			&d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("webhook_deliveries: scan: %w", err)
		}
		items = append(items, d)
	}
	return items, nil
}

func (r *WebhookDeliveriesRepo) Get(ctx context.Context, projectID, webhookID, deliveryID string) (webhook.Delivery, error) {
	var d webhook.Delivery
	err := r.pool.QueryRow(ctx,
		`SELECT id, webhook_id, project_id, event, status, attempts, response_code,
			COALESCE(last_error, ''), payload, COALESCE(redelivery_of::text, ''), created_at, updated_at
		FROM webhook_deliveries
		WHERE project_id = $1 AND webhook_id = $2 AND id = $3`,
		projectID, webhookID, deliveryID,
	).Scan(&d.ID, &d.WebhookID, &d.ProjectID, &d.Event, &d.Status,
		&d.Attempts, &d.ResponseCode, &d.LastError, &d.Payload, &d.RedeliveryOf,
		&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return webhook.Delivery{}, apierr.NotFound("delivery not found")
		}
		return webhook.Delivery{}, fmt.Errorf("webhook_deliveries: get: %w", err)
	}
	return d, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
