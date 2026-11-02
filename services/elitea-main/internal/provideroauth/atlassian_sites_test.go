package provideroauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testCloudID = "11223344-a1b2-c3d4-e5f6-77889900aabb"

// mockGateway is an Atlassian API gateway that lists two sites for the
// token "good-token" and refuses any other token.
func mockGateway(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != accessibleResourcesPath || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
 {"id":"99999999-0000-0000-0000-000000000000","url":"https://other.atlassian.net","name":"other","scopes":["read:jira-work"]},
 {"id":"` + testCloudID + `","url":"https://Acme.atlassian.net","name":"acme","scopes":["read:jira-work"]}
]`))
	}))
	t.Cleanup(server.Close)
	return server
}

func gatewayClient(t *testing.T, provider, apiURL string) *Client {
	t.Helper()
	cfg := atlassianConfig("https://auth.example.com/oauth/token")
	cfg.Provider, cfg.APIURL = provider, apiURL
	cfg.RedirectURI = "https://elitea.example.com/api/v2/credential_consent/" + provider + ":callback"
	c, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// #1464: the consent resolves the target site's cloud id once and answers
// the gateway base of the provider's product.
func TestResolveAPIBaseFindsTheTargetSite(t *testing.T) {
	gateway := mockGateway(t)
	ctx := context.Background()
	jira := gatewayClient(t, "jira", gateway.URL)
	base, err := jira.ResolveAPIBase(ctx, "good-token", "https://acme.atlassian.net")
	if err != nil || base != gateway.URL+"/ex/jira/"+testCloudID {
		t.Fatalf("jira base %q err=%v", base, err)
	}
	if !jira.validAPIBase(base) {
		t.Fatal("the client refuses the base it resolved")
	}
	confluence := gatewayClient(t, "confluence", gateway.URL)
	base, err = confluence.ResolveAPIBase(ctx, "good-token", "https://acme.atlassian.net/wiki")
	if err != nil || base != gateway.URL+"/ex/confluence/"+testCloudID {
		t.Fatalf("confluence base %q err=%v", base, err)
	}
	if jira.validAPIBase(base) {
		t.Fatal("a Jira client accepts a Confluence base")
	}
}

func TestResolveAPIBaseRefusals(t *testing.T) {
	gateway := mockGateway(t)
	ctx := context.Background()
	jira := gatewayClient(t, "jira", gateway.URL)
	if _, err := jira.ResolveAPIBase(ctx, "good-token", "https://unlisted.atlassian.net"); !errors.Is(err, ErrSiteNotAccessible) {
		t.Fatalf("a site the consent does not cover: %v", err)
	}
	if _, err := jira.ResolveAPIBase(ctx, "bad-token", "https://acme.atlassian.net"); !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("a refused token: %v", err)
	}
	gateway.Close()
	if _, err := jira.ResolveAPIBase(ctx, "good-token", "https://acme.atlassian.net"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("a gateway that is down: %v", err)
	}
	for _, base := range []string{
		"https://evil.example/ex/jira/" + testCloudID,
		gateway.URL + "/ex/jira/bad_id",
		gateway.URL + "/ex/jira/" + testCloudID + "/x",
		"",
	} {
		if jira.validAPIBase(base) {
			t.Fatalf("%q is accepted", base)
		}
	}
}

// Aha! has no gateway: no base and no call.
func TestResolveAPIBaseIsEmptyWithoutAGateway(t *testing.T) {
	cfg := ProviderConfig{
		Provider: "aha", Flavor: FlavorAha, ClientID: "c", ClientSecret: "s",
		AuthorizeURL: "https://acme.aha.io/oauth/authorize", TokenURL: "https://acme.aha.io/oauth/token",
		RedirectURI: "https://elitea.example.com/api/v2/credential_consent/aha:callback",
	}
	c, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base, err := c.ResolveAPIBase(context.Background(), "t", "https://acme.aha.io"); base != "" || err != nil {
		t.Fatalf("base %q err=%v", base, err)
	}
}

// The default gateway is api.atlassian.com.
func TestAtlassianClientDefaultsToTheAtlassianGateway(t *testing.T) {
	c, err := NewClient(atlassianConfig("https://auth.example.com/oauth/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.validAPIBase("https://api.atlassian.com/ex/jira/" + testCloudID) {
		t.Fatal("the default gateway is not api.atlassian.com")
	}
}
