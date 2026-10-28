package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	v2mcp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/governedmcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

type noInboundUsers struct{}

func (noInboundUsers) ResolveInboundUser(*http.Request, v2mcp.InboundToken) (auth.User, error) {
	return auth.User{}, auth.ErrPrincipalInactive
}

func governedMCPRouter(t *testing.T, governed bool) http.Handler {
	t.Helper()
	cfg := RouterConfig{
		SkillsRepo: struct{ v2skills.Repository }{}, AuthValidator: testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	}
	if governed {
		verifier, err := v2mcp.NewOAuthVerifier(v2mcp.OAuthVerifierConfig{
			Issuers: map[string]coreoidc.KeySet{"https://idp.example.com": &coreoidc.StaticKeySet{}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg.GovernedMCPInbound, err = v2mcp.NewGovernedInbound(v2mcp.GovernedInboundConfig{
			PublicOrigin: "https://elitea.example.com", Verifier: verifier, Users: noInboundUsers{},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg.GovernedMCPOutbound, err = governedmcp.New(governedmcp.Config{
			RedirectURI:       "https://elitea.example.com/api/v2/credential_consent/mcp:callback",
			ClientMetadataURL: "https://elitea.example.com" + governedmcp.ClientMetadataPath,
			Guard:             egress.New(nil),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return NewRouter(cfg)
}

const metadataPath = v2mcp.ProtectedResourceMetadataPrefix + "/app/1/mcp"

// Flag off: no metadata route, and the MCP endpoint's 401 is unchanged.
func TestGovernedMCPRoutesAreAbsentWhenNil(t *testing.T) {
	router := governedMCPRouter(t, false)
	for _, path := range []string{metadataPath, governedmcp.ClientMetadataPath} {
		if got := anonymousStatus(router, path).Code; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, got)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/app/1/mcp", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("flag off MCP 401: %d %q", recorder.Code, recorder.Header().Get("WWW-Authenticate"))
	}
}

// Governed: both documents answer anonymously, are listed as public by
// design, and the MCP endpoint's 401 names the metadata.
func TestGovernedMCPRoutesServeAnonymously(t *testing.T) {
	router := governedMCPRouter(t, true)
	meta := anonymousStatus(router, metadataPath)
	var doc map[string]any
	if meta.Code != http.StatusOK || json.Unmarshal(meta.Body.Bytes(), &doc) != nil || doc["resource"] != "https://elitea.example.com/app/1/mcp" {
		t.Fatalf("metadata = %d %s", meta.Code, meta.Body.String())
	}
	if cimd := anonymousStatus(router, governedmcp.ClientMetadataPath); cimd.Code != http.StatusOK {
		t.Fatalf("client metadata = %d", cimd.Code)
	}
	for _, pattern := range []string{
		v2mcp.ProtectedResourceMetadataPrefix + "/app/{projectID}/mcp", v2mcp.ProtectedResourceMetadataPrefix + "/app/{projectID}/mcp/*",
		governedmcp.ClientMetadataPath,
	} {
		if _, public := isPublic(pattern); !public {
			t.Errorf("%s is not listed in publicByDesign", pattern)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/app/1/mcp", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusUnauthorized ||
		recorder.Header().Get("WWW-Authenticate") != `Bearer resource_metadata="https://elitea.example.com`+metadataPath+`"` {
		t.Fatalf("governed MCP 401: %d %q", recorder.Code, recorder.Header().Get("WWW-Authenticate"))
	}
}
