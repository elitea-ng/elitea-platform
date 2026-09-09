package webhook_test

// Acceptance for #876's second half: an event really reaches a webhook,
// signed, and the outcome is really persisted — over a REAL Postgres
// database, with the REAL migrations/shared/0122_webhooks_and_deliveries.sql
// schema and the REAL internal/infra/db/repos implementations, not the
// in-memory fakes dispatcher_test.go uses for the fast, DB-free half of this
// coverage.
//
// Same template-database technique
// internal/api/v2/moderation/project_requests_postgres_integration_test.go
// uses: one ledgered-migration template built once in TestMain, copied per
// test. No tenant schema is needed here — webhooks and webhook_deliveries
// are shared-schema tables with a plain `project_id text` column, not a
// per-project p_<id> schema — so Spec.Tenants is empty.
//
// Skips (not fails) with no ELITEA_TEST_DATABASE_URL, same as every other
// _postgres_integration_test.go in this service.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const webhookDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
const webhookBootstrapSchema = "../../infra/db/migrations/001_initial.sql"

var webhookTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(webhookDatabaseURLEnv)
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(webhookBootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build webhook template: %v\n", err)
		os.Exit(1)
	}
	webhookTemplate = templateName
	os.Exit(m.Run())
}

func newWebhookPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(webhookDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", webhookDatabaseURLEnv)
	}
	if webhookTemplate == "" {
		t.Fatalf("TestMain did not build the webhook template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_webhook_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, webhookTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", webhookDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

func waitForDeliveries(t *testing.T, deliveries webhook.DeliveryRepository, projectID, webhookID string, count int) []webhook.Delivery {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		items, err := deliveries.ListRecent(context.Background(), projectID, webhookID, 20)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		if len(items) >= count {
			return items
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("did not observe %d delivery row(s) before the deadline", count)
	return nil
}

// TestConversationCreatedEventIsDeliveredSignedLoggedAndRedeliverable is the
// end-to-end proof the issue asks for: register a webhook, fire the
// dispatcher exactly the way internal/events.Publisher.Emit does for the
// conversation.created producer (internal/api/v2/conversations/handler.go's
// Create), and assert the receiver got a correctly signed request, the
// attempt is logged in webhook_deliveries, and Redeliver produces a second,
// linked row.
func TestConversationCreatedEventIsDeliveredSignedLoggedAndRedeliverable(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()

	var received int32
	var gotSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		gotSignature = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo)

	const projectID = "proj-webhook-it"
	created, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL:    server.URL,
		Events: []string{"conversation.created"},
		Secret: "integration-secret",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	// The exact call events.Publisher.Emit makes on the sink it was built
	// with — see internal/events/publisher.go's Emit and
	// cmd/elitea-main/main.go's composition of the domain events publisher.
	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{
		"conversation_id": "conv-it-1",
		"name":            "Integration test conversation",
	})

	items := waitForDeliveries(t, deliveriesRepo, projectID, created.ID, 1)
	if atomic.LoadInt32(&received) != 1 {
		t.Fatalf("receiver saw %d requests, want 1", received)
	}
	if gotSignature == "" {
		t.Fatal("delivery carried no signature header")
	}

	delivery := items[0]
	if delivery.Status != webhook.DeliveryStatusSuccess {
		t.Errorf("status = %s, want success", delivery.Status)
	}
	if delivery.Event != "conversation.created" {
		t.Errorf("event = %s, want conversation.created", delivery.Event)
	}
	if delivery.ResponseCode == nil || *delivery.ResponseCode != http.StatusOK {
		t.Errorf("response_code = %v, want 200", delivery.ResponseCode)
	}
	if delivery.RedeliveryOf != "" {
		t.Errorf("original delivery carries redelivery_of = %q, want empty", delivery.RedeliveryOf)
	}

	// Redeliver — the "Recent deliveries" panel's action.
	redelivered, err := dispatcher.Redeliver(ctx, projectID, created.ID, delivery.ID)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if redelivered.RedeliveryOf != delivery.ID {
		t.Errorf("redelivery_of = %q, want %q", redelivered.RedeliveryOf, delivery.ID)
	}
	if atomic.LoadInt32(&received) != 2 {
		t.Fatalf("receiver saw %d requests after redeliver, want 2", received)
	}

	all := waitForDeliveries(t, deliveriesRepo, projectID, created.ID, 2)
	if len(all) != 2 {
		t.Fatalf("webhook_deliveries has %d rows, want 2", len(all))
	}
}

// TestListDeliveriesIsScopedToTheOwningProjectAndWebhook proves the same
// isolation discipline the five CRUD routes already carry (#496) extends to
// the new delivery log: a delivery cannot be read, and cannot be
// redelivered, through another project's or another webhook's id.
func TestListDeliveriesIsScopedToTheOwningProjectAndWebhook(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo)

	ownerWebhook, err := webhooksRepo.Create(ctx, "proj-owner", webhook.Webhook{
		URL: server.URL, Events: []string{"conversation.created"}, Secret: "s", Active: true,
	})
	if err != nil {
		t.Fatalf("create owner webhook: %v", err)
	}

	dispatcher.HandleDomainEvent(ctx, "proj-owner", "conversation.created", map[string]any{})
	items := waitForDeliveries(t, deliveriesRepo, "proj-owner", ownerWebhook.ID, 1)
	deliveryID := items[0].ID

	if _, err := deliveriesRepo.Get(ctx, "proj-intruder", ownerWebhook.ID, deliveryID); err == nil {
		t.Error("a delivery was readable under a different project id")
	}
	if _, err := dispatcher.Redeliver(ctx, "proj-intruder", ownerWebhook.ID, deliveryID); err == nil {
		t.Error("a delivery was redeliverable under a different project id")
	}
}
