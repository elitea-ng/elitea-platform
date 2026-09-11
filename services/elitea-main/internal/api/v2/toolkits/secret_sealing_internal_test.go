package toolkits

import (
	"context"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dynamicMCPTypeSchemas struct{}

func (dynamicMCPTypeSchemas) ListToolkitTypeSchemas(context.Context) (map[string]map[string]any, error) {
	return map[string]map[string]any{
		"mcp_ado": {
			"properties": map[string]any{
				"api_token": map[string]any{
					"type": "string", "secret": true, "format": "password",
				},
				"org_name": map[string]any{"type": "string"},
			},
		},
	}, nil
}

type toolkitSecretSealerStub struct{}

func (toolkitSecretSealerStub) SealProjectHiddenSecrets(
	context.Context,
	pgx.Tx,
	int64,
	[]configurationapp.HiddenSecretMutation,
) error {
	return nil
}

func TestDynamicMCPSecretExtractionKeepsPlaintextOutOfSettings(t *testing.T) {
	handler := &Handler{
		pool:               &pgxpool.Pool{},
		dynamicTypeSchemas: dynamicMCPTypeSchemas{},
		secretSealer:       toolkitSecretSealerStub{},
	}
	sealed, mutations, status, message := handler.sealDynamicToolkitSettings(
		context.Background(),
		"mcp_ado",
		map[string]any{"api_token": "project-token", "org_name": "engineering"},
	)
	if status != 0 || message != "" || len(mutations) != 1 {
		t.Fatalf("status=%d message=%q mutations=%d", status, message, len(mutations))
	}
	if sealed["api_token"] == "project-token" || mutations[0].Value != "project-token" {
		t.Fatal("plaintext was not moved into the vault mutation")
	}
	if sealed["org_name"] != "engineering" {
		t.Fatal("non-secret parameter changed")
	}
}

func TestDynamicMCPSecretExtractionFailsClosedWithoutVault(t *testing.T) {
	handler := &Handler{dynamicTypeSchemas: dynamicMCPTypeSchemas{}}
	_, mutations, status, _ := handler.sealDynamicToolkitSettings(
		context.Background(), "mcp_ado", map[string]any{"api_token": "project-token"})
	if status != 503 || mutations != nil {
		t.Fatalf("status=%d mutations=%v", status, mutations)
	}
}
