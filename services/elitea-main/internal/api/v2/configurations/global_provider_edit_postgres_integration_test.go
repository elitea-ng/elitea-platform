package configurations_test

// The admin provider dialog's EDIT, end to end against a real PostgreSQL.
//
// The dialog never reads a secret back, so on an edit an untouched secret
// field is OMITTED from `data` ("blank = keep"). The compatibility Update's
// partial semantics are per top-level column: a present `data` is written
// whole. Through /admin/gateway/providers that erased the sealed
// `{{secret.NAME}}` reference of every key the operator did not retype — an
// endpoint fix silently turned a working platform credential into one with no
// key. These tests read the ROW, not the status code: the 200 was never the
// symptom.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func adminProviderDo(
	t *testing.T, router chi.Router, method, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// createAdminProvider publishes one provider through the admin surface and
// returns its id.
func createAdminProvider(t *testing.T, router chi.Router, body map[string]any) int {
	t.Helper()
	recorder := adminProviderDo(t, router, http.MethodPost, "/api/v2/admin/gateway/providers", body)
	if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
		t.Fatalf("POST /admin/gateway/providers = %d: %s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created provider: %v", err)
	}
	id, ok := created["id"].(float64)
	if !ok {
		t.Fatalf("the created provider carries no id: %s", recorder.Body.String())
	}
	return int(id)
}

func storedProviderData(t *testing.T, pool *pgxpool.Pool, id int) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT data FROM p_%d.configuration WHERE id = $1`, storedCheckProject), id).Scan(&raw); err != nil {
		t.Fatalf("read the stored provider: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode the stored provider data %q: %v", raw, err)
	}
	return data
}

func sealedReference(t *testing.T, data map[string]any, field string) string {
	t.Helper()
	value, _ := data[field].(string)
	if !strings.HasPrefix(value, "{{secret.") {
		t.Fatalf("data.%s = %#v, want a sealed {{secret.NAME}} reference; row: %v", field, data[field], data)
	}
	return value
}

// TestAnAdminProviderEditThatOmitsTheKeyKeepsTheSealedReference is the
// dialog's own edit body: name, type, and `data` without the untouched key.
func TestAnAdminProviderEditThatOmitsTheKeyKeepsTheSealedReference(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})

	id := createAdminProvider(t, router, map[string]any{
		"elitea_title": "autotest_platform_dial",
		"type":         "ai_dial",
		"data": map[string]any{
			"api_key":     "dial-key-only-the-vault-holds",
			"api_base":    "https://dial.autotest.invalid",
			"api_version": "2024-02-01",
			"auth_mode":   "key",
		},
	})
	before := sealedReference(t, storedProviderData(t, pool, id), "api_key")

	recorder := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"elitea_title": "autotest_platform_dial",
			"type":         "ai_dial",
			"data": map[string]any{
				"api_base":    "https://dial-moved.autotest.invalid",
				"api_version": "2024-02-01",
				"auth_mode":   "key",
			},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}

	after := storedProviderData(t, pool, id)
	if got := after["api_key"]; got != before {
		t.Fatalf("the edit erased the stored key: data.api_key = %#v, want %q; row: %v", got, before, after)
	}
	if got := after["api_base"]; got != "https://dial-moved.autotest.invalid" {
		t.Errorf("data.api_base = %#v, want the edited endpoint", got)
	}
	if strings.Contains(fmt.Sprint(after), "dial-key-only-the-vault-holds") {
		t.Errorf("the row holds the key in clear text: %v", after)
	}
}

// TestAnAdminProviderEditWithANewKeyReplacesIt — the merge must not make a
// retyped key impossible.
func TestAnAdminProviderEditWithANewKeyReplacesIt(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})

	id := createAdminProvider(t, router, map[string]any{
		"elitea_title": "autotest_platform_openai",
		"type":         "open_ai",
		"data":         map[string]any{"api_key": "sk-old", "api_base": "https://api.openai.com/v1"},
	})
	before := sealedReference(t, storedProviderData(t, pool, id), "api_key")

	recorder := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"type": "open_ai",
			"data": map[string]any{"api_key": "sk-new", "api_base": "https://api.openai.com/v1"},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	after := storedProviderData(t, pool, id)
	if got := sealedReference(t, after, "api_key"); got == before {
		t.Fatalf("data.api_key still names the OLD secret %q after a new key was typed", got)
	}
}

// TestAnAdminProviderSwitchToWorkloadIdentityStillClearsTheStoredKey — the
// merge brings the stored reference back into `data`, and the keyless rule
// must still remove it: a keyless row holding a key is refused by the gateway.
func TestAnAdminProviderSwitchToWorkloadIdentityStillClearsTheStoredKey(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})

	id := createAdminProvider(t, router, map[string]any{
		"elitea_title": "autotest_platform_dial_wi",
		"type":         "ai_dial",
		"data": map[string]any{
			"api_key":   "dial-key-to-drop",
			"api_base":  "https://dial.autotest.invalid",
			"auth_mode": "key",
		},
	})
	sealedReference(t, storedProviderData(t, pool, id), "api_key")

	recorder := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"type": "ai_dial",
			"data": map[string]any{
				"api_base":  "https://dial.autotest.invalid",
				"auth_mode": "workload_identity",
			},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	after := storedProviderData(t, pool, id)
	if _, present := after["api_key"]; present {
		t.Fatalf("a keyless row kept data.api_key = %#v", after["api_key"])
	}
	if after["auth_mode"] != "workload_identity" {
		t.Fatalf("data.auth_mode = %#v, want workload_identity", after["auth_mode"])
	}
}

// TestAnAdminProviderSwitchToWorkloadIdentityClearsAStoredKeyOfAnyForm — the
// merge brings back whatever the row holds, and a legacy row can hold a key in
// clear text, as a legacy reference, or as a JSON object. None of them is a
// key the operator typed in this request, so the switch removes them all
// instead of refusing with a field the dialog does not show. A key the
// operator DID type is still refused.
func TestAnAdminProviderSwitchToWorkloadIdentityClearsAStoredKeyOfAnyForm(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})

	id := createAdminProvider(t, router, map[string]any{
		"elitea_title": "autotest_platform_dial_legacy",
		"type":         "ai_dial",
		"data": map[string]any{
			"api_key":   "dial-key",
			"api_base":  "https://dial.autotest.invalid",
			"auth_mode": "key",
		},
	})
	// A legacy row: plaintext api_key, an object-shaped api_token.
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE p_%d.configuration
		    SET data = data || '{"api_key":"legacy-plaintext-key","api_token":{"value":"x"}}'::jsonb
		  WHERE id = $1`, storedCheckProject), id); err != nil {
		t.Fatalf("seed the legacy key: %v", err)
	}

	typed := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"type": "ai_dial",
			"data": map[string]any{
				"api_base": "https://dial.autotest.invalid", "auth_mode": "workload_identity",
				"api_key": "a-key-typed-now",
			},
		})
	if typed.Code != http.StatusBadRequest || !strings.Contains(typed.Body.String(), "data.api_key") {
		t.Fatalf("PUT with a typed key = %d: %s, want the 400 naming data.api_key", typed.Code, typed.Body.String())
	}

	recorder := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"type": "ai_dial",
			"data": map[string]any{"api_base": "https://dial.autotest.invalid", "auth_mode": "workload_identity"},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	after := storedProviderData(t, pool, id)
	for _, field := range []string{"api_key", "api_token"} {
		if _, present := after[field]; present {
			t.Errorf("a keyless row kept data.%s = %#v", field, after[field])
		}
	}
}

// TestAnAdminProviderSwitchBackToKeyDropsTheTokenAudience — token_audience
// means something only in the keyless mode. The dialog does not send it in the
// key mode, and the merge must not bring the stale value back.
func TestAnAdminProviderSwitchBackToKeyDropsTheTokenAudience(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})

	id := createAdminProvider(t, router, map[string]any{
		"elitea_title": "autotest_platform_dial_audience",
		"type":         "ai_dial",
		"data": map[string]any{
			"api_base": "https://dial.autotest.invalid", "auth_mode": "workload_identity",
			"token_audience": "stale-audience",
		},
	})
	recorder := adminProviderDo(t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/admin/gateway/providers/%d", id), map[string]any{
			"type": "ai_dial",
			"data": map[string]any{
				"api_base": "https://dial.autotest.invalid", "auth_mode": "key", "api_key": "dial-key",
			},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	after := storedProviderData(t, pool, id)
	if _, present := after["token_audience"]; present {
		t.Fatalf("a key-mode row kept data.token_audience = %#v", after["token_audience"])
	}
	sealedReference(t, after, "api_key")
}
