package api_test

// Router-level wiring of the ADR-0025 WP4 minimum client version: the gate
// sits in the /api/v2 group after Auth (and Maintenance), judges only a
// principal authenticated by a native access token, and the discovery
// document publishes the same policy instance's public subset.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// prefixValidator authenticates `native-*` bearers as a native principal and
// anything else as a personal access token.
type prefixValidator struct{}

func (prefixValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	user := auth.User{ID: "7", UserID: "7", TokenID: "70", Email: "u@example.test", AuthType: "token"}
	if strings.HasPrefix(token, "native-") {
		user.NativeClientID = "ai.elitea.ios"
	}
	return user, nil
}

func TestClientVersionGateIsComposedIntoTheAPIGroup(t *testing.T) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.MinClientVersion = "1.0.0"
	policy.RequireDeviceLock = true
	cfg := buildMinimalRouterConfig(t, prefixValidator{}, nil, nil)
	cfg.NativePolicy = nativepolicy.NewWithLoader(func(context.Context) (platformconfig.NativeClientPolicy, error) {
		return policy, nil
	}, nil)
	r := api.NewRouter(cfg)

	call := func(bearer, version string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/platform_settings", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		if version != "" {
			req.Header.Set(apimw.ClientVersionHeader, version)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if got := call("native-a", "0.0.1"); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("native 0.0.1 = %d %s, want 426", got.Code, got.Body.String())
	}
	if got := call("native-a", "1.0.0"); got.Code == http.StatusUpgradeRequired {
		t.Fatalf("native 1.0.0 = 426, want it served")
	}
	if got := call("pat-a", "0.0.1"); got.Code == http.StatusUpgradeRequired {
		t.Fatal("a PAT caller must be exempt (decision 13)")
	}

	disc := httptest.NewRecorder()
	r.ServeHTTP(disc, httptest.NewRequest(http.MethodGet, "/.well-known/elitea-client", nil))
	var doc struct {
		ClientPolicy map[string]any `json:"client_policy"`
	}
	if err := json.Unmarshal(disc.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery: %v (%s)", err, disc.Body.String())
	}
	if doc.ClientPolicy["min_client_version"] != "1.0.0" || doc.ClientPolicy["require_device_lock"] != true {
		t.Fatalf("discovery client_policy = %v, want the policy's public subset", doc.ClientPolicy)
	}
}
