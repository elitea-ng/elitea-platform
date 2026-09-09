package mcp

import (
	"bytes"
	"context"
	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"net/http"
	"testing"
)

func TestInternalSecretPolicyUsesSharedVaultHandler(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := context.Background()
	handler := secretsapi.NewHandler(pool, secretsapi.WithPlatformDefaultSecretPolicy(pool))
	executor := newHandlerInternalSecretExecutor(handler)
	created, err := executor.Execute(ctx, 1, 73, internalCreateSecret, map[string]any{"name": "protected", "value": "private-marker"})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("seed status=%d error=%v", created.status, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO centry.platform_config(section,key,value) VALUES ('default_secrets','default_secret_keys','["protected"]'),('default_secrets','ignore_default_secret_api','true')`)
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM centry.secrets_data WHERE id='project-1'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	listed, err := executor.Execute(ctx, 1, 73, internalListSecrets, nil)
	if err != nil || listed.status != http.StatusOK || string(listed.body) != "[]" {
		t.Fatalf("list status=%d error=%v body=%s", listed.status, err, listed.body)
	}
	changed, err := executor.Execute(ctx, 1, 73, internalUpdateSecret, map[string]any{"secret": "protected", "value": "replacement"})
	if err != nil || changed.status != http.StatusBadRequest {
		t.Fatalf("update status=%d error=%v", changed.status, err)
	}
	var after []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM centry.secrets_data WHERE id='project-1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("MCP refusal changed encrypted vault bytes")
	}
}
