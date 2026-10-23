package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

type recoveryControlStub struct {
	polls, acks int
	claim       ContentClaim
}

func (s *recoveryControlStub) PollNodeRecovery(_ context.Context, c ContentClaim) (NodeRecoveryControl, error) {
	s.polls++
	s.claim = c
	return NodeRecoveryControl{Schema: "elitea.pipeline.node-recovery-control.v1", ExecutionID: c.ExecutionID, Generation: c.Generation, DesiredState: "SUSPENDED", Receipt: []byte(`{}`), Action: []byte(`null`)}, nil
}
func (s *recoveryControlStub) AcknowledgeNodeRecovery(_ context.Context, c ContentClaim, a NodeRecoveryAck) (NodeRecoveryAckOutcome, error) {
	s.acks++
	s.claim = c
	return NodeRecoveryAckOutcome{Schema: "elitea.pipeline.node-recovery-ack.v1", RequestID: a.RequestID}, nil
}

func TestNodeRecoveryPrivateTransportFencesAndStrictAck(t *testing.T) {
	control := &recoveryControlStub{}
	s, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		t.Fatal("ordinary content authority crossed")
		return ContentAuthorization{}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		t.Fatal("ordinary input opened")
		return nil, nil
	}), 8192)
	if err != nil {
		t.Fatal(err)
	}
	s.WithNodeRecoveryControl(control)
	scope := contentScope("0123456789abcdef0123456789abcdef", 1)
	claimed := contentClaimContext(context.Background(), nil, "claim-1", bytes.Repeat([]byte{1}, 32))
	if _, err := s.PollNodeRecovery(claimed, &runtimev1.PollNodeRecoveryRequestV1{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcknowledgeNodeRecovery(claimed, &runtimev1.AcknowledgeNodeRecoveryRequestV1{Scope: scope, AckJson: []byte(`{"request_id":"` + strings.Repeat("2", 64) + `","activation_id":"` + strings.Repeat("1", 64) + `","expected_revision":3,"receipt_sha256":"` + strings.Repeat("3", 64) + `","applied_revision":4,"continuation_receipt":null,"terminal_stop_reason":null,"failure_route_continuation":null}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = s.AcknowledgeNodeRecovery(claimed, &runtimev1.AcknowledgeNodeRecoveryRequestV1{Scope: scope, AckJson: []byte(`{"request_id":"` + strings.Repeat("2", 64) + `","activation_id":"` + strings.Repeat("1", 64) + `","expected_revision":3,"expected_revision":3,"receipt_sha256":"` + strings.Repeat("3", 64) + `","applied_revision":4,"continuation_receipt":null,"terminal_stop_reason":null,"failure_route_continuation":null}`)})
	requireContentCode(t, err, codes.InvalidArgument)
	// No verified peer: refused before the recovery store.
	_, err = s.PollNodeRecovery(metadata.NewIncomingContext(context.Background(), metadata.Pairs(grpcauth.MetadataClaimID, "claim-1", grpcauth.MetadataClaimFence, string(bytes.Repeat([]byte{1}, 32)))), &runtimev1.PollNodeRecoveryRequestV1{Scope: scope})
	requireContentCode(t, err, codes.PermissionDenied)
	// A stale (short) fence is not a claim.
	_, err = s.PollNodeRecovery(contentClaimContext(context.Background(), nil, "claim-1", []byte{1}), &runtimev1.PollNodeRecoveryRequestV1{Scope: scope})
	requireContentCode(t, err, codes.PermissionDenied)
	if control.polls != 1 || control.acks != 1 || control.claim.ClaimID != "claim-1" || control.claim.Generation != 1 || !bytes.Equal(control.claim.FenceToken, bytes.Repeat([]byte{1}, 32)) {
		t.Fatal(control)
	}
}

func TestNodeRecoveryInspectionDoesNotRedeemCredentials(t *testing.T) {
	data := []byte(`{"frozen":"fixture-definition"}`)
	digest := sha256.Sum256(data)
	s, err := NewMaterializingContentServerWithLimits(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		return ContentAuthorization{InspectionOnly: true, ResourceProjectID: "7", InputBundleID: "bundle-1", CapabilityID: "agent.execute.application.v1", SemanticRole: "agent.execution_request", ExpectedDigest: digest, ExpectedLength: int64(len(data)), ExpectedMediaType: "application/json"}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}), contentMaterializerFunc(func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error) {
		t.Fatal("recovery inspection redeemed credentials")
		return nil, nil
	}), 8192, 1)
	if err != nil {
		t.Fatal(err)
	}
	stream := newRecordingServerStream[runtimev1.GetInputResponseV1](contentClaimContext(context.Background(), nil, "claim-1", bytes.Repeat([]byte{1}, 32)))
	if err := s.GetInput(&runtimev1.GetInputRequestV1{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "v1"}, stream); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, message := range stream.sent[1:] {
		body = append(body, message.GetChunk()...)
	}
	if stream.sent[0].GetHeader() == nil || !bytes.Equal(body, data) {
		t.Fatal(stream.sent)
	}
}
