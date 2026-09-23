package admin_test

import (
	"net/http"
	"testing"
)

func TestDefaultSecretsSectionPersistsOnlyValidPolicy(t *testing.T) {
	pool, router := newConfigEnvironment(t)
	values := map[string]any{"default_secret_keys": []any{"protected"}, "ignore_default_secret_api": true}
	response := saveSection(t, router, "default_secrets", values)
	if response.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", response.Code, response.Body)
	}
	if raw, ok := storedValueSQL(t, pool, "default_secrets", "default_secret_keys"); !ok || raw != `["protected"]` {
		t.Fatalf("stored names=%q present=%v", raw, ok)
	}
	if readSection(t, router, "default_secrets").Values["ignore_default_secret_api"] != true {
		t.Fatal("policy flag missing")
	}
	for _, invalid := range []map[string]any{
		{"default_secret_keys": []any{"bad-name"}}, {"default_secret_keys": make([]any, 101)}, {"ignore_default_secret_api": "true"},
	} {
		response = saveSection(t, router, "default_secrets", invalid)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid save status=%d", response.Code)
		}
		if raw, _ := storedValueSQL(t, pool, "default_secrets", "default_secret_keys"); raw != `["protected"]` {
			t.Fatal("invalid write changed policy")
		}
	}
}
