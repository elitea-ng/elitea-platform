package configurations_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// fakeGateway is an httptest server standing in for elitea-llm-gateway's
// POST /llm/v1/check_connection, so GatewayConnectionChecker.Check's wire
// behaviour (request shape, identity headers, response mapping) can be
// verified without a live gateway or podman stack.
type fakeGateway struct {
	*llmproxytest.Server
	lastRequest map[string]any
	lastHeaders http.Header
	respond     func(w http.ResponseWriter)
}

func newFakeGateway(t *testing.T, respond func(w http.ResponseWriter)) *fakeGateway {
	fg := &fakeGateway{respond: respond}
	fg.Server = llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fg.lastRequest = body
		fg.lastHeaders = r.Header.Clone()
		fg.respond(w)
	}))
	return fg
}

// client is a Gateway client of fg over real mutual TLS.
func (fg *fakeGateway) client(t *testing.T, secret string) llmproxy.Gateway {
	t.Helper()
	client, err := llmproxy.NewGatewayClient(llmproxy.GatewayClientConfig{Target: fg.Target, IdentitySecret: secret, TLS: fg.ClientTLS(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func writeGatewayJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestGatewayConnectionChecker_SuccessRoundTrip proves the checker forwards
// the credential type/data to the gateway and reports success only when the
// gateway itself reports success.
func TestGatewayConnectionChecker_SuccessRoundTrip(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter) {
		writeGatewayJSON(w, http.StatusOK, map[string]any{"success": true, "reason": "ok"})
	})
	defer gw.Close()

	checker := handler.NewGatewayConnectionChecker(gw.client(t, "shared-secret"))
	ctx := handler.WithConnectionCheckProjectID(context.Background(), "42")

	result, err := checker.Check(ctx, "open_ai", map[string]any{"api_base": "https://api.openai.com/v1", "api_key": "sk-good"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}

	if gw.lastRequest["type"] != "open_ai" {
		t.Errorf("gateway received type=%v, want open_ai", gw.lastRequest["type"])
	}
	if gw.lastRequest["api_base"] != "https://api.openai.com/v1" {
		t.Errorf("gateway received api_base=%v", gw.lastRequest["api_base"])
	}
	if gw.lastRequest["api_key"] != "sk-good" {
		t.Errorf("gateway received api_key=%v", gw.lastRequest["api_key"])
	}
	if pid := gw.lastHeaders.Get("X-Elitea-Project-Id"); pid != "42" {
		t.Errorf("X-Elitea-Project-Id header = %q, want 42", pid)
	}
	if sig := gw.lastHeaders.Get("X-Elitea-Identity-Signature"); sig == "" {
		t.Error("expected a signed identity header when a secret is configured")
	}
}

// TestGatewayConnectionChecker_ForwardsCloudCredentialFields proves the
// amazon_bedrock and vertex_ai credential fields actually reach the gateway.
//
// They are the ONLY thing those two types authenticate with: they carry no
// api_base and no api_key, so a body that forwarded api_base/api_key alone
// would make the gateway answer "this credential is missing ..." for a
// perfectly valid credential — a real failure reported for a working
// credential, which is the mirror image of the false success #319 removed.
// The wire field names are the gateway's own (checkConnectionRequest).
func TestGatewayConnectionChecker_ForwardsCloudCredentialFields(t *testing.T) {
	t.Run("amazon_bedrock", func(t *testing.T) {
		gw := newFakeGateway(t, func(w http.ResponseWriter) {
			writeGatewayJSON(w, http.StatusOK, map[string]any{"success": true, "reason": "ok"})
		})
		defer gw.Close()

		checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
		if _, err := checker.Check(context.Background(), "amazon_bedrock", map[string]any{
			"aws_access_key_id":     "AKIAIOSFODNN7EXAMPLE",
			"aws_secret_access_key": "secret-value",
			"aws_region_name":       "eu-central-1",
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for field, want := range map[string]string{
			"aws_access_key_id":     "AKIAIOSFODNN7EXAMPLE",
			"aws_secret_access_key": "secret-value",
			"aws_region_name":       "eu-central-1",
		} {
			if got := gw.lastRequest[field]; got != want {
				t.Errorf("gateway received %s=%v, want %q", field, got, want)
			}
		}
	})

	t.Run("vertex_ai", func(t *testing.T) {
		gw := newFakeGateway(t, func(w http.ResponseWriter) {
			writeGatewayJSON(w, http.StatusOK, map[string]any{"success": true, "reason": "ok"})
		})
		defer gw.Close()

		checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
		if _, err := checker.Check(context.Background(), "vertex_ai", map[string]any{
			"vertex_project":     "elitea-test-project",
			"vertex_location":    "us-central1",
			"vertex_credentials": `{"type":"service_account"}`,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got := gw.lastRequest["vertex_project"]; got != "elitea-test-project" {
			t.Errorf("gateway received vertex_project=%v", got)
		}
		if got := gw.lastRequest["vertex_location"]; got != "us-central1" {
			t.Errorf("gateway received vertex_location=%v", got)
		}
		if got := gw.lastRequest["vertex_credentials"]; got != `{"type":"service_account"}` {
			t.Errorf("gateway received vertex_credentials=%v", got)
		}
	})

	// The service-account document reaches this handler as a nested OBJECT
	// from one of the two screens that write it. Forwarding it as-is (rather
	// than through a string coercion that would silently drop it) is what
	// keeps that shape working; the gateway accepts both forms.
	t.Run("vertex_ai credentials as a nested object", func(t *testing.T) {
		gw := newFakeGateway(t, func(w http.ResponseWriter) {
			writeGatewayJSON(w, http.StatusOK, map[string]any{"success": true, "reason": "ok"})
		})
		defer gw.Close()

		checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
		if _, err := checker.Check(context.Background(), "vertex_ai", map[string]any{
			"vertex_project":     "elitea-test-project",
			"vertex_location":    "us-central1",
			"vertex_credentials": map[string]any{"type": "service_account"},
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		doc, ok := gw.lastRequest["vertex_credentials"].(map[string]any)
		if !ok {
			t.Fatalf("gateway received vertex_credentials=%#v, want the object forwarded intact", gw.lastRequest["vertex_credentials"])
		}
		if doc["type"] != "service_account" {
			t.Errorf("forwarded document = %v", doc)
		}
	})
}

// TestGatewayConnectionChecker_FailureRoundTrip proves a gateway-reported
// failure surfaces as ConnectionCheckResult{Success:false} with a safe
// message, not an error and not success.
func TestGatewayConnectionChecker_FailureRoundTrip(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter) {
		writeGatewayJSON(w, http.StatusOK, map[string]any{"success": false, "reason": "unauthorized", "detail": "the provider rejected the credential"})
	})
	defer gw.Close()

	checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
	result, err := checker.Check(context.Background(), "open_ai", map[string]any{"api_base": "https://api.openai.com/v1", "api_key": "sk-bad"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatal("expected success=false for a gateway-reported rejection")
	}
	if result.Message == "" {
		t.Error("expected a non-empty safe message")
	}
}

// TestGatewayConnectionChecker_GatewayUnreachableIsError proves a transport
// failure (connection refused) is returned as an error, not fabricated
// success or a silently-empty failure.
func TestGatewayConnectionChecker_GatewayUnreachableIsError(t *testing.T) {
	// A closed server: connecting fails immediately.
	gw := newFakeGateway(t, func(w http.ResponseWriter) { writeGatewayJSON(w, http.StatusOK, map[string]any{"success": true}) })
	gw.Close()

	checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
	_, err := checker.Check(context.Background(), "open_ai", map[string]any{"api_base": "https://api.openai.com/v1"})
	if err == nil {
		t.Fatal("expected an error when the gateway is unreachable")
	}
}

// TestGatewayConnectionChecker_NonOKStatusIsError proves an unexpected HTTP
// status from the gateway (e.g. 500, 403 from a broken identity secret) is
// reported as an error rather than silently treated as failure or success.
func TestGatewayConnectionChecker_NonOKStatusIsError(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })
	defer gw.Close()

	checker := handler.NewGatewayConnectionChecker(gw.client(t, ""))
	_, err := checker.Check(context.Background(), "open_ai", map[string]any{"api_base": "https://api.openai.com/v1"})
	if err == nil {
		t.Fatal("expected an error for a non-200 gateway response")
	}
}
