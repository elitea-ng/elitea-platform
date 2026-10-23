package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/stretchr/testify/require"
)

type contentAuthorizerFunc func(context.Context, ContentClaim) (ContentAuthorization, error)

func (f contentAuthorizerFunc) AuthorizeContent(ctx context.Context, claim ContentClaim) (ContentAuthorization, error) {
	return f(ctx, claim)
}

type contentStoreFunc func(context.Context, string, string, string, string) (io.ReadCloser, error)

func (f contentStoreFunc) OpenContent(ctx context.Context, projectID, inputBundleID, contentID, version string) (io.ReadCloser, error) {
	return f(ctx, projectID, inputBundleID, contentID, version)
}

type contentMaterializerFunc func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error)

func (f contentMaterializerFunc) MaterializeContent(
	ctx context.Context,
	authorization ContentAuthorization,
	source []byte,
	maxBytes int64,
) ([]byte, error) {
	return f(ctx, authorization, source, maxBytes)
}

func TestContentServerRequiresVerifiedMTLSAndClaimFence(t *testing.T) {
	t.Parallel()

	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("authorizer must not be called")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("store must not be called")
			return nil, nil
		}),
		0,
	)
	require.NoError(t, err)

	// A verified peer without the claim metadata, and a call with no peer.
	for _, ctx := range []context.Context{contentPeerContext(context.Background(), nil), context.Background()} {
		_, _, err = collectInput(server, ctx, &runtimev1.GetInputRequestV1{Scope: contentScope("e1", 1), ContentId: "settings", ImmutableVersion: "v1"})
		requireContentCode(t, err, codes.Unauthenticated)
	}
	// A short fence is not a claim.
	_, _, err = collectInput(server, contentClaimContext(context.Background(), nil, "claim-1", []byte{1}), validInputRequest())
	requireContentCode(t, err, codes.Unauthenticated)
	// Generation zero is not a claim.
	_, _, err = collectInput(server, validContentContext(), &runtimev1.GetInputRequestV1{Scope: contentScope("e1", 0), ContentId: "settings", ImmutableVersion: "v1"})
	requireContentCode(t, err, codes.Unauthenticated)
}

func TestContentServerReturnsOnlyAuthorizedVerifiedBytes(t *testing.T) {
	t.Parallel()

	data := []byte{0x0a, 0x00}
	digest := sha256.Sum256(data)
	fence := bytes.Repeat([]byte{7}, sha256.Size)
	certificate := &x509.Certificate{SerialNumber: nil}

	server, err := NewContentServer(
		contentAuthorizerFunc(func(_ context.Context, claim ContentClaim) (ContentAuthorization, error) {
			require.Equal(t, "execution-1", claim.ExecutionID)
			require.EqualValues(t, 1, claim.Generation)
			require.Equal(t, "claim-1", claim.ClaimID)
			require.Equal(t, fence, claim.FenceToken)
			require.Same(t, certificate, claim.PeerCertificate)
			return ContentAuthorization{
				ResourceProjectID: "42",
				ActorID:           "17",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.AgentApplicationCapability,
				SemanticRole:      executiondomain.AgentExecutionRequestRole,
				ExpectedMediaType: executiondomain.AgentExecutionInputMediaType,
				ExpectedDigest:    digest,
				ExpectedLength:    int64(len(data)),
			}, nil
		}),
		contentStoreFunc(func(_ context.Context, projectID, inputBundleID, contentID, version string) (io.ReadCloser, error) {
			require.Equal(t, "42", projectID)
			require.Equal(t, "bundle-1", inputBundleID)
			require.Equal(t, "settings", contentID)
			require.Equal(t, "v1", version)
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		0,
	)
	require.NoError(t, err)

	header, body, err := collectInput(server, contentClaimContext(context.Background(), certificate, "claim-1", fence), validInputRequest())
	require.NoError(t, err)
	require.Equal(t, data, body)
	require.Equal(t, executiondomain.AgentExecutionInputMediaType, header.GetMediaType())
	require.EqualValues(t, len(data), header.GetLength())
	require.Equal(t, digest[:], header.GetSha256())
	require.Equal(t, header.GetSha256(), header.GetSourceSha256())
	require.Equal(t, "v1", header.GetSourceImmutableVersion())
	require.EqualValues(t, 2, header.GetSourceLength())
}

func TestContentServerStreamsLargeInputInBoundedChunks(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte{'x'}, 3*contentChunkBytes+17)
	digest := sha256.Sum256(data)
	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{
				ResourceProjectID: "42", ActorID: "17", InputBundleID: "bundle-1",
				CapabilityID: executiondomain.ConfigurationValidationCapability, SemanticRole: "configuration.settings",
				ExpectedMediaType: "application/json", ExpectedDigest: digest, ExpectedLength: int64(len(data)),
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		0,
	)
	require.NoError(t, err)

	stream := newRecordingServerStream[runtimev1.GetInputResponseV1](validContentContext())
	require.NoError(t, server.GetInput(validInputRequest(), stream))
	require.Len(t, stream.sent, 5)
	require.NotNil(t, stream.sent[0].GetHeader())
	var body []byte
	for _, message := range stream.sent[1:] {
		require.Nil(t, message.GetHeader())
		require.LessOrEqual(t, len(message.GetChunk()), contentChunkBytes)
		body = append(body, message.GetChunk()...)
	}
	require.Equal(t, data, body)

	// A cancelled stream ends the call with the send error; nothing more is sent.
	cancelled := newRecordingServerStream[runtimev1.GetInputResponseV1](validContentContext())
	cancelled.sendErr = func(sent int) error {
		if sent == 2 {
			return context.Canceled
		}
		return nil
	}
	require.ErrorIs(t, server.GetInput(validInputRequest(), cancelled), context.Canceled)
	require.Len(t, cancelled.sent, 2)
}

func TestContentServerDecodesEscapedClaimPathParts(t *testing.T) {
	t.Parallel()

	data := []byte(`{"bounded":true}`)
	digest := sha256.Sum256(data)
	immutableVersion := "sha256:" + strings.Repeat("a", 64)
	fence := bytes.Repeat([]byte{7}, sha256.Size)
	certificate := &x509.Certificate{SerialNumber: nil}

	server, err := NewContentServer(
		contentAuthorizerFunc(func(_ context.Context, claim ContentClaim) (ContentAuthorization, error) {
			require.Equal(t, "execution:one", claim.ExecutionID)
			require.Equal(t, "settings:id", claim.ContentID)
			require.Equal(t, immutableVersion, claim.ImmutableVersion)
			return ContentAuthorization{
				ResourceProjectID: "42",
				ActorID:           "17",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.IndexIngestCapability,
				SemanticRole:      "index.toolkit_configuration",
				ExpectedMediaType: "application/json",
				ExpectedDigest:    digest,
				ExpectedLength:    int64(len(data)),
			}, nil
		}),
		contentStoreFunc(func(_ context.Context, projectID, inputBundleID, contentID, version string) (io.ReadCloser, error) {
			require.Equal(t, "42", projectID)
			require.Equal(t, "bundle-1", inputBundleID)
			require.Equal(t, "settings:id", contentID)
			require.Equal(t, immutableVersion, version)
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		0,
	)
	require.NoError(t, err)

	header, body, err := collectInput(server, contentClaimContext(context.Background(), certificate, "claim-1", fence), &runtimev1.GetInputRequestV1{
		Scope:            contentScope("execution:one", 1),
		ContentId:        "settings:id",
		ImmutableVersion: immutableVersion,
	})
	require.NoError(t, err)
	require.Equal(t, data, body)
	require.Equal(t, immutableVersion, header.GetSourceImmutableVersion())
}

func TestContentClaimPartsRejectNULOrUnboundedValues(t *testing.T) {
	t.Parallel()

	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("a malformed claim part must not reach authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("store must not be called")
			return nil, nil
		}),
		0,
	)
	require.NoError(t, err)
	for _, request := range []*runtimev1.GetInputRequestV1{
		{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "sha256\x00value"},
		{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: ""},
		{Scope: contentScope("execution\x00one", 1), ContentId: "settings", ImmutableVersion: "v1"},
		{Scope: contentScope("execution-1", 1), ContentId: strings.Repeat("a", 4096), ImmutableVersion: "v1"},
	} {
		_, _, err := collectInput(server, validContentContext(), request)
		requireContentCode(t, err, codes.Unauthenticated)
	}
}

func TestContentServerDoesNotReleaseWrongDigest(t *testing.T) {
	t.Parallel()

	data := []byte(`{}`)
	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{
				ResourceProjectID: "42",
				ActorID:           "17",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.ConfigurationValidationCapability,
				SemanticRole:      "configuration.settings",
				ExpectedMediaType: "application/json",
				ExpectedDigest:    sha256.Sum256([]byte("different")),
				ExpectedLength:    int64(len(data)),
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		0,
	)
	require.NoError(t, err)

	stream := newRecordingServerStream[runtimev1.GetInputResponseV1](validContentContext())
	requireContentCode(t, server.GetInput(validInputRequest(), stream), codes.Internal)
	require.Empty(t, stream.sent, "no header or byte may precede the digest check")
}

func TestContentServerHidesAuthorizationFailure(t *testing.T) {
	t.Parallel()

	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{}, errors.New("sensitive reason")
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("store must not be called")
			return nil, nil
		}),
		0,
	)
	require.NoError(t, err)

	_, _, err = collectInput(server, validContentContext(), validInputRequest())
	requireContentCode(t, err, codes.PermissionDenied)
	require.NotContains(t, err.Error(), "sensitive")
}

func TestContentServerRejectsOverLimitBeforeOpeningContent(t *testing.T) {
	t.Parallel()

	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{
				ResourceProjectID: "42",
				ActorID:           "17",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.ConfigurationValidationCapability,
				SemanticRole:      "configuration.settings",
				ExpectedMediaType: "application/json",
				ExpectedDigest:    sha256.Sum256([]byte("bounded")),
				ExpectedLength:    9,
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("over-limit content must not be opened")
			return nil, nil
		}),
		8,
	)
	require.NoError(t, err)

	_, _, err = collectInput(server, validContentContext(), validInputRequest())
	requireContentCode(t, err, codes.ResourceExhausted)
}

func TestContentServerRejectsMultiplexedWorkBeyondRequestCapacity(t *testing.T) {
	t.Parallel()

	server, err := NewContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("saturated content request must not reach PostgreSQL authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("saturated content request must not open content")
			return nil, nil
		}),
		defaultMaxInputContentBytes,
		1,
	)
	require.NoError(t, err)
	server.requests <- struct{}{}
	defer func() { <-server.requests }()

	_, _, err = collectInput(server, validContentContext(), validInputRequest())
	requireContentCode(t, err, codes.Unavailable)
}

func TestMaterializingRuntimeContentServerComposesBothBoundedPaths(t *testing.T) {
	t.Parallel()

	source := []byte(`{"private":true,"elitea_title":"source"}`)
	expanded := []byte(`{"configuration_type":"confluence","private":true}`)
	digest := sha256.Sum256(source)
	materializerCalls := 0
	materializer := contentMaterializerFunc(func(
		_ context.Context,
		authorization ContentAuthorization,
		gotSource []byte,
		maxBytes int64,
	) ([]byte, error) {
		materializerCalls++
		require.Equal(t, "17", authorization.ActorID)
		require.Equal(t, source, gotSource)
		require.EqualValues(t, 1024, maxBytes)
		return append([]byte(nil), expanded...), nil
	})
	runtimeToken := &EliteaClientTokenService{}
	server, err := NewMaterializingRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{
				ResourceProjectID: "42",
				ActorID:           "17",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.IndexIngestCapability,
				SemanticRole:      "index.toolkit_configuration",
				ExpectedMediaType: "application/json",
				ExpectedDigest:    digest,
				ExpectedLength:    int64(len(source)),
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(source)), nil
		}),
		materializer,
		runtimeToken,
		1024,
		1,
	)
	require.NoError(t, err)
	require.NotNil(t, server.materializer)
	require.Same(t, runtimeToken, server.runtimeToken)
	require.Equal(t, 1, cap(server.requests))

	header, body, err := collectInput(server, validContentContext(), validInputRequest())
	require.NoError(t, err)
	require.Equal(t, expanded, body)
	expandedDigest := sha256.Sum256(expanded)
	require.Equal(t, expandedDigest[:], header.GetSha256())
	require.Equal(t, digest[:], header.GetSourceSha256())
	require.EqualValues(t, len(source), header.GetSourceLength())
	require.Equal(t, 1, materializerCalls)

	tokenRequest := &runtimev1.GetEliteaClientTokenRequestV1{Scope: contentScope("execution-1", 1)}
	_, err = server.GetEliteaClientToken(contentPeerContext(context.Background(), nil), tokenRequest)
	requireContentCode(t, err, codes.Unauthenticated)

	server.requests <- struct{}{}
	_, _, err = collectInput(server, validContentContext(), validInputRequest())
	requireContentCode(t, err, codes.Unavailable)
	_, err = server.GetEliteaClientToken(validContentContext(), tokenRequest)
	requireContentCode(t, err, codes.Unavailable)
	<-server.requests
}

func TestMaterializingRuntimeContentServerRequiresBothServices(t *testing.T) {
	t.Parallel()

	authorizer := contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		return ContentAuthorization{}, nil
	})
	store := contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		return nil, nil
	})
	materializer := contentMaterializerFunc(func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error) {
		return nil, nil
	})
	runtimeToken := &EliteaClientTokenService{}

	_, err := NewMaterializingRuntimeContentServerWithLimits(authorizer, store, nil, runtimeToken, 1024, 1)
	require.EqualError(t, err, "content materializer is required")
	_, err = NewMaterializingRuntimeContentServerWithLimits(authorizer, store, materializer, nil, 1024, 1)
	require.EqualError(t, err, "runtime context is required")

	materializing, err := NewMaterializingContentServerWithLimits(authorizer, store, materializer, 1024, 1)
	require.NoError(t, err)
	require.NotNil(t, materializing.materializer)
	require.Nil(t, materializing.runtimeToken)
	runtime, err := NewRuntimeContentServerWithLimits(authorizer, store, runtimeToken, 1024, 1)
	require.NoError(t, err)
	require.Nil(t, runtime.materializer)
	require.Same(t, runtimeToken, runtime.runtimeToken)
}

func TestMaterializingContentFailureLogsStatusWithoutPrivateValues(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		cause error
		code  codes.Code
		stage string
	}{
		{"rejected", ErrContentRejected, codes.FailedPrecondition, "unknown"},
		{"unavailable", runtimeContextUnavailable(runtimeContextStageActorPATIssuance), codes.Unavailable, runtimeContextStageActorPATIssuance},
	} {
		t.Run(test.name, func(t *testing.T) {
			const privateValue = "private-input-and-credential-canary"
			source := []byte(privateValue)
			server, err := NewMaterializingContentServerWithLimits(
				contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
					return ContentAuthorization{
						ResourceProjectID: "42",
						ActorID:           "17",
						InputBundleID:     "bundle-1",
						CapabilityID:      executiondomain.AgentApplicationCapability,
						SemanticRole:      executiondomain.AgentExecutionRequestRole,
						ExpectedMediaType: executiondomain.AgentExecutionInputMediaType,
						ExpectedDigest:    sha256.Sum256(source),
						ExpectedLength:    int64(len(source)),
					}, nil
				}),
				contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(source)), nil
				}),
				contentMaterializerFunc(func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error) {
					return nil, fmt.Errorf("%s: %w", privateValue, test.cause)
				}),
				1024, 1,
			)
			require.NoError(t, err)
			var logs bytes.Buffer
			server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			_, _, err = collectInput(server, validContentContext(), validInputRequest())

			requireContentCode(t, err, test.code)
			require.NotContains(t, err.Error(), privateValue)
			require.NotContains(t, logs.String(), privateValue)
			var record map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
			require.Equal(t, "ERROR", record["level"])
			require.Equal(t, "execution-1", record["execution_id"])
			require.Equal(t, float64(1), record["generation"])
			require.Equal(t, "materialization", record["boundary"])
			require.Equal(t, test.code.String(), record["code"])
			require.Equal(t, test.stage, record["stage"])
		})
	}
}

func TestContentServerLeavesUncomposedRPCsUnimplemented(t *testing.T) {
	t.Parallel()

	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("an uncomposed RPC must not authorize content")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("an uncomposed RPC must not open content")
			return nil, nil
		}),
		0,
	)
	require.NoError(t, err)
	ctx := validContentContext()
	scope := contentScope("execution-1", 1)

	_, err = server.GetEliteaClientToken(ctx, &runtimev1.GetEliteaClientTokenRequestV1{Scope: scope})
	requireContentCode(t, err, codes.Unimplemented)
	_, err = server.GetApplicationVersion(ctx, &runtimev1.GetApplicationVersionRequestV1{Scope: scope, ApplicationId: 1, VersionId: 1})
	requireContentCode(t, err, codes.Unimplemented)
	_, err = server.GetAttachmentObject(ctx, &runtimev1.GetAttachmentObjectRequestV1{Scope: scope, Bucket: "b", Name: "n"})
	requireContentCode(t, err, codes.Unimplemented)
	_, err = server.WriteSkill(ctx, &runtimev1.WriteSkillRequestV1{Scope: scope, RequestJson: []byte(`{}`)})
	requireContentCode(t, err, codes.Unimplemented)
	_, err = server.ListArtifacts(ctx, &runtimev1.ListArtifactsRequestV1{Scope: scope, RequestJson: []byte(`{}`)})
	requireContentCode(t, err, codes.Unimplemented)
	_, err = server.PutToolkitDiscoveryResult(ctx, &runtimev1.PutToolkitDiscoveryResultRequestV1{Scope: scope, ContentId: "settings", ImmutableVersion: "v1", ResultJson: []byte(`{}`)})
	requireContentCode(t, err, codes.Unimplemented)
}

// collectInput runs one GetInput call and reassembles its header and chunks,
// refusing a stream that is not exactly one header followed by chunks.
func collectInput(server *ContentServer, ctx context.Context, request *runtimev1.GetInputRequestV1) (*runtimev1.InputContentHeaderV1, []byte, error) {
	stream := newRecordingServerStream[runtimev1.GetInputResponseV1](ctx)
	if err := server.GetInput(request, stream); err != nil {
		return nil, nil, err
	}
	if len(stream.sent) == 0 || stream.sent[0].GetHeader() == nil {
		return nil, nil, errors.New("stream did not start with a header")
	}
	var body []byte
	for _, message := range stream.sent[1:] {
		if message.GetHeader() != nil {
			return nil, nil, errors.New("second header")
		}
		body = append(body, message.GetChunk()...)
	}
	return stream.sent[0].GetHeader(), body, nil
}

func validInputRequest() *runtimev1.GetInputRequestV1 {
	return &runtimev1.GetInputRequestV1{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "v1"}
}

func validContentContext() context.Context {
	return contentClaimContext(context.Background(), nil, "claim-1", bytes.Repeat([]byte{1}, sha256.Size))
}

func TestWorkloadIdentityRequiresOneUnambiguousVerifiedSAN(t *testing.T) {
	t.Parallel()

	valid, err := url.Parse("spiffe://elitea.internal/runtime/worker-1")
	require.NoError(t, err)
	identity, err := workloadIdentity(&x509.Certificate{URIs: []*url.URL{valid}})
	require.NoError(t, err)
	require.Equal(t, valid.String(), identity)
	dnsIdentity, err := workloadIdentity(&x509.Certificate{DNSNames: []string{"Worker.Runtime.Example"}})
	require.NoError(t, err)
	require.Equal(t, "dns:worker.runtime.example", dnsIdentity)

	for _, certificate := range []*x509.Certificate{
		nil,
		{},
		{DNSNames: []string{"*.runtime.example"}},
		{URIs: []*url.URL{valid}, DNSNames: []string{"worker.runtime.example"}},
		{URIs: []*url.URL{valid, valid}},
		{URIs: []*url.URL{{Scheme: "https", Host: "elitea.internal", Path: "/runtime/worker-1"}}},
		{URIs: []*url.URL{{Scheme: "spiffe", Host: "", Path: "/runtime/worker-1"}}},
		{URIs: []*url.URL{{Scheme: "spiffe", Host: "elitea.internal", Path: ""}}},
	} {
		_, err := workloadIdentity(certificate)
		require.ErrorIs(t, err, ErrContentUnauthorized)
	}
}
