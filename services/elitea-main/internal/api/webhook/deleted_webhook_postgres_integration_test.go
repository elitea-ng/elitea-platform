package webhook_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// TestDeliveryLogOfADeletedWebhookIsTheTypedGoneError pins the delivery
// logger against a webhook deleted while its attempt ran. The foreign key
// webhook_deliveries_webhook_id_fkey refuses the row. That used to surface
// as a wrapped 23503 that the async path logged at error level, and that
// Redeliver answered as a 500. It is now webhook.ErrWebhookGone, a typed 404.
func TestDeliveryLogOfADeletedWebhookIsTheTypedGoneError(t *testing.T) {
	pool := newWebhookPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)

	const projectID = "proj-webhook-gone"
	created, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL:    "https://autotest.invalid/hook",
		Events: []string{"conversation.created"},
		Secret: "integration-secret",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	delivery := webhook.Delivery{
		WebhookID: created.ID,
		ProjectID: projectID,
		Event:     "conversation.created",
		Status:    webhook.DeliveryStatusFailed,
		Attempts:  1,
		Payload:   []byte(`{"type":"conversation.created"}`),
	}
	if _, err := deliveriesRepo.Create(ctx, delivery); err != nil {
		t.Fatalf("log a delivery of a live webhook: %v", err)
	}

	if err := webhooksRepo.Delete(ctx, projectID, created.ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	_, err = deliveriesRepo.Create(ctx, delivery)
	if !errors.Is(err, webhook.ErrWebhookGone) {
		t.Fatalf("log a delivery of a deleted webhook = %v, want webhook.ErrWebhookGone", err)
	}
}
