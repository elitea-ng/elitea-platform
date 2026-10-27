package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// redemptionAuthorizer is a content authorizer that also binds redemptions.
type redemptionAuthorizer struct {
	calls int
	err   error
}

func (a *redemptionAuthorizer) AuthorizeContent(context.Context, ContentClaim) (ContentAuthorization, error) {
	return ContentAuthorization{}, errors.New("not used")
}

func (a *redemptionAuthorizer) AuthorizeCredentialRedemption(_ context.Context, claim ContentClaim) (credentialbroker.ClaimedRun, error) {
	a.calls++
	if a.err != nil {
		return credentialbroker.ClaimedRun{}, a.err
	}
	return credentialbroker.ClaimedRun{ExecutionID: claim.ExecutionID, Generation: claim.Generation, ClaimID: claim.ClaimID, TenantProjectID: 42}, nil
}

type redeemerFunc func(context.Context, credentialbroker.RedeemRequest) (credentialbroker.Result, error)

func (f redeemerFunc) Redeem(ctx context.Context, req credentialbroker.RedeemRequest) (credentialbroker.Result, error) {
	return f(ctx, req)
}

type pinReaderFunc func(context.Context, string) (profile.Pin, error)

func (f pinReaderFunc) ReadPin(ctx context.Context, executionID string) (profile.Pin, error) {
	return f(ctx, executionID)
}

func newRedemptionServer(t *testing.T, authorizer ContentAuthorizer) *ContentServer {
	t.Helper()
	server, err := NewContentServerWithLimits(authorizer,
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("a redemption must not open input content")
			return nil, nil
		}), 1024, 1)
	require.NoError(t, err)
	return server
}

func sha256Digest(b byte) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: bytes.Repeat([]byte{b}, 32)}
}

func validRedeemRequest() *runtimev1.RedeemCredentialRequestV1 {
	return &runtimev1.RedeemCredentialRequestV1{
		Scope: contentScope("execution-1", 1),
		Invocation: &runtimev1.BoundToolInvocationV1{
			InvocationId: "0199f0a0-0000-7000-8000-000000000001", ToolCallId: "call-1", ToolkitId: 11,
			OperationId: "jira.issue.read", ResourceRef: "PROJ-1", ArgumentsDigest: sha256Digest(2),
			CredentialReferenceId: "0199f0a0-0000-7000-8000-0000000000aa",
		},
		SecurityContextDigest: sha256Digest(3),
	}
}

// Flag-off contract: with no broker composed the RPC answers Unimplemented
// and runs nothing, not even the claim parse.
func TestRedeemCredentialIsUnimplementedWithoutABroker(t *testing.T) {
	t.Parallel()
	authorizer := &redemptionAuthorizer{}
	server := newRedemptionServer(t, authorizer)
	_, err := server.RedeemCredential(validContentContext(), validRedeemRequest())
	requireContentCode(t, err, codes.Unimplemented)
	require.Zero(t, authorizer.calls)
}

// Acceptance: a claim binding failure never reaches the broker, so no
// secret store is called.
func TestRedeemCredentialBindingFailuresNeverReachTheBroker(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		ctx     context.Context
		request func() *runtimev1.RedeemCredentialRequestV1
		err     error
		code    codes.Code
	}{
		"no claim metadata":       {ctx: contentPeerContext(context.Background(), nil), request: validRedeemRequest, code: codes.Unauthenticated},
		"claim refused":           {ctx: validContentContext(), request: validRedeemRequest, err: ErrContentUnauthorized, code: codes.PermissionDenied},
		"claim store unavailable": {ctx: validContentContext(), request: validRedeemRequest, err: errors.New("db down"), code: codes.Unavailable},
		"no context digest": {ctx: validContentContext(), code: codes.InvalidArgument, request: func() *runtimev1.RedeemCredentialRequestV1 {
			r := validRedeemRequest()
			r.SecurityContextDigest = nil
			return r
		}},
		"short arguments digest": {ctx: validContentContext(), code: codes.InvalidArgument, request: func() *runtimev1.RedeemCredentialRequestV1 {
			r := validRedeemRequest()
			r.Invocation.ArgumentsDigest.Value = []byte{1}
			return r
		}},
		"no invocation": {ctx: validContentContext(), code: codes.InvalidArgument, request: func() *runtimev1.RedeemCredentialRequestV1 {
			r := validRedeemRequest()
			r.Invocation = nil
			return r
		}},
	} {
		t.Run(name, func(t *testing.T) {
			server := newRedemptionServer(t, &redemptionAuthorizer{err: test.err})
			server.WithCredentialBroker(redeemerFunc(func(context.Context, credentialbroker.RedeemRequest) (credentialbroker.Result, error) {
				t.Fatal("the broker ran for a binding failure")
				return credentialbroker.Result{}, nil
			}))
			_, err := server.RedeemCredential(test.ctx, test.request())
			requireContentCode(t, err, test.code)
		})
	}
}

func TestRedeemCredentialNeedsABindingAuthorizer(t *testing.T) {
	t.Parallel()
	server := newRedemptionServer(t, contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		return ContentAuthorization{}, nil
	}))
	server.WithCredentialBroker(redeemerFunc(func(context.Context, credentialbroker.RedeemRequest) (credentialbroker.Result, error) {
		t.Fatal("the broker ran without a claim binding")
		return credentialbroker.Result{}, nil
	}))
	_, err := server.RedeemCredential(validContentContext(), validRedeemRequest())
	requireContentCode(t, err, codes.Unimplemented)
}

func TestRedeemCredentialAnswersDenialsAndEnvelopes(t *testing.T) {
	t.Parallel()
	var got credentialbroker.RedeemRequest
	material := []byte("s3cr3t")
	result := credentialbroker.Result{}
	server := newRedemptionServer(t, &redemptionAuthorizer{})
	server.WithCredentialBroker(redeemerFunc(func(_ context.Context, req credentialbroker.RedeemRequest) (credentialbroker.Result, error) {
		got = req
		return result, nil
	}))

	result = credentialbroker.Result{Denial: &credentialbroker.Denial{
		Code: credentialbroker.CodeAuthorizationRequired, DecisionID: "d-1", AuthorizationReference: "car_x",
	}}
	response, err := server.RedeemCredential(validContentContext(), validRedeemRequest())
	require.NoError(t, err)
	require.Equal(t, "AUTHORIZATION_REQUIRED", response.GetDenial().GetCode())
	require.Equal(t, "car_x", response.GetDenial().GetAuthorizationReference())
	require.Equal(t, "d-1", response.GetDenial().GetDecisionId())
	require.Equal(t, "execution-1", got.Run.ExecutionID)
	require.Equal(t, int64(42), got.Run.TenantProjectID)
	require.Equal(t, byte(3), got.SecurityContextDigest[0])
	require.Equal(t, byte(2), got.Invocation.ArgumentsDigest[31])
	require.Equal(t, "jira.issue.read", got.Invocation.OperationID)

	notAfter := time.UnixMilli(1_800_000_000_000)
	result = credentialbroker.Result{Envelope: &credentialbroker.Envelope{
		RedemptionID: "r-1", Kind: credentialbroker.KindStatic, Material: map[string][]byte{"api_token": material},
		NotAfter: notAfter, TargetIdentity: "https://a.example.com", Obligations: []string{"audit.sync"}, DecisionID: "d-2",
	}}
	response, err = server.RedeemCredential(validContentContext(), validRedeemRequest())
	require.NoError(t, err)
	envelope := response.GetEnvelope()
	require.Equal(t, runtimev1.CredentialEnvelopeKindV1_CREDENTIAL_ENVELOPE_KIND_V1_STATIC, envelope.GetKind())
	require.Equal(t, "s3cr3t", envelope.GetMaterial()["api_token"])
	require.Equal(t, notAfter.UnixMilli(), envelope.GetNotAfterUnixMillis())
	require.Equal(t, "r-1", envelope.GetRedemptionId())
	require.Equal(t, []string{"audit.sync"}, envelope.GetObligations())
	require.Equal(t, make([]byte, len(material)), material, "the broker's bytes are zeroed after the response is built")
}

// Scope change of #1118: a governed run keeps its model-path token, but
// its response carries no secrets-header value; a legacy run and a
// deployment with no pin reader (flag off) are unchanged.
func TestGovernedRunTokenCarriesNoSecretsHeader(t *testing.T) {
	t.Parallel()
	const value = "a-real-project-value"
	pinned := func(p profile.Profile, err error) profile.PinReader {
		return pinReaderFunc(func(_ context.Context, executionID string) (profile.Pin, error) {
			require.Equal(t, "execution-1", executionID)
			return profile.Pin{ExecutionID: executionID, Profile: p}, err
		})
	}
	for name, test := range map[string]struct {
		pins       profile.PinReader
		wantHeader bool
		code       codes.Code
	}{
		"governed":            {pins: pinned(profile.ProfileGoverned, nil), code: codes.OK},
		"legacy pin":          {pins: pinned(profile.ProfileLegacy, nil), wantHeader: true, code: codes.OK},
		"no pin row":          {pins: pinned("", profile.ErrPinNotFound), wantHeader: true, code: codes.OK},
		"flag off, no reader": {wantHeader: true, code: codes.OK},
		"pin store down":      {pins: pinned("", errors.New("db down")), code: codes.Unavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := newIndexRuntimeContextTestServer(t, newSecretsHeaderTokenService(t, stubProjectSecretsHeader(value)))
			if test.pins != nil {
				server.WithGovernedPins(test.pins)
			}
			response := serve(server, validRuntimeContextRequest(t, runtimeContextFieldSecretsHeader))
			require.Equal(t, test.code, response.Code)
			if test.code != codes.OK {
				require.NotContains(t, string(response.Body), value)
				return
			}
			keys := responseKeys(t, response.Body)
			require.Contains(t, keys, "token", "the model-path token stays")
			if test.wantHeader {
				require.Contains(t, keys, "secrets_header_value")
			} else {
				require.NotContains(t, keys, "secrets_header_value")
				require.NotContains(t, string(response.Body), value)
			}
		})
	}
}
