package webhook_test

// Acceptance for the SSRF hardening: a webhook pointed at a loopback address,
// the cloud metadata address, or a name that resolves into a private range is
// refused at Create with 400 — and, if it reaches the table some other way
// (a direct repository write, standing in for a row admitted before this
// guard existed, or written by a future code path that forgets to validate),
// the Dispatcher refuses to dial it and logs the outcome as `blocked` rather
// than silently doing nothing or retrying forever.
//
// Shares TestMain, newWebhookPool and waitForDeliveries with
// dispatcher_postgres_integration_test.go (same package, same template
// database technique). Skips with no ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// rebindingResolver is a fake DNS answer for a name the attacker controls —
// standing in for the exact primitive DNS rebinding exploits: a hostname
// that resolves into RFC1918 space.
type rebindingResolver struct {
	host string
	ip   string
}

func (r rebindingResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if host == r.host {
		return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func postWebhookCreate(t *testing.T, h *webhook.Handler, projectID, destinationURL string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(webhook.Webhook{
		URL:    destinationURL,
		Events: []string{"conversation.created"},
		Active: true,
	})
	if err != nil {
		t.Fatalf("marshal webhook payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/prompt_lib/"+projectID, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.Create(w, req)
	return w
}

func TestWebhookDestinationIsRefusedAtCreate(t *testing.T) {
	pool := newWebhookPool(t)
	webhooksRepo := repos.NewWebhooksRepo(pool)

	cases := []struct {
		name    string
		guard   *webhook.DestinationGuard
		url     string
		wantMsg string
	}{
		{
			name:  "loopback IP literal",
			guard: webhook.NewDestinationGuard(nil),
			url:   "http://127.0.0.1:9/hook",
		},
		{
			name:  "cloud metadata address",
			guard: webhook.NewDestinationGuard(nil),
			url:   "http://169.254.169.254/latest/meta-data/",
		},
		{
			name:  "a name that resolves to a 10.x address",
			guard: webhook.NewDestinationGuardWithResolver(nil, rebindingResolver{host: "internal.rebind.example", ip: "10.5.5.5"}),
			url:   "http://internal.rebind.example/hook",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := webhook.NewHandler(webhooksRepo, webhook.WithDestinationGuard(tc.guard))
			w := postWebhookCreate(t, h, "proj-ssrf-create", tc.url)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if _, ok := body["error"]; !ok {
				t.Errorf("error body has no `error` field: %v", body)
			}
		})
	}
}

// TestWebhookSmuggledIntoTableIsRefusedAtDialAndLoggedBlocked is the
// defence-in-depth half: a row that reached the table WITHOUT going through
// Handler.Create's guard (a direct repository write, here standing in for a
// row that predates this change, or a future bug that skips validation) is
// still refused when the Dispatcher actually tries to deliver to it, and the
// refusal is durably logged so an operator can see WHY nothing was ever
// delivered.
func TestWebhookSmuggledIntoTableIsRefusedAtDialAndLoggedBlocked(t *testing.T) {
	pool := newWebhookPool(t)
	ctx := context.Background()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)

	const projectID = "proj-ssrf-dial"
	smuggled, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL:    "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		Events: []string{"conversation.created"},
		Secret: "s",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create smuggled webhook row: %v", err)
	}

	guard := webhook.NewDestinationGuard(nil)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo, webhook.WithGuard(guard))

	dispatcher.HandleDomainEvent(ctx, projectID, "conversation.created", map[string]any{"conversation_id": "conv-1"})

	items := waitForDeliveries(t, deliveriesRepo, projectID, smuggled.ID, 1)
	delivery := items[0]
	if delivery.Status != webhook.DeliveryStatusBlocked {
		t.Errorf("status = %s, want %s", delivery.Status, webhook.DeliveryStatusBlocked)
	}
	if delivery.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a blocked destination is never retried)", delivery.Attempts)
	}
	if delivery.LastError == "" {
		t.Error("last_error is empty on a blocked delivery")
	}
}
