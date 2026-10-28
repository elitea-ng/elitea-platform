package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

type runTokenIssuerFunc func(runtoken.Binding) (string, error)

func (f runTokenIssuerFunc) Issue(binding runtoken.Binding) (string, error) {
	return f(binding)
}

// #1461: GetEliteaClientToken serves a governed-pinned run a run token
// bound to its claim, project, actor and the actor's token row; a
// legacy-pinned run, an unpinned run and a flag-off deployment get exactly
// the response they got before, the issuer present or not.
func TestGovernedRunTokenIsARunToken(t *testing.T) {
	t.Parallel()
	signer, err := runtoken.NewSigner(runtoken.DeriveKey([]byte("pat-signing-key")))
	require.NoError(t, err)
	pinned := func(p profile.Profile, err error) profile.PinReader {
		return pinReaderFunc(func(_ context.Context, executionID string) (profile.Pin, error) {
			return profile.Pin{ExecutionID: executionID, Profile: p}, err
		})
	}
	// The response every non-governed run gets today: the actor's PAT, the
	// project and the secrets header, nothing else.
	baseline := func(t *testing.T) []byte {
		t.Helper()
		server := newIndexRuntimeContextTestServer(t, newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value")))
		response := serve(server, validRuntimeContextRequest(t, runtimeContextFieldSecretsHeader))
		require.Equal(t, codes.OK, response.Code)
		return response.Body
	}(t)

	for name, test := range map[string]struct {
		pins     profile.PinReader
		issuer   bool
		governed bool
	}{
		"governed":                      {pins: pinned(profile.ProfileGoverned, nil), issuer: true, governed: true},
		"legacy pin":                    {pins: pinned(profile.ProfileLegacy, nil), issuer: true},
		"no pin row":                    {pins: pinned("", profile.ErrPinNotFound), issuer: true},
		"flag off, no reader":           {},
		"flag off, issuer not composed": {pins: pinned(profile.ProfileLegacy, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
			var issued []runtoken.Binding
			if test.issuer {
				service.WithRunTokens(runTokenIssuerFunc(func(b runtoken.Binding) (string, error) {
					issued = append(issued, b)
					return signer.Issue(b)
				}))
			}
			server := newIndexRuntimeContextTestServer(t, service)
			if test.pins != nil {
				server.WithGovernedPins(test.pins)
			}
			response := serve(server, validRuntimeContextRequest(t, runtimeContextFieldSecretsHeader))
			require.Equal(t, codes.OK, response.Code)
			if !test.governed {
				require.Equal(t, string(baseline), string(response.Body), "a non-governed run's response changed")
				require.Empty(t, issued)
				return
			}
			var body EliteaClientTokenContext
			require.NoError(t, json.Unmarshal(response.Body, &body))
			require.True(t, runtoken.HasPrefix(body.Token), "governed token = %q", body.Token)
			require.NotContains(t, string(response.Body), "header.payload.signature", "the PAT leaked")
			require.Equal(t, int64(42), body.ProjectID)
			require.Empty(t, body.SecretsHeaderValue)
			_, err := signer.Verify(body.Token, runtoken.AudienceArtifacts)
			require.NoError(t, err, "the token reaches the run project's artifacts")
			binding, err := signer.Verify(body.Token, runtoken.AudienceModelGateway)
			require.NoError(t, err)
			require.Equal(t, runtoken.Binding{
				ExecutionID: "execution-1", Generation: 1, ClaimID: "claim-1",
				FenceToken: bytes.Repeat([]byte{1}, sha256.Size),
				ProjectID:  42, UserID: 900, TokenID: 901,
			}, binding)
		})
	}
}

// An issuer that fails fails the redemption: a governed run never falls
// back to the PAT.
func TestGovernedRunTokenNeverFallsBackToThePAT(t *testing.T) {
	t.Parallel()
	service := newSecretsHeaderTokenService(t, stubProjectSecretsHeader("value"))
	service.WithRunTokens(runTokenIssuerFunc(func(runtoken.Binding) (string, error) {
		return "", errors.New("no key")
	}))
	server := newIndexRuntimeContextTestServer(t, service)
	server.WithGovernedPins(pinReaderFunc(func(_ context.Context, executionID string) (profile.Pin, error) {
		return profile.Pin{ExecutionID: executionID, Profile: profile.ProfileGoverned}, nil
	}))
	response := serve(server, validRuntimeContextRequest(t))
	require.Equal(t, codes.Unavailable, response.Code)
	require.NotContains(t, string(response.Body), "header.payload.signature")
}
