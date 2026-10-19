package storage

// The `egress` object of the runtime-context `workspace` field (ADR-0034
// decision 9, "Host allowlist: the egress proxy"), through the real
// redemption route and a real run-token minter.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

func egressServer(t *testing.T, policies *fakePolicies, egress workspaceruntime.Egress) *ContentServer {
	t.Helper()
	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
	service.WithWorkspaceContext(workspaceruntime.NewContextResolver(cloudTurn(), policies).WithEgress(egress, nil))
	return newIndexRuntimeContextTestServer(t, service)
}

func redeemEgress(t *testing.T, server *ContentServer) workspaceruntime.RuntimeContextEgress {
	t.Helper()
	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var decoded EliteaClientTokenContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	require.NotNil(t, decoded.Workspace)
	return decoded.Workspace.Egress
}

func testMinter(t *testing.T, now time.Time) (*egresstoken.Minter, *runtoken.KeySet) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	minter, err := egresstoken.New(priv, egresstoken.WithClock(func() time.Time { return now }))
	require.NoError(t, err)
	keys, err := runtoken.NewKeySet(pub)
	require.NoError(t, err)
	return minter, keys
}

func TestWorkspaceEgressProxyModeCarriesATokenTheProxyVerifies(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	minter, keys := testMinter(t, now)
	policies := enabledPolicy()
	policies.policy.TurnWallSeconds = 1800
	server := egressServer(t, policies, workspaceruntime.Egress{
		ProxyURL: "http://elitea-egress-proxy.platform.svc:3128", Minter: minter,
	})

	egress := redeemEgress(t, server)
	require.Equal(t, workspaceruntime.EgressModeProxy, egress.Mode)
	require.Equal(t, "http://elitea-egress-proxy.platform.svc:3128", egress.ProxyURL)
	require.Empty(t, egress.Reason)
	require.NotEmpty(t, egress.Token)

	verified, err := keys.Verify(egress.Token, now.Add(time.Minute))
	require.NoError(t, err)
	claims := verified.Claims
	require.Equal(t, "execution-1", claims.ExecutionID)
	require.Equal(t, int64(42), claims.ProjectID)
	// The allowlist is the normalised, de-duplicated, sorted policy list.
	require.Equal(t, []string{"github.com", "pypi.org"}, claims.Allowlist)
	// ttl = the turn's wall time plus the grace for hydration.
	require.Equal(t, 30*time.Minute+workspaceruntime.EgressTokenGrace, claims.ExpiresAt.Sub(claims.NotBefore))
	// The token never prints with the context.
	require.NotContains(t, egress.String(), egress.Token)
}

func TestWorkspaceEgressModesWithoutAnAllowlist(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t, time.Now())
	proxy := workspaceruntime.Egress{ProxyURL: "http://proxy:3128", Minter: minter}

	direct := enabledPolicy()
	direct.policy.NetworkAllowlist = nil
	egress := redeemEgress(t, egressServer(t, direct, proxy))
	require.Equal(t, workspaceruntime.RuntimeContextEgress{Mode: workspaceruntime.EgressModeDirect}, egress)

	off := enabledPolicy()
	off.policy.Network = false
	egress = redeemEgress(t, egressServer(t, off, proxy))
	require.Equal(t, workspaceruntime.RuntimeContextEgress{Mode: workspaceruntime.EgressModeNone}, egress)
}

// An allowlist this deployment cannot enforce is never widened to direct
// egress: the worker is told to refuse the turn, and why.
func TestWorkspaceEgressAllowlistWithoutProxyOrKeyIsUnavailable(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t, time.Now())
	for name, tc := range map[string]struct {
		egress workspaceruntime.Egress
		reason string
	}{
		"no proxy":       {workspaceruntime.Egress{Minter: minter}, workspaceruntime.EgressUnavailableProxyNotEnabled},
		"no signing key": {workspaceruntime.Egress{ProxyURL: "http://proxy:3128"}, workspaceruntime.EgressUnavailableNoSigningKey},
		"mint fails":     {workspaceruntime.Egress{ProxyURL: "http://proxy:3128", Minter: failingMinter{}}, workspaceruntime.EgressUnavailableMintFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			egress := redeemEgress(t, egressServer(t, enabledPolicy(), tc.egress))
			require.Equal(t, workspaceruntime.RuntimeContextEgress{Mode: workspaceruntime.EgressModeUnavailable, Reason: tc.reason}, egress)
		})
	}
}

type failingMinter struct{}

func (failingMinter) Mint(string, int64, []string, time.Duration) (string, error) {
	return "", errors.New("signing failed")
}
