package grpcauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// Metadata names on an internal gRPC hop. gRPC metadata keys are lower case;
// the identity names are the X-Elitea-* header names lower-cased, so a caller
// that holds the tuple as HTTP headers and one that holds it as metadata
// sign and verify the same bytes.
const (
	// MetadataAuthorization carries "Bearer <token>": the worker's claim
	// token, a provider callback grant — whatever bearer the HTTP hop
	// carried, unchanged.
	MetadataAuthorization = "authorization"
	// MetadataExecutionIdentity and MetadataExecutionFence carry the
	// protobuf encodings of runtimev1.ExecutionIdentityV1 and
	// runtimev1.ExecutionFenceV1, the claim an execution-bound RPC is made
	// under. Binary ("-bin") metadata, so gRPC base64-codes them on the wire.
	MetadataExecutionIdentity = "elitea-execution-identity-bin"
	MetadataExecutionFence    = "elitea-execution-fence-bin"
)

var (
	metadataProjectID   = strings.ToLower(llmproxy.HeaderProjectID)
	metadataUserID      = strings.ToLower(llmproxy.HeaderUserID)
	metadataTenantID    = strings.ToLower(llmproxy.HeaderTenantID)
	metadataExecutionID = strings.ToLower(llmproxy.HeaderExecutionID)
	metadataSignature   = strings.ToLower(llmproxy.HeaderSignature)
)

const (
	maxBearerBytes        = 16 * 1024
	maxClaimMetadataBytes = 4 * 1024
)

var (
	// ErrNoBearer: the call carries no usable bearer token.
	ErrNoBearer = errors.New("a bearer token is required")
	// ErrNoExecutionClaim: the call carries no usable execution claim.
	ErrNoExecutionClaim = errors.New("an execution claim and fence are required")
	// ErrIdentityNotSigned: the identity tuple is absent, ambiguous or its
	// signature does not verify.
	ErrIdentityNotSigned = errors.New("the signed caller identity does not verify")
)

// single returns the one value of key in the incoming metadata. Absent and
// repeated keys are both refused: a second value is never "the other one".
func single(ctx context.Context, key string) (string, bool) {
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := incoming.Get(key)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// BearerToken is the token of the call's single `authorization: Bearer <t>`
// value. The scheme name is case-insensitive.
func BearerToken(ctx context.Context) (string, error) {
	value, ok := single(ctx, MetadataAuthorization)
	if !ok {
		return "", ErrNoBearer
	}
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrNoBearer
	}
	if token == "" || len(token) > maxBearerBytes || !utf8.ValidString(token) || strings.ContainsAny(token, " \t\r\n\x00") {
		return "", ErrNoBearer
	}
	return token, nil
}

// WithBearerToken is the client half of BearerToken: it appends the token to
// the outgoing metadata of ctx.
func WithBearerToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, MetadataAuthorization, "Bearer "+token)
}

// ExecutionClaim decodes the execution identity and fence of the call. Both
// must be present exactly once, decode cleanly and carry no unknown fields.
// It does NOT authorize anything: bind the fence to the peer through
// workloadauth.PeerAuthorizer (WorkloadSessionId, ProducerId) and to the
// persisted claim as the execution-bound service requires.
func ExecutionClaim(ctx context.Context) (*runtimev1.ExecutionIdentityV1, *runtimev1.ExecutionFenceV1, error) {
	identity := &runtimev1.ExecutionIdentityV1{}
	fence := &runtimev1.ExecutionFenceV1{}
	if !decodeClaimPart(ctx, MetadataExecutionIdentity, identity) || !decodeClaimPart(ctx, MetadataExecutionFence, fence) {
		return nil, nil, ErrNoExecutionClaim
	}
	if identity.GetExecutionId() == "" || fence.GetWorkloadSessionId() == "" || fence.GetProducerId() == "" || len(fence.GetFenceToken()) == 0 {
		return nil, nil, ErrNoExecutionClaim
	}
	return identity, fence, nil
}

func decodeClaimPart(ctx context.Context, key string, message proto.Message) bool {
	raw, ok := single(ctx, key)
	if !ok || raw == "" || len(raw) > maxClaimMetadataBytes {
		return false
	}
	if err := proto.Unmarshal([]byte(raw), message); err != nil {
		return false
	}
	return len(message.ProtoReflect().GetUnknown()) == 0
}

// WithExecutionClaim is the client half of ExecutionClaim.
func WithExecutionClaim(ctx context.Context, identity *runtimev1.ExecutionIdentityV1, fence *runtimev1.ExecutionFenceV1) (context.Context, error) {
	if identity == nil || fence == nil {
		return nil, ErrNoExecutionClaim
	}
	encodedIdentity, err := proto.MarshalOptions{Deterministic: true}.Marshal(identity)
	if err != nil {
		return nil, err
	}
	encodedFence, err := proto.MarshalOptions{Deterministic: true}.Marshal(fence)
	if err != nil {
		return nil, err
	}
	return metadata.AppendToOutgoingContext(ctx,
		MetadataExecutionIdentity, string(encodedIdentity),
		MetadataExecutionFence, string(encodedFence),
	), nil
}

// Claim metadata of the runtime content plane (elitea.runtime.v1.
// RuntimeContentService, #1359 U4). They are the X-Elitea-Claim-Id and
// X-Elitea-Fence headers of the HTTPS routes that service replaced: the
// accepted claim's id and its raw 32-byte fence token. The execution and
// generation travel in the request message.
const (
	MetadataClaimID    = "elitea-claim-id"
	MetadataClaimFence = "elitea-claim-fence-bin"
	// ClaimFenceBytes is the exact length of a content-plane fence token.
	ClaimFenceBytes = 32
	maxClaimIDBytes = 256
)

// ContentClaim reads the claim id and fence of a content-plane call. Each
// must appear exactly once; the id must be a bounded single-line value and
// the fence exactly ClaimFenceBytes. It authorizes nothing: the content
// service binds both to the verified peer and the durable execution state.
func ContentClaim(ctx context.Context) (string, []byte, error) {
	claimID, ok := single(ctx, MetadataClaimID)
	if !ok || claimID == "" || len(claimID) > maxClaimIDBytes || strings.ContainsAny(claimID, "\r\n\x00") {
		return "", nil, ErrNoExecutionClaim
	}
	fence, ok := single(ctx, MetadataClaimFence)
	if !ok || len(fence) != ClaimFenceBytes {
		return "", nil, ErrNoExecutionClaim
	}
	return claimID, []byte(fence), nil
}

// WithContentClaim is the client half of ContentClaim.
func WithContentClaim(ctx context.Context, claimID string, fence []byte) context.Context {
	return metadata.AppendToOutgoingContext(ctx, MetadataClaimID, claimID, MetadataClaimFence, string(fence))
}

// SignedIdentity is the X-Elitea-* identity tuple of a call: who the request
// is for, as elitea-main resolved and signed it. Empty fields were not part of
// the tuple.
type SignedIdentity struct {
	ProjectID   string
	UserID      string
	TenantID    string
	ExecutionID string
}

// VerifiedSignedIdentity reads the tuple from the incoming metadata and
// verifies its signature under secret with llmproxy's scheme (v2 when an
// execution id is present, v1 otherwise; the HMAC format is llmproxy's and is
// not restated here). Each name may appear at most once, the signature
// exactly once.
func VerifiedSignedIdentity(ctx context.Context, secret []byte) (SignedIdentity, error) {
	if len(secret) == 0 {
		return SignedIdentity{}, ErrIdentityNotSigned
	}
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return SignedIdentity{}, ErrIdentityNotSigned
	}
	headers := http.Header{}
	for _, name := range []struct{ key, header string }{
		{metadataProjectID, llmproxy.HeaderProjectID},
		{metadataUserID, llmproxy.HeaderUserID},
		{metadataTenantID, llmproxy.HeaderTenantID},
		{metadataExecutionID, llmproxy.HeaderExecutionID},
		{metadataSignature, llmproxy.HeaderSignature},
	} {
		values := incoming.Get(name.key)
		if len(values) > 1 {
			return SignedIdentity{}, ErrIdentityNotSigned
		}
		if len(values) == 1 {
			headers.Set(name.header, values[0])
		}
	}
	if headers.Get(llmproxy.HeaderSignature) == "" || !llmproxy.VerifyIdentityHeaders(headers, secret) {
		return SignedIdentity{}, ErrIdentityNotSigned
	}
	return SignedIdentity{
		ProjectID:   headers.Get(llmproxy.HeaderProjectID),
		UserID:      headers.Get(llmproxy.HeaderUserID),
		TenantID:    headers.Get(llmproxy.HeaderTenantID),
		ExecutionID: headers.Get(llmproxy.HeaderExecutionID),
	}, nil
}

// WithSignedIdentity is the client half of VerifiedSignedIdentity: it signs
// the tuple with llmproxy.SignIdentityHeaders and appends it to the outgoing
// metadata. Like the HTTP hop, nothing is signed without a project id.
func WithSignedIdentity(ctx context.Context, secret []byte, identity SignedIdentity) context.Context {
	headers := http.Header{}
	llmproxy.SignIdentityHeaders(headers, secret, identity.ProjectID, identity.UserID, identity.TenantID, identity.ExecutionID)
	pairs := make([]string, 0, 2*len(headers))
	for name, values := range headers {
		for _, value := range values {
			pairs = append(pairs, strings.ToLower(name), value)
		}
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}
