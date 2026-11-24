package webhook_test

// Issue 2963 over a REAL Postgres database: a webhook's signing secret lives in
// the project vault and the row keeps only a reference; the start-up move
// takes an existing plaintext secret into the vault; and deliveries verify
// with the vault value before and after the move. Skips with no
// ELITEA_TEST_DATABASE_URL, like every _postgres_integration_test.go here.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// vaultedWebhooksRepo composes the registry with the project vault the way
// cmd/elitea-main does: one vault writer that creates an absent vault through
// the secrets handler, and that same handler as the dispatcher's resolver.
// SECRETS_MASTER_KEY is unset in this test process, so both read and write
// the unwrapped form.
func vaultedWebhooksRepo(t *testing.T, pool *pgxpool.Pool) (*repos.WebhooksRepo, *v2secrets.Handler) {
	t.Helper()
	secrets := v2secrets.NewHandler(pool)
	vault, err := repos.NewCurrentSecretVaultRepository(pool, nil, repos.WithProjectVaultCreator(secrets))
	if err != nil {
		t.Fatalf("vault repository: %v", err)
	}
	return repos.NewWebhooksRepo(pool, repos.WithWebhookSecretVault(vault)), secrets
}

type signedRequest struct {
	signature string
	body      []byte
}

// signatureReceiver records every delivery's signature header and body.
type signatureReceiver struct {
	mu       sync.Mutex
	requests []signedRequest
	server   *httptest.Server
}

func newSignatureReceiver(t *testing.T) *signatureReceiver {
	t.Helper()
	receiver := &signatureReceiver{}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receiver.mu.Lock()
		receiver.requests = append(receiver.requests, signedRequest{r.Header.Get(webhook.SignatureHeader), body})
		receiver.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

// assertVerifies fails unless request n verifies under secret.
func (s *signatureReceiver) assertVerifies(t *testing.T, n int, secret string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) <= n {
		t.Fatalf("receiver saw %d requests, want at least %d", len(s.requests), n+1)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(s.requests[n].body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); s.requests[n].signature != want {
		t.Fatalf("request %d signature = %q, want the HMAC under the expected secret", n, s.requests[n].signature)
	}
}

func storedSecretColumns(t *testing.T, pool *pgxpool.Pool, id string) (secret, ref string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT secret, secret_ref FROM webhooks WHERE id = $1`, id).Scan(&secret, &ref); err != nil {
		t.Fatalf("read webhook row: %v", err)
	}
	return secret, ref
}

// After a create the `secret` column is empty, the row holds a reference, the
// vault holds the value, and a delivery verifies under it. A rotation and a
// delete each remove the vault value the row stopped naming.
func TestCreateStoresTheSecretInTheVaultOnly(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()
	receiver := newSignatureReceiver(t)

	webhooksRepo, secrets := vaultedWebhooksRepo(t, pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo, webhook.WithSecretResolver(secrets))

	const projectID = "9102"
	created, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL: receiver.server.URL, Events: []string{"conversation.created"}, Secret: "created-in-the-vault", Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if !created.HasSecret || created.Secret != "" {
		t.Fatalf("created row has_secret=%v secret=%q, want a reference only", created.HasSecret, created.Secret)
	}

	column, ref := storedSecretColumns(t, pool, created.ID)
	if column != "" {
		t.Fatalf("the secret column holds %q after create, want empty", column)
	}
	if !strings.HasPrefix(ref, "{{secret.") {
		t.Fatalf("secret_ref = %q, want a vault reference", ref)
	}
	if value, err := secrets.ResolveSecretValue(ctx, projectID, ref); err != nil || value != "created-in-the-vault" {
		t.Fatalf("vault value = %q, %v; want the created secret", value, err)
	}

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{"conversation_id": "c-1"})
	waitForDeliveries(t, deliveriesRepo, projectID, created.ID, 1)
	receiver.assertVerifies(t, 0, "created-in-the-vault")

	// Rotation: a new reference, the old vault value gone, deliveries sign
	// with the new one.
	if _, err := webhooksRepo.RotateSecret(ctx, projectID, created.ID, "rotated-in-the-vault"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_, rotatedRef := storedSecretColumns(t, pool, created.ID)
	if rotatedRef == ref {
		t.Fatal("rotation kept the previous reference")
	}
	if _, err := secrets.ResolveSecretValue(ctx, projectID, ref); !errors.Is(err, v2secrets.ErrSecretNotFound) {
		t.Fatalf("the previous vault value after rotation: %v, want not found", err)
	}
	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{"conversation_id": "c-2"})
	waitForDeliveries(t, deliveriesRepo, projectID, created.ID, 2)
	receiver.assertVerifies(t, 1, "rotated-in-the-vault")

	// An update that sends no secret keeps it.
	if _, err := webhooksRepo.Update(ctx, projectID, created.ID, webhook.Webhook{
		URL: receiver.server.URL, Events: []string{"conversation.created"}, Active: true,
	}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, keptRef := storedSecretColumns(t, pool, created.ID); keptRef != rotatedRef {
		t.Fatalf("an update without a secret changed the reference to %q", keptRef)
	}
	// An empty secret is "keep" as well: a client echoing a read back must
	// not strip the signature.
	empty := ""
	if _, err := webhooksRepo.Update(ctx, projectID, created.ID, webhook.Webhook{
		URL: receiver.server.URL, Events: []string{"conversation.created"}, Active: true,
	}, &empty); err != nil {
		t.Fatalf("update with an empty secret: %v", err)
	}
	if _, keptRef := storedSecretColumns(t, pool, created.ID); keptRef != rotatedRef {
		t.Fatalf("an update with an empty secret changed the reference to %q", keptRef)
	}

	if err := webhooksRepo.Delete(ctx, projectID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := secrets.ResolveSecretValue(ctx, projectID, rotatedRef); !errors.Is(err, v2secrets.ErrSecretNotFound) {
		t.Fatalf("the vault value after delete: %v, want not found", err)
	}
}

// A row written before this change holds its secret in plain text. Until the
// start-up move reaches it, deliveries still verify with that value; the move
// seals it into the vault, blanks the column, and deliveries keep verifying
// with the same value. A second pass moves nothing.
func TestTheMoveTakesAPlaintextSecretIntoTheVault(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()
	receiver := newSignatureReceiver(t)

	webhooksRepo, secrets := vaultedWebhooksRepo(t, pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo, webhook.WithSecretResolver(secrets))

	const projectID = "9103"
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO webhooks (project_id, url, events, secret, active)
		VALUES ($1, $2, $3, $4, true) RETURNING id`,
		projectID, receiver.server.URL, []string{"conversation.created"}, "written-before-the-move",
	).Scan(&id); err != nil {
		t.Fatalf("insert a pre-move row: %v", err)
	}

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{})
	waitForDeliveries(t, deliveriesRepo, projectID, id, 1)
	receiver.assertVerifies(t, 0, "written-before-the-move")

	report, err := webhooksRepo.MoveLegacySecretsToVault(ctx)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if report.Moved != 1 || report.Skipped != 0 {
		t.Fatalf("move report = %+v, want one moved", report)
	}
	column, ref := storedSecretColumns(t, pool, id)
	if column != "" || ref == "" {
		t.Fatalf("after the move secret=%q secret_ref=%q, want an empty column and a reference", column, ref)
	}
	if value, err := secrets.ResolveSecretValue(ctx, projectID, ref); err != nil || value != "written-before-the-move" {
		t.Fatalf("vault value = %q, %v; want the moved secret", value, err)
	}

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{})
	waitForDeliveries(t, deliveriesRepo, projectID, id, 2)
	receiver.assertVerifies(t, 1, "written-before-the-move")

	again, err := webhooksRepo.MoveLegacySecretsToVault(ctx)
	if err != nil || again.Moved != 0 || again.Skipped != 0 {
		t.Fatalf("second move = %+v, %v; want nothing to do", again, err)
	}
}

// A webhook stored with no signing secret (created before #2959 made one
// mandatory) is not delivered unsigned: the dispatcher logs the delivery
// FAILED and sends nothing. The start-up pass then gives it a random secret in
// the vault, and from then on every delivery verifies with that value. A
// second pass generates nothing.
func TestTheStartUpPassGivesAnUnsignedWebhookASecret(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()
	receiver := newSignatureReceiver(t)

	webhooksRepo, secrets := vaultedWebhooksRepo(t, pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo, webhook.WithSecretResolver(secrets))

	const projectID = "9107"
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO webhooks (project_id, url, events, secret, active)
		VALUES ($1, $2, $3, '', true) RETURNING id`,
		projectID, receiver.server.URL, []string{"conversation.created"},
	).Scan(&id); err != nil {
		t.Fatalf("insert an unsigned row: %v", err)
	}

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{})
	before := waitForDeliveries(t, deliveriesRepo, projectID, id, 1)
	if before[0].Status != webhook.DeliveryStatusFailed || !strings.Contains(before[0].LastError, "no signing secret") {
		t.Fatalf("delivery before the pass = %s %q, want failed for the missing secret", before[0].Status, before[0].LastError)
	}
	receiver.mu.Lock()
	sent := len(receiver.requests)
	receiver.mu.Unlock()
	if sent != 0 {
		t.Fatalf("an unsigned webhook reached the receiver %d time(s)", sent)
	}

	report, err := webhooksRepo.MoveLegacySecretsToVault(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if report.Generated != 1 || report.Skipped != 0 {
		t.Fatalf("pass report = %+v, want one generated", report)
	}
	column, ref := storedSecretColumns(t, pool, id)
	if column != "" || ref == "" {
		t.Fatalf("after the pass secret=%q secret_ref=%q, want an empty column and a reference", column, ref)
	}
	value, err := secrets.ResolveSecretValue(ctx, projectID, ref)
	if err != nil || len(value) < 43 {
		t.Fatalf("vault value: len %d, %v; want a generated 32-byte secret", len(value), err)
	}

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{})
	waitForDeliveries(t, deliveriesRepo, projectID, id, 2)
	receiver.assertVerifies(t, 0, value)

	again, err := webhooksRepo.MoveLegacySecretsToVault(ctx)
	if err != nil || again.Generated != 0 || again.Moved != 0 || again.Skipped != 0 {
		t.Fatalf("second pass = %+v, %v; want nothing to do", again, err)
	}
}

// A registry composed without a vault refuses to store a secret rather than
// writing it in plain text.
func TestASecretWriteWithoutAVaultIsRefused(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	if _, err := webhooksRepo.Create(ctx, "9104", webhook.Webhook{
		URL: "https://autotest.invalid/hook", Events: []string{}, Secret: "never-stored", Active: true,
	}); err == nil {
		t.Fatal("create with a secret and no vault succeeded")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE secret <> ''`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d rows hold a plaintext secret", rows)
	}
}

// A webhook whose vault can no longer be written stays deletable: the vault
// clean-up is best effort and never blocks the row delete.
func TestDeleteSucceedsWhenTheVaultCannotBeWritten(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()

	vaulted, _ := vaultedWebhooksRepo(t, pool)
	created, err := vaulted.Create(ctx, "9105", webhook.Webhook{
		URL: "https://autotest.invalid/hook", Events: []string{}, Secret: "s", Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repos.NewWebhooksRepo(pool).Delete(ctx, "9105", created.ID); err != nil {
		t.Fatalf("delete without a vault: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE id = $1`, created.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the webhook row survived the delete")
	}
}
