package secrets

// PR #1025 review follow-ups, against a real vault (PostgreSQL + Fernet):
//
//   - a typed (non-string) `secrets_header_value` lifts no suppression on any
//     project route, and the backfill replaces it with a fresh random string;
//   - Hide applies the default-secret policy (pylon hide.py: 400 "Default
//     secrets API disabled");
//   - a rename through Update keeps an unchanged typed value's JSON type.
//
// Requires ELITEA_TEST_DATABASE_URL; skipped otherwise.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func policyHandler(t *testing.T) (*Handler, *pgxpool.Pool) {
	t.Helper()
	pool := newSecretsPool(t)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE centry.platform_config(section text, key text, value jsonb, PRIMARY KEY(section,key));
 INSERT INTO centry.platform_config VALUES ('default_secrets','default_secret_keys','["protected"]'),('default_secrets','ignore_default_secret_api','true');`); err != nil {
		t.Fatal(err)
	}
	return NewHandler(pool, WithPlatformDefaultSecretPolicy(pool)), pool
}

func callRoute(t *testing.T, fn http.HandlerFunc, method, projectID, name, body, header string) *httptest.ResponseRecorder {
	t.Helper()
	request := routeRequest(t, method, projectID, name, body)
	if header != "" {
		request.Header.Set("X-SECRET", header)
	}
	response := httptest.NewRecorder()
	fn(response, request)
	return response
}

// storedSecretsJSON decrypts a project vault and returns its raw collections.
func storedSecretsJSON(t *testing.T, handler *Handler, projectID string) storedVault {
	t.Helper()
	plaintext, _, err := handler.openVaultContents(context.Background(), handler.pool, dbKey(projectID), false)
	if err != nil {
		t.Fatalf("open the stored vault: %v", err)
	}
	var stored storedVault
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestATypedSecretsHeaderValueGrantsNothingOnTheProjectRoutes(t *testing.T) {
	handler, _ := policyHandler(t)
	ctx := context.Background()
	for i, raw := range typedHeaderValues {
		for _, hidden := range []bool{false, true} {
			projectID := fmt.Sprintf("%d", 7100+i*2+map[bool]int{false: 0, true: 1}[hidden])
			seed := typedHeaderVault(t, raw, hidden)
			seed.Secrets["ordinary"] = "o"
			if err := handler.writeVaultCtx(ctx, projectID, seed); err != nil {
				t.Fatal(err)
			}
			before := storedSecretsJSON(t, handler, projectID)
			label := fmt.Sprintf("value %s hidden=%v", raw, hidden)

			// List omits the default key.
			list := callRoute(t, handler.List, http.MethodGet, projectID, "", "", raw)
			if list.Code != http.StatusOK || strings.Contains(list.Body.String(), `"protected"`) {
				t.Fatalf("%s: List = %d %s, want the default key omitted", label, list.Code, list.Body)
			}
			// Get refuses the default key.
			if got := callRoute(t, handler.Get, http.MethodGet, projectID, "protected", "", raw); got.Code != http.StatusBadRequest ||
				strings.Contains(got.Body.String(), "platform-default") {
				t.Fatalf("%s: Get = %d %s, want 400 with no value", label, got.Code, got.Body)
			}
			// Every write of the default key is refused, and the vault is untouched.
			for _, write := range []struct {
				fn           http.HandlerFunc
				method, name string
				body         string
			}{
				{handler.Create, http.MethodPost, "", `{"name":"protected","value":"x"}`},
				{handler.Update, http.MethodPut, "protected", `{"value":"x"}`},
				{handler.Update, http.MethodPut, "ordinary", `{"name":"protected","value":"x"}`},
				{handler.Delete, http.MethodDelete, "protected", ""},
				{handler.Hide, http.MethodPost, "protected", ""},
			} {
				if got := callRoute(t, write.fn, write.method, projectID, write.name, write.body, raw); got.Code != http.StatusBadRequest {
					t.Fatalf("%s: %s %s = %d %s, want 400", label, write.method, write.name, got.Code, got.Body)
				}
			}
			if after := storedSecretsJSON(t, handler, projectID); !reflect.DeepEqual(before, after) {
				t.Fatalf("%s: a refused write changed the vault", label)
			}

			// The backfill replaces the typed value with a fresh random string.
			written, err := handler.EnsureProjectSecretsHeaderValue(ctx, projectID)
			if err != nil || !written {
				t.Fatalf("%s: EnsureProjectSecretsHeaderValue = %v, %v; want a fresh value", label, written, err)
			}
			value, err := handler.ResolveSecretValue(ctx, projectID, SecretsHeaderValueName)
			if err != nil || len(value) != 43 || value == raw {
				t.Fatalf("%s: resolved header value = %q, %v; want a 43-character random string", label, value, err)
			}
			// …and that value now lifts suppression, while the guess does not.
			if got := callRoute(t, handler.Get, http.MethodGet, projectID, "protected", "", value); got.Code != http.StatusOK ||
				!strings.Contains(got.Body.String(), "platform-default") {
				t.Fatalf("%s: Get with the fresh value = %d %s", label, got.Code, got.Body)
			}
			if got := callRoute(t, handler.Get, http.MethodGet, projectID, "protected", "", raw); got.Code != http.StatusBadRequest {
				t.Fatalf("%s: Get with the old guess = %d, want 400", label, got.Code)
			}
			// A second pass keeps it.
			if again, err := handler.EnsureProjectSecretsHeaderValue(ctx, projectID); err != nil || again {
				t.Fatalf("%s: second Ensure = %v, %v; want it kept", label, again, err)
			}
		}
	}
}

func TestHideAppliesTheDefaultSecretPolicy(t *testing.T) {
	handler, _ := policyHandler(t)
	ctx := context.Background()
	const projectID = "7300"
	original := vaultData{
		Secrets:       map[string]string{"protected": "keep", "ordinary": "o", SecretsHeaderValueName: "test-header"},
		HiddenSecrets: map[string]string{},
	}
	if err := handler.writeVaultCtx(ctx, projectID, original); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "wrong"} {
		got := callRoute(t, handler.Hide, http.MethodPost, projectID, "protected", "", header)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "Default secrets API disabled") {
			t.Fatalf("Hide with X-SECRET %q = %d %s, want 400 Default secrets API disabled", header, got.Code, got.Body)
		}
		actual, err := handler.readVaultCtx(ctx, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, original) {
			t.Fatalf("a refused Hide changed the vault: %+v", actual)
		}
	}
	// An ordinary name is hidden without the header, as before.
	if got := callRoute(t, handler.Hide, http.MethodPost, projectID, "ordinary", "", ""); got.Code != http.StatusOK {
		t.Fatalf("Hide ordinary = %d %s, want 200", got.Code, got.Body)
	}
	// The default name is hidden with the right header.
	if got := callRoute(t, handler.Hide, http.MethodPost, projectID, "protected", "", "test-header"); got.Code != http.StatusOK {
		t.Fatalf("Hide protected with the header = %d %s, want 200", got.Code, got.Body)
	}
	actual, err := handler.readVaultCtx(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.HiddenSecrets["protected"] != "keep" || actual.HiddenSecrets["ordinary"] != "o" {
		t.Fatalf("hidden secrets = %v", actual.HiddenSecrets)
	}
}

func TestARenameThroughUpdateKeepsAnUnchangedTypedValue(t *testing.T) {
	pool := newSecretsPool(t)
	handler := NewHandler(pool)
	ctx := context.Background()
	const projectID = "7400"
	seed := decodeVault(t, `{"secrets":{"model_id":42,"enabled":false,"other":7},"hidden_secrets":{}}`)
	if err := handler.writeVaultCtx(ctx, projectID, seed); err != nil {
		t.Fatal(err)
	}
	for _, rename := range []struct{ from, body string }{
		{"model_id", `{"name":"renamed_id","value":"42"}`},
		{"enabled", `{"name":"renamed_enabled","value":"false"}`},
		{"other", `{"name":"renamed_other","value":"8"}`}, // a changed value
	} {
		if got := callRoute(t, handler.Update, http.MethodPut, projectID, rename.from, rename.body, ""); got.Code != http.StatusOK {
			t.Fatalf("Update %s = %d %s", rename.from, got.Code, got.Body)
		}
	}
	stored := storedSecretsJSON(t, handler, projectID).Secrets
	for name, want := range map[string]string{"renamed_id": `42`, "renamed_enabled": `false`, "renamed_other": `"8"`} {
		if got := string(stored[name]); got != want {
			t.Errorf("stored %s = %s, want %s", name, got, want)
		}
	}
	for _, gone := range []string{"model_id", "enabled", "other"} {
		if _, still := stored[gone]; still {
			t.Errorf("old name %s still stored", gone)
		}
	}
}

func TestAnAdminRenameKeepsAnUnchangedTypedValue(t *testing.T) {
	pool := newSecretsPool(t)
	handler := NewHandler(pool)
	ctx := context.Background()
	seed := decodeVault(t, `{"secrets":{"limit":5},"hidden_secrets":{}}`)
	if err := handler.writeVaultByID(ctx, adminVaultKey, seed); err != nil {
		t.Fatal(err)
	}
	got := callRoute(t, handler.AdminUpdate, http.MethodPut, "1", "renamed_limit", `{"secret":{"old_name":"limit","value":"5"}}`, "")
	if got.Code != http.StatusOK {
		t.Fatalf("AdminUpdate = %d %s", got.Code, got.Body)
	}
	plaintext, _, err := handler.openVaultContents(ctx, pool, adminVaultKey, false)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedVault
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Secrets["renamed_limit"]) != `5` {
		t.Fatalf("stored renamed_limit = %s, want 5", stored.Secrets["renamed_limit"])
	}
}
