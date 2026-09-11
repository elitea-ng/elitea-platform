package storage

import (
	"context"
	"encoding/json"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"strings"
	"testing"
)

type toolkitTokenLoader struct {
	want      mcpoauth.TokenBinding
	revision  int64
	loads     int
	revoked   bool
	tokenType string
}

func (l *toolkitTokenLoader) Validate(_ context.Context, reference string, binding mcpoauth.TokenBinding) (mcpoauth.TokenReference, error) {
	if binding != l.want || l.revoked {
		return mcpoauth.TokenReference{}, mcpoauth.ErrTokenUnavailable
	}
	return mcpoauth.TokenReference{Reference: reference, Revision: l.revision}, nil
}
func (l *toolkitTokenLoader) Load(_ context.Context, _ string, binding mcpoauth.TokenBinding) (mcpoauth.AccessToken, error) {
	l.loads++
	if binding != l.want || l.revoked {
		return mcpoauth.AccessToken{}, mcpoauth.ErrTokenUnavailable
	}
	tokenType := l.tokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return mcpoauth.AccessToken{AccessToken: "transient-token", TokenType: tokenType}, nil
}
func TestToolkitContextRedeemsOnlyExactClaimAuthorization(t *testing.T) {
	loader := &toolkitTokenLoader{want: mcpoauth.TokenBinding{ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: "https://mcp.example.test/"}, revision: 1}
	materializer := newCurrentConfigurationsMaterializerForTest(t, &currentMaterializationUnsecreterStub{})
	materializer.toolkitTokens = loader
	source := []byte(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}},"mcp_token_reference":{"reference":"` + strings.Repeat("a", 43) + `","revision":1,"toolkit_id":19,"resource":"https://mcp.example.test/"}}`)
	authorization := ContentAuthorization{ResourceProjectID: "7", ActorID: "42", CapabilityID: executiondomain.ToolkitCallToolCapability, SemanticRole: executiondomain.ToolkitCallToolRuntimeContextRole}
	for i := 0; i < 2; i++ {
		result, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(result, &object) != nil {
			t.Fatal("invalid materialized JSON")
		}
		if _, exists := object["mcp_token_reference"]; exists || !strings.Contains(string(result), "transient-token") {
			t.Fatal("reference did not redeem transiently")
		}
		var tokens map[string]map[string]string
		if json.Unmarshal(object["mcp_tokens"], &tokens) != nil || len(tokens[loader.want.Resource]) != 1 || tokens[loader.want.Resource]["access_token"] != "transient-token" {
			t.Fatal("materialized token violates the Rust token contract")
		}
		if strings.Contains(string(source), "transient-token") {
			t.Fatal("source snapshot mutated")
		}
	}
	loader.tokenType = "DPoP"
	if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
		t.Fatal("unsupported token type accepted")
	}
	loader.tokenType = ""
	for _, foreign := range []ContentAuthorization{
		{ResourceProjectID: "8", ActorID: "42", CapabilityID: executiondomain.ToolkitCallToolCapability, SemanticRole: executiondomain.ToolkitCallToolRuntimeContextRole},
		{ResourceProjectID: "7", ActorID: "43", CapabilityID: executiondomain.ToolkitCallToolCapability, SemanticRole: executiondomain.ToolkitCallToolRuntimeContextRole},
	} {
		if _, err := materializer.MaterializeContent(context.Background(), foreign, source, 256*1024); err == nil {
			t.Fatal("foreign claim redeemed token")
		}
	}
	loader.revision = 2
	if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
		t.Fatal("changed credential revision accepted")
	}
	loader.revision = 1
	loader.revoked = true
	if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
		t.Fatal("revoked grant redeemed")
	}
	if loader.loads != 3 {
		t.Fatal("invalid authorization reached token loader")
	}
}
