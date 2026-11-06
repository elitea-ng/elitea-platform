package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

type fakeBrowserResolver struct {
	field *browserruntime.RuntimeContext
	err   error
	calls int
	claim browserruntime.Claim
}

func (f *fakeBrowserResolver) ResolveBrowserContext(_ context.Context, claim browserruntime.Claim) (*browserruntime.RuntimeContext, error) {
	f.calls++
	f.claim = claim
	return f.field, f.err
}

func browserTokenServer(t *testing.T, resolver BrowserContextResolver) *ContentServer {
	t.Helper()
	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
	service.WithBrowserContext(resolver)
	return newIndexRuntimeContextTestServer(t, service)
}

func browserField() *browserruntime.RuntimeContext {
	return &browserruntime.RuntimeContext{
		Engine: "full",
		Egress: workspaceruntime.RuntimeContextEgress{
			Mode: "proxy", ProxyURL: "http://egress.svc:3128", Token: "ert1.token",
		},
		MaxRunSeconds: 1800, AllowDomains: []string{"example.com"},
	}
}

func TestRuntimeContextServesNoBrowserFieldWithoutTheAccept(t *testing.T) {
	t.Parallel()
	resolver := &fakeBrowserResolver{field: browserField()}
	server := browserTokenServer(t, resolver)
	response := redeem(t, server, runtimeContextFieldSecretsHeader)
	require.Equal(t, codes.OK, response.Code)
	require.NotContains(t, responseKeys(t, response.Body), "browser")
	require.Zero(t, resolver.calls)
}

func TestRuntimeContextServesTheBrowserFieldToAWorkerThatAsks(t *testing.T) {
	t.Parallel()
	resolver := &fakeBrowserResolver{field: browserField()}
	server := browserTokenServer(t, resolver)
	response := redeem(t, server, browserruntime.RuntimeContextField)
	require.Equal(t, codes.OK, response.Code, string(response.Body))
	var decoded struct {
		Browser map[string]json.RawMessage `json:"browser"`
	}
	require.NoError(t, json.Unmarshal(response.Body, &decoded))
	require.Len(t, decoded.Browser, 7)
	require.JSONEq(t, `{"mode":"proxy","proxy_url":"http://egress.svc:3128","token":"ert1.token","reason":""}`,
		string(decoded.Browser["egress"]))
	require.Equal(t, 1, resolver.calls)
	require.False(t, resolver.claim.Governed)
	require.Positive(t, resolver.claim.ProjectID)
}

func TestRuntimeContextServesNoBrowserFieldForADisabledProject(t *testing.T) {
	t.Parallel()
	server := browserTokenServer(t, &fakeBrowserResolver{})
	response := redeem(t, server, browserruntime.RuntimeContextField)
	require.Equal(t, codes.OK, response.Code)
	require.NotContains(t, responseKeys(t, response.Body), "browser")
}

func TestRuntimeContextBrowserFailureFailsTheRedemption(t *testing.T) {
	t.Parallel()
	server := browserTokenServer(t, &fakeBrowserResolver{err: errors.New("canary database text")})
	response := redeem(t, server, browserruntime.RuntimeContextField)
	require.Equal(t, codes.Unavailable, response.Code)
	require.NotContains(t, string(response.Body), "canary")
	require.Equal(t, "browser_context", runtimeContextUnavailableStage(runtimeContextUnavailable(runtimeContextStageBrowser)))
}

func TestWithBrowserContextIgnoresATypedNil(t *testing.T) {
	t.Parallel()
	var resolver *browserruntime.Resolver
	server := browserTokenServer(t, resolver)
	response := redeem(t, server, browserruntime.RuntimeContextField)
	require.Equal(t, codes.OK, response.Code)
	require.NotContains(t, responseKeys(t, response.Body), "browser")
}
