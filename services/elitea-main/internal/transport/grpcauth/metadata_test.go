package grpcauth_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
)

// incoming turns what a client appended to its outgoing context into the
// server's incoming metadata, as the transport would.
func incoming(outgoing context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(outgoing)
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestBearerToken(t *testing.T) {
	token, err := grpcauth.BearerToken(incoming(grpcauth.WithBearerToken(context.Background(), "elnat_claim")))
	if err != nil || token != "elnat_claim" {
		t.Fatalf("BearerToken = %q, %v", token, err)
	}
	for name, values := range map[string][]string{
		"absent":       nil,
		"two values":   {"Bearer a", "Bearer b"},
		"basic":        {"Basic dXNlcg=="},
		"empty token":  {"Bearer "},
		"inner space":  {"Bearer a b"},
		"no scheme":    {"token"},
		"over the cap": {"Bearer " + strings.Repeat("x", 16*1024+1)},
	} {
		md := metadata.MD{}
		for _, value := range values {
			md.Append(grpcauth.MetadataAuthorization, value)
		}
		if _, err := grpcauth.BearerToken(metadata.NewIncomingContext(context.Background(), md)); err == nil {
			t.Errorf("%s: a bearer was accepted", name)
		}
	}
	md := metadata.Pairs(grpcauth.MetadataAuthorization, "bearer lower-case-scheme")
	if token, err := grpcauth.BearerToken(metadata.NewIncomingContext(context.Background(), md)); err != nil || token != "lower-case-scheme" {
		t.Fatalf("a lower-case scheme = %q, %v", token, err)
	}
}

func TestExecutionClaimRoundTrip(t *testing.T) {
	identity := &runtimev1.ExecutionIdentityV1{TenantId: "t", ExecutionId: "e-1", CommandId: "c-1", Generation: 3}
	fence := &runtimev1.ExecutionFenceV1{WorkloadSessionId: "ws", ProducerId: "p", ClaimAttempt: 1, LeaseEpoch: 2, FenceToken: []byte{1, 2, 3}}
	outgoing, err := grpcauth.WithExecutionClaim(context.Background(), identity, fence)
	if err != nil {
		t.Fatal(err)
	}
	gotIdentity, gotFence, err := grpcauth.ExecutionClaim(incoming(outgoing))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(gotIdentity, identity) || !proto.Equal(gotFence, fence) {
		t.Fatalf("claim = %v / %v", gotIdentity, gotFence)
	}

	if _, _, err := grpcauth.ExecutionClaim(context.Background()); err == nil {
		t.Error("no metadata was accepted as a claim")
	}
	// A fence without its token, and a repeated fence, are both refused.
	tokenless := proto.Clone(fence).(*runtimev1.ExecutionFenceV1)
	tokenless.FenceToken = nil
	outgoing, _ = grpcauth.WithExecutionClaim(context.Background(), identity, tokenless)
	if _, _, err := grpcauth.ExecutionClaim(incoming(outgoing)); err == nil {
		t.Error("a fence without a token was accepted")
	}
	outgoing, _ = grpcauth.WithExecutionClaim(context.Background(), identity, fence)
	outgoing, _ = grpcauth.WithExecutionClaim(outgoing, identity, fence)
	if _, _, err := grpcauth.ExecutionClaim(incoming(outgoing)); err == nil {
		t.Error("a repeated claim was accepted")
	}
	// Unknown fields are not silently dropped.
	encoded, _ := proto.Marshal(fence)
	encoded = append(encoded, 0xf8, 0x07, 0x01) // field 127, varint 1
	encodedIdentity, _ := proto.Marshal(identity)
	md := metadata.Pairs(grpcauth.MetadataExecutionIdentity, string(encodedIdentity), grpcauth.MetadataExecutionFence, string(encoded))
	if _, _, err := grpcauth.ExecutionClaim(metadata.NewIncomingContext(context.Background(), md)); err == nil {
		t.Error("a fence with an unknown field was accepted")
	}
}

func TestSignedIdentity(t *testing.T) {
	secret := []byte("identity-secret")
	for name, identity := range map[string]grpcauth.SignedIdentity{
		"v1": {ProjectID: "7", UserID: "42", TenantID: "acme"},
		"v2": {ProjectID: "7", UserID: "42", TenantID: "acme", ExecutionID: "0123abcd"},
	} {
		got, err := grpcauth.VerifiedSignedIdentity(incoming(grpcauth.WithSignedIdentity(context.Background(), secret, identity)), secret)
		if err != nil || got != identity {
			t.Errorf("%s: VerifiedSignedIdentity = %+v, %v", name, got, err)
		}
		if _, err := grpcauth.VerifiedSignedIdentity(incoming(grpcauth.WithSignedIdentity(context.Background(), secret, identity)), []byte("other")); err == nil {
			t.Errorf("%s: verified under the wrong secret", name)
		}
	}

	// The metadata carries the HTTP hop's exact bytes: a tuple signed by
	// llmproxy for the gateway verifies here unchanged.
	headers := http.Header{}
	llmproxy.SignIdentityHeaders(headers, secret, "7", "42", "", "exec-1")
	md := metadata.MD{}
	for name, values := range headers {
		md.Append(strings.ToLower(name), values...)
	}
	got, err := grpcauth.VerifiedSignedIdentity(metadata.NewIncomingContext(context.Background(), md), secret)
	if err != nil || got.ExecutionID != "exec-1" || got.ProjectID != "7" {
		t.Fatalf("an llmproxy-signed tuple = %+v, %v", got, err)
	}

	// A caller-attached execution id the signature does not cover.
	tampered := md.Copy()
	tampered.Set("x-elitea-execution-id", "someone-elses-run")
	if _, err := grpcauth.VerifiedSignedIdentity(metadata.NewIncomingContext(context.Background(), tampered), secret); err == nil {
		t.Error("a tampered execution id verified")
	}
	// A repeated project id is ambiguous.
	repeated := md.Copy()
	repeated.Append("x-elitea-project-id", "8")
	if _, err := grpcauth.VerifiedSignedIdentity(metadata.NewIncomingContext(context.Background(), repeated), secret); err == nil {
		t.Error("a repeated project id verified")
	}
	// No secret configured is never "verified".
	if _, err := grpcauth.VerifiedSignedIdentity(metadata.NewIncomingContext(context.Background(), md), nil); err == nil {
		t.Error("verified without a secret")
	}
}

func TestContentClaimRoundTrip(t *testing.T) {
	fence := []byte(strings.Repeat("f", grpcauth.ClaimFenceBytes))
	claimID, gotFence, err := grpcauth.ContentClaim(incoming(grpcauth.WithContentClaim(context.Background(), "claim-1", fence)))
	if err != nil || claimID != "claim-1" || string(gotFence) != string(fence) {
		t.Fatalf("ContentClaim = %q, %q, %v", claimID, gotFence, err)
	}
	for name, md := range map[string]metadata.MD{
		"absent":        {},
		"no fence":      metadata.Pairs(grpcauth.MetadataClaimID, "c"),
		"short fence":   metadata.Pairs(grpcauth.MetadataClaimID, "c", grpcauth.MetadataClaimFence, "short"),
		"no claim id":   metadata.Pairs(grpcauth.MetadataClaimFence, string(fence)),
		"two claim ids": metadata.Pairs(grpcauth.MetadataClaimID, "a", grpcauth.MetadataClaimID, "b", grpcauth.MetadataClaimFence, string(fence)),
		"two fences":    metadata.Pairs(grpcauth.MetadataClaimID, "a", grpcauth.MetadataClaimFence, string(fence), grpcauth.MetadataClaimFence, string(fence)),
		"newline id":    metadata.Pairs(grpcauth.MetadataClaimID, "a\nb", grpcauth.MetadataClaimFence, string(fence)),
		"over-long id":  metadata.Pairs(grpcauth.MetadataClaimID, strings.Repeat("a", 257), grpcauth.MetadataClaimFence, string(fence)),
	} {
		if _, _, err := grpcauth.ContentClaim(metadata.NewIncomingContext(context.Background(), md)); err == nil {
			t.Errorf("%s: a claim was accepted", name)
		}
	}
}
