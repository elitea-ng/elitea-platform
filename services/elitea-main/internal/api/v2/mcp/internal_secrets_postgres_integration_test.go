package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

func TestInternalSecretLifecycleUsesEncryptedVaultWithoutReturningPlaintext(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	handler := secretsapi.NewHandler(pool)
	executor := newHandlerInternalSecretExecutor(handler)
	ctx := context.Background()
	const (
		name         = "MCP_TEST_TOKEN"
		initialValue = "internal-mcp-private-marker-one"
		updatedValue = "internal-mcp-private-marker-two"
	)

	list, err := executor.Execute(ctx, 1, 73, internalListSecrets, nil)
	if err != nil || list.status != http.StatusOK || string(list.body) != "[]" {
		t.Fatalf("initial list: status=%d error=%v body=%s", list.status, err, list.body)
	}

	created, err := executor.Execute(ctx, 1, 73, internalCreateSecret, map[string]any{
		"name": name, "value": initialValue,
	})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create: status=%d error=%v body=%s", created.status, err, created.body)
	}
	assertSecretToolResultIsSafe(t, created.body, name, initialValue)

	listed, err := executor.Execute(ctx, 1, 73, internalListSecrets, nil)
	if err != nil || listed.status != http.StatusOK {
		t.Fatalf("list after create: status=%d error=%v body=%s", listed.status, err, listed.body)
	}
	var items []map[string]any
	if err := json.Unmarshal(listed.body, &items); err != nil || len(items) != 1 || items[0]["name"] != name {
		t.Fatalf("listed secrets = %#v, error=%v, body=%s", items, err, listed.body)
	}
	if bytes.Contains(listed.body, []byte(initialValue)) {
		t.Fatalf("list exposed plaintext: %s", listed.body)
	}

	updated, err := executor.Execute(ctx, 1, 73, internalUpdateSecret, map[string]any{
		"secret": name, "name": "IGNORED_RENAME", "value": updatedValue,
	})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("update: status=%d error=%v body=%s", updated.status, err, updated.body)
	}
	assertSecretToolResultIsSafe(t, updated.body, name, updatedValue)
	if bytes.Contains(updated.body, []byte("IGNORED_RENAME")) {
		t.Fatalf("update honored a body rename current pylon ignores: %s", updated.body)
	}

	plain, err := invokeInternalHandler(
		ctx,
		http.MethodGet,
		nil,
		nil,
		map[string]string{"projectID": "1", "name": name},
		handler.Get,
	)
	if err != nil || plain.status != http.StatusOK || !bytes.Contains(plain.body, []byte(updatedValue)) {
		t.Fatalf("direct vault verification: status=%d error=%v body=%s", plain.status, err, plain.body)
	}

	var encrypted []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM centry.secrets_data WHERE id = 'project-1'`).Scan(&encrypted); err != nil {
		t.Fatalf("read encrypted vault: %v", err)
	}
	for _, plaintext := range []string{initialValue, updatedValue} {
		if bytes.Contains(encrypted, []byte(plaintext)) {
			t.Fatalf("encrypted vault contains plaintext %q", plaintext)
		}
	}
}

func assertSecretToolResultIsSafe(t *testing.T, body []byte, name, plaintext string) {
	t.Helper()
	if bytes.Contains(body, []byte(plaintext)) {
		t.Fatalf("secret tool result exposed plaintext: %s", body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode secret result: %v; body=%s", err, body)
	}
	if response["name"] != name || response["secret_name"] != "{{secret."+name+"}}" {
		t.Fatalf("secret result = %#v", response)
	}
	if _, exposed := response["value"]; exposed {
		t.Fatalf("secret result has a value field: %#v", response)
	}
}
