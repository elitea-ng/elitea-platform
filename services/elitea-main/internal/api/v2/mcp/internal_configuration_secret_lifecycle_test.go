package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	configurationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestInternalGitHubCredentialSealsAndRotatesSecret(t *testing.T) {
	t.Setenv(secretsapi.MasterKeyEnvVar, "")
	pool := newInternalApplicationsPool(t)
	sealer, err := repos.NewCurrentSecretVaultRepository(pool, nil, repos.WithProjectVaultCreator(secretsapi.NewHandler(pool)))
	if err != nil {
		t.Fatal(err)
	}
	defer sealer.Destroy()
	executor := newHandlerInternalConfigurationExecutor(configurationsapi.NewHandler(pool, configurationsapi.WithSecretSealer(sealer)), nil)
	ctx := context.Background()
	const original = "synthetic-github-credential-before-rotation"
	const rotated = "synthetic-github-credential-after-rotation"
	created, err := executor.Execute(ctx, 1, 73, internalCreateConfiguration, map[string]any{
		"elitea_title": "github_secret_lifecycle", "label": "GitHub lifecycle", "type": "github",
		"data": map[string]any{"base_url": "https://api.github.com", "access_token": original},
	})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create: status=%d error=%v", created.status, err)
	}
	var body map[string]any
	if err := json.Unmarshal(created.body, &body); err != nil {
		t.Fatal(err)
	}
	id := scalarArgument(body["id"])
	assertSecret := func(expected string, response []byte) string {
		t.Helper()
		if bytes.Contains(response, []byte(original)) || bytes.Contains(response, []byte(rotated)) {
			t.Fatal("response exposes a credential")
		}
		var data, key, encrypted []byte
		if err := pool.QueryRow(ctx, `SELECT data FROM p_1.configuration WHERE id=$1`, id).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(original)) || bytes.Contains(data, []byte(rotated)) {
			t.Fatal("configuration stores plaintext")
		}
		var settings map[string]any
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatal(err)
		}
		reference, _ := settings["access_token"].(string)
		if !strings.HasPrefix(reference, "{{secret.") || !strings.HasSuffix(reference, "}}") {
			t.Fatal("credential reference missing")
		}
		if err := pool.QueryRow(ctx, `SELECT k.data,d.data FROM centry.secrets_key k JOIN centry.secrets_data d USING(id) WHERE k.id='project-1'`).Scan(&key, &encrypted); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encrypted, []byte(expected)) {
			t.Fatal("vault stores plaintext")
		}
		vault, err := centrysecrets.OpenUnwrapped(key, encrypted)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := vault.Lookup(strings.TrimSuffix(strings.TrimPrefix(reference, "{{secret."), "}}"))
		if err != nil || !secret.Hidden || secret.Value != expected {
			t.Fatal("vault does not resolve the current credential")
		}
		return reference
	}
	reference := assertSecret(original, created.body)
	updated, err := executor.Execute(ctx, 1, 73, internalUpdateConfiguration, map[string]any{"config_id": id, "label": "Renamed GitHub"})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("label update: status=%d error=%v", updated.status, err)
	}
	if assertSecret(original, updated.body) != reference {
		t.Fatal("label update replaces credential reference")
	}
	updated, err = executor.Execute(ctx, 1, 73, internalUpdateConfiguration, map[string]any{"config_id": id, "data": map[string]any{"base_url": "https://api.github.com", "access_token": rotated}})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("rotation: status=%d error=%v", updated.status, err)
	}
	assertSecret(rotated, updated.body)
}
