package storage

// The runtime-context `workspace` field (ADR-0034 decision 9). Assertions are
// on the RAW KEY SET for the reason runtime_context_secrets_header_test.go
// gives: both workers refuse a key they do not know, so the field must be
// absent — not empty — for a worker that did not ask.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

type fakeTurns struct {
	turn        workspaceruntime.Turn
	ok          bool
	err         error
	handedOff   bool
	calls       atomic.Int32
	handedCalls atomic.Int32
	seen        workspaceruntime.TurnClaim
}

func (f *fakeTurns) ResolveCloudTurn(_ context.Context, claim workspaceruntime.TurnClaim) (workspaceruntime.Turn, bool, error) {
	f.calls.Add(1)
	f.seen = claim
	return f.turn, f.ok, f.err
}

func (f *fakeTurns) HandedOff(context.Context, workspaceruntime.TurnClaim) (bool, error) {
	f.handedCalls.Add(1)
	return f.handedOff, nil
}

type fakePolicies struct {
	policy workspaceruntime.Policy
	err    error
	calls  atomic.Int32
}

func (f *fakePolicies) EffectiveCloudPolicy(context.Context, int64, string) (workspaceruntime.Policy, error) {
	f.calls.Add(1)
	return f.policy, f.err
}

func workspaceTokenServer(t *testing.T, turns *fakeTurns, policies *fakePolicies) *ContentServer {
	t.Helper()
	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
	service.WithWorkspaceContext(workspaceruntime.NewContextResolver(turns, policies))
	return newIndexRuntimeContextTestServer(t, service)
}

func redeem(t *testing.T, server *ContentServer, accept string) *httptest.ResponseRecorder {
	t.Helper()
	request := validRuntimeContextRequest(t, nil)
	request.ContentLength = 0
	if accept != "" {
		request.Header.Set(runtimeContextAcceptHeader, accept)
	}
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, request)
	return response
}

func cloudTurn() *fakeTurns {
	sha := "1111111111111111111111111111111111111111"
	return &fakeTurns{ok: true, turn: workspaceruntime.Turn{
		ThreadUUID: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60", Epoch: 2, CloudSHA: &sha, HasUpstream: true, UnpackedBytes: 5_555_555,
	}}
}

func enabledPolicy() *fakePolicies {
	return &fakePolicies{policy: workspaceruntime.Policy{
		Enabled: true, Shell: true, Network: true, NetworkAllowlist: []string{"PyPI.org", "github.com", "pypi.org"},
		MaxSegmentBytes: 200 << 20, MaxFileBytes: 25 << 20, MaxFiles: 20_000, MaxUnpackedBytes: 2 << 30,
	}}
}

func TestRuntimeContextServesNoWorkspaceFieldWithoutTheAccept(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	server := workspaceTokenServer(t, turns, policies)

	response := redeem(t, server, "")
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, responseKeys(t, response.Body.Bytes()), 3)
	// The cloud turn is never started for a worker that cannot read it.
	require.Zero(t, turns.calls.Load())

	response = redeem(t, server, runtimeContextFieldSecretsHeader)
	keys := responseKeys(t, response.Body.Bytes())
	require.NotContains(t, keys, "workspace")
	require.Zero(t, turns.calls.Load())
}

// A worker that cannot run a workspace turn is refused a turn of a handed-off
// thread (it would run without the files), and nothing is started.
func TestRuntimeContextRefusesAHandedOffTurnToAWorkerWithoutWorkspaceRuns(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	turns.handedOff = true
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, runtimeContextFieldSecretsHeader)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Zero(t, turns.calls.Load())
	require.Equal(t, int32(1), turns.handedCalls.Load())
}

// A thread the cloud does not own for this turn (a take back, another turn)
// refuses the redemption instead of serving a turn without its workspace.
func TestRuntimeContextRefusesATurnTheThreadIsNotTheCloudsFor(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	turns.err = workspaceruntime.ErrNotCloudTurn
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusForbidden, response.Code)
}

func TestRuntimeContextServesTheWorkspaceFieldToAWorkerThatAsks(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	server := workspaceTokenServer(t, turns, policies)

	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusOK, response.Code)
	keys := responseKeys(t, response.Body.Bytes())
	require.Contains(t, keys, "workspace")
	// The secrets value was not accepted, so it is still absent.
	require.NotContains(t, keys, "secrets_header_value")

	var field map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(keys["workspace"], &field))
	require.ElementsMatch(t, []string{"thread", "epoch", "expected_old_cloud", "limits", "network", "shell", "network_allowlist", "has_upstream", "env_secret", "rewind", "rewind_to", "egress"}, mapKeys(field))
	var egress map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(field["egress"], &egress))
	require.ElementsMatch(t, []string{"mode", "proxy_url", "token", "reason"}, mapKeys(egress))
	var decoded workspaceruntime.RuntimeContext
	require.NoError(t, json.Unmarshal(keys["workspace"], &decoded))
	require.Equal(t, uint64(2), decoded.Epoch)
	require.Equal(t, []string{"github.com", "pypi.org"}, decoded.NetworkAllowlist)
	require.Equal(t, uint64(5_555_555), decoded.Limits.UnpackedBytes)
	require.True(t, decoded.HasUpstream)
	// The policy is recomputed at this claim, for the claim's project.
	require.Equal(t, int32(1), policies.calls.Load())
	require.Equal(t, int64(42), turns.seen.ProjectID)
}

func TestRuntimeContextServesNoWorkspaceFieldForAnOrdinaryTurn(t *testing.T) {
	t.Parallel()
	turns, policies := &fakeTurns{}, enabledPolicy()
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, "workspace,secrets-header-value")
	require.Equal(t, http.StatusOK, response.Code)
	keys := responseKeys(t, response.Body.Bytes())
	require.NotContains(t, keys, "workspace")
	require.Contains(t, keys, "secrets_header_value")
	require.Zero(t, policies.calls.Load())
}

func TestRuntimeContextRefusesACloudTurnTheRecomputedPolicyDisables(t *testing.T) {
	t.Parallel()
	turns := cloudTurn()
	policies := enabledPolicy()
	policies.policy.Enabled = false
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusForbidden, response.Code)

	policies.policy.Enabled = true
	policies.err = errors.New("database down")
	response = redeem(t, server, "workspace")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestRuntimeContextNetworkOffServesNoAllowlist(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	policies.policy.Network = false
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, "workspace")
	var decoded EliteaClientTokenContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	require.NotNil(t, decoded.Workspace)
	require.False(t, decoded.Workspace.Network)
	require.Empty(t, decoded.Workspace.NetworkAllowlist)
	require.Contains(t, response.Body.String(), `"network_allowlist":[]`)
	require.Equal(t, workspaceruntime.EgressModeNone, decoded.Workspace.Egress.Mode)
	require.Empty(t, decoded.Workspace.Egress.Token)
}

func TestRuntimeContextWithoutAComposedResolverIgnoresTheAccept(t *testing.T) {
	t.Parallel()
	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
	service.WithWorkspaceContext(workspaceruntime.NewContextResolver(nil, nil))
	server := newIndexRuntimeContextTestServer(t, service)
	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, responseKeys(t, response.Body.Bytes()), 3)
}

// A regenerate of the newest cloud turn tells the worker to rewind the cloud
// ref first; a rewind with no cloud turn to rewind is refused.
func TestRuntimeContextServesTheRegenerateRewind(t *testing.T) {
	t.Parallel()
	turns, policies := cloudTurn(), enabledPolicy()
	parent := "2222222222222222222222222222222222222222"
	turns.turn.Rewind, turns.turn.RewindTo = true, &parent
	server := workspaceTokenServer(t, turns, policies)
	response := redeem(t, server, "workspace")
	require.Equal(t, http.StatusOK, response.Code)
	var decoded EliteaClientTokenContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	require.NotNil(t, decoded.Workspace)
	require.True(t, decoded.Workspace.Rewind)
	require.Equal(t, *turns.turn.CloudSHA, *decoded.Workspace.ExpectedOldCloud)
	require.Equal(t, parent, *decoded.Workspace.RewindTo)

	// A rewind with no target, a target equal to the cloud commit, or a
	// target without a rewind is refused.
	turns.turn.RewindTo = nil
	require.NotEqual(t, http.StatusOK, redeem(t, server, "workspace").Code)
	same := *turns.turn.CloudSHA
	turns.turn.RewindTo = &same
	require.NotEqual(t, http.StatusOK, redeem(t, server, "workspace").Code)
	turns.turn.Rewind, turns.turn.RewindTo = false, &parent
	require.NotEqual(t, http.StatusOK, redeem(t, server, "workspace").Code)

	turns.turn.Rewind = true
	turns.turn.CloudSHA = nil
	response = redeem(t, server, "workspace")
	require.NotEqual(t, http.StatusOK, response.Code)
}

func mapKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
