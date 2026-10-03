package secrets

// Review follow-ups on PR #1025: what the permissive vault decoder must NOT
// let through, and the typed-value cases the first round missed.
//
//   - A non-string `secrets_header_value` is no credential. Its JSON text
//     (`null`, `true`, `0`) is guessable, so `X-SECRET: null` must not lift
//     default-secret suppression.
//   - A rename that leaves the value as it was keeps its JSON type.
//   - A failure before the vault is read (begin, key mint, key insert) is a
//     save failure, not "the vault is unreadable".
//   - Every vault 500 — project and administration mode — logs its class.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// typedHeaderValues are the non-string JSON values a pylon-written vault can
// hold under any name — `value: Optional[str] = None` stores null.
var typedHeaderValues = []string{`null`, `true`, `false`, `0`, `42`, `[]`, `{}`}

func typedHeaderVault(t *testing.T, raw string, hidden bool) vaultData {
	t.Helper()
	header := fmt.Sprintf(`{%q:%s}`, SecretsHeaderValueName, raw)
	regular, hiddenDoc := header, `{}`
	if hidden {
		regular, hiddenDoc = `{}`, header
	}
	v := decodeVault(t, fmt.Sprintf(`{"secrets":%s,"hidden_secrets":%s}`, regular, hiddenDoc))
	if v.Secrets == nil {
		v.Secrets = map[string]string{}
	}
	if v.HiddenSecrets == nil {
		v.HiddenSecrets = map[string]string{}
	}
	v.Secrets["protected"] = "platform-default"
	return v
}

func suppressingPolicy(t *testing.T) defaultSecretPolicy {
	t.Helper()
	policy, err := parseDefaultSecretPolicy(platformconfig.Values{
		"default_secret_keys":       []any{"protected"},
		"ignore_default_secret_api": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestATypedSecretsHeaderValueNeverLiftsSuppression(t *testing.T) {
	t.Parallel()
	policy := suppressingPolicy(t)
	for _, raw := range typedHeaderValues {
		for _, hidden := range []bool{false, true} {
			vault := typedHeaderVault(t, raw, hidden)
			// The decoder exposes the value as its JSON text — the guess.
			text := vault.Secrets[SecretsHeaderValueName]
			if hidden {
				text = vault.HiddenSecrets[SecretsHeaderValueName]
			}
			if text != raw {
				t.Fatalf("premise: decoded %s as %q", raw, text)
			}
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("X-SECRET", text)
			if !policy.suppressed(vault, request) {
				t.Errorf("value %s hidden=%v: X-SECRET %q lifted suppression", raw, hidden, text)
			}
			if !policy.refuse(httptest.NewRecorder(), request, vault, "protected") {
				t.Errorf("value %s hidden=%v: a default name was not refused", raw, hidden)
			}
		}
	}
}

// The control: a STRING header value that happens to read "0" still works.
func TestAStringSecretsHeaderValueStillLiftsSuppression(t *testing.T) {
	t.Parallel()
	policy := suppressingPolicy(t)
	vault := decodeVault(t, `{"secrets":{"secrets_header_value":"0"},"hidden_secrets":{}}`)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-SECRET", "0")
	if policy.suppressed(vault, request) {
		t.Fatal("a matching string header value did not lift suppression")
	}
}

func TestVaultDataCredentialRefusesTypedValues(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, `{"secrets":{"a":"x","b":null},"hidden_secrets":{"c":7,"d":"y","a":"shadowed"}}`)
	for _, test := range []struct {
		name          string
		value         string
		found, usable bool
	}{
		{"a", "x", true, true},
		{"b", "null", true, false},
		{"c", "7", true, false},
		{"d", "y", true, true},
		{"missing", "", false, false},
	} {
		value, found, usable := v.credential(test.name)
		if found != test.found || usable != test.usable || (usable && value != test.value) {
			t.Errorf("credential(%q) = %q,%v,%v; want %q,%v,%v",
				test.name, value, found, usable, test.value, test.found, test.usable)
		}
	}
}

func TestVaultDataRenameKeepsTheTypeOfAnUnchangedValue(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, intModelVault)
	v.rename("default_chat_model_project_id", "renamed_id", "42")
	v.rename("flag", "renamed_flag", "true")
	v.rename("nothing", "renamed_nothing", "null")
	v.rename("ratio", "renamed_ratio", "2.5") // the value changed
	v.rename("api_key", "renamed_key", "sk-live")
	v.rename("default_chat_model_name", "default_chat_model_name", "gpt-4.1") // value-only
	regular, _ := storedRaw(t, v)
	for name, want := range map[string]string{
		"renamed_id":              `42`,
		"renamed_flag":            `true`,
		"renamed_nothing":         `null`,
		"renamed_ratio":           `"2.5"`,
		"renamed_key":             `"sk-live"`,
		"default_chat_model_name": `"gpt-4.1"`,
	} {
		if got := string(regular[name]); got != want {
			t.Errorf("stored secrets[%q] = %s, want %s", name, got, want)
		}
	}
	for _, gone := range []string{"default_chat_model_project_id", "flag", "nothing", "ratio", "api_key"} {
		if _, still := regular[gone]; still {
			t.Errorf("old name %q is still stored", gone)
		}
	}
}

// A value-only update of a typed secret to the SAME text keeps its type, and
// rename works on a vault built in memory (no raw map at all).
func TestVaultDataRenameInMemoryAndValueOnly(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, `{"secrets":{"id":42},"hidden_secrets":{}}`)
	v.rename("id", "id", "42")
	if regular, _ := storedRaw(t, v); string(regular["id"]) != `42` {
		t.Fatalf("value-only update with the same text = %s, want 42", regular["id"])
	}
	m := vaultData{Secrets: map[string]string{"a": "1"}, HiddenSecrets: map[string]string{}}
	m.rename("a", "b", "1")
	if regular, _ := storedRaw(t, m); string(regular["b"]) != `"1"` || len(regular) != 1 {
		t.Fatalf("in-memory rename = %v", regular)
	}
}

// unreachablePool is a pool whose every connection attempt is refused, so a
// mutation fails at BeginTx — before any vault row is read.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none?connect_timeout=2&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func routeRequest(t *testing.T, method, projectID, name, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", projectID)
	if name != "" {
		route.URLParams.Add("name", name)
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return request.WithContext(context.WithValue(ctx, chi.RouteCtxKey, route))
}

// captureLog swaps the default slog logger for one test. Not parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

func TestADatabaseFaultBeforeTheVaultIsReadIsASaveFailure(t *testing.T) {
	logs := captureLog(t)
	handler := NewHandler(unreachablePool(t))
	for _, test := range []struct {
		route   string
		fn      http.HandlerFunc
		method  string
		name    string
		body    string
		message string
	}{
		{"Create", handler.Create, http.MethodPost, "", `{"name":"a","value":"b"}`, "failed to save the secret"},
		{"Update", handler.Update, http.MethodPut, "a", `{"value":"b"}`, "failed to save the secret"},
		{"Delete", handler.Delete, http.MethodDelete, "a", "", "failed to delete the secret"},
		{"Hide", handler.Hide, http.MethodPost, "a", "", "failed to hide the secret"},
		{"AdminCreate", handler.AdminCreate, http.MethodPost, "a", `{"secret":"b"}`, "failed to save the secret"},
		{"AdminUpdate", handler.AdminUpdate, http.MethodPut, "a", `{"secret":{"old_name":"a","value":"b"}}`, "failed to save the secret"},
		{"AdminDelete", handler.AdminDelete, http.MethodDelete, "a", "", "failed to delete the secret"},
	} {
		logs.Reset()
		response := httptest.NewRecorder()
		test.fn(response, routeRequest(t, test.method, "9", test.name, test.body))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500 (body %s)", test.route, response.Code, response.Body)
		}
		if !strings.Contains(response.Body.String(), test.message) || strings.Contains(response.Body.String(), "unreadable") {
			t.Fatalf("%s body = %s, want %q and not \"unreadable\"", test.route, response.Body, test.message)
		}
		if !strings.Contains(logs.String(), `"class":"tx"`) {
			t.Fatalf("%s log = %s, want class tx", test.route, logs)
		}
	}
}

func TestEveryVaultFiveHundredLogsItsClassAndVault(t *testing.T) {
	logs := captureLog(t)
	cause := fmt.Errorf("%w: decrypt admin vault key: %w", errVaultKey, errors.New("fernet: bad token"))
	for _, test := range []struct {
		answer  func(http.ResponseWriter, *http.Request)
		body    string
		vaultID string
		class   string
	}{
		{func(w http.ResponseWriter, r *http.Request) { adminVaultUnreadable(w, r, cause) },
			`{"message":"global vault is unreadable"}`, "admin", "key"},
		{func(w http.ResponseWriter, r *http.Request) {
			adminVaultSaveFailed(w, r, fmt.Errorf("%w: x", errVaultWrite), "failed to save the secret")
		}, `{"message":"failed to save the secret"}`, "admin", "write"},
		{func(w http.ResponseWriter, r *http.Request) { vaultUnreadable(w, r, cause) },
			`{"error":"project vault is unreadable"}`, "project-5", "key"},
		{func(w http.ResponseWriter, r *http.Request) {
			vaultSaveFailed(w, r, fmt.Errorf("%w: x", errVaultTx), "failed to save the secret")
		}, `{"error":"failed to save the secret"}`, "project-5", "tx"},
	} {
		logs.Reset()
		response := httptest.NewRecorder()
		test.answer(response, routeRequest(t, http.MethodGet, "5", "", ""))
		if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != test.body {
			t.Fatalf("answer = %d %s, want 500 %s", response.Code, response.Body, test.body)
		}
		if strings.Contains(response.Body.String(), "fernet") {
			t.Fatal("the cause leaked into the response")
		}
		var line map[string]any
		if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
			t.Fatalf("no single log line: %q", logs)
		}
		if line["class"] != test.class || line["vault_id"] != test.vaultID || line["error"] == nil {
			t.Fatalf("log line = %v, want class %s vault_id %s with an error", line, test.class, test.vaultID)
		}
	}
}
