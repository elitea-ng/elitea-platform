package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	for _, spec := range []struct {
		name, body, suffix string
		noTLS              bool
		status             int
	}{{"poll", "", "control", false, 200}, {"ack", `{"request_id":"` + strings.Repeat("2", 64) + `","activation_id":"` + strings.Repeat("1", 64) + `","expected_revision":3,"receipt_sha256":"` + strings.Repeat("3", 64) + `","applied_revision":4,"continuation_receipt":null,"terminal_stop_reason":null,"failure_route_continuation":null}`, "ack", false, 200}, {"duplicate", `{"request_id":"` + strings.Repeat("2", 64) + `","activation_id":"` + strings.Repeat("1", 64) + `","expected_revision":3,"expected_revision":3,"receipt_sha256":"` + strings.Repeat("3", 64) + `","applied_revision":4,"continuation_receipt":null,"terminal_stop_reason":null,"failure_route_continuation":null}`, "ack", false, 400}, {"mTLS absent", "", "control", true, 403}} {
		t.Run(spec.name, func(t *testing.T) {
			req := validContentRequest(t)
			req.Method = http.MethodPost
			req.URL.Path = "/executions/0123456789abcdef0123456789abcdef/generations/1/node-recovery/" + spec.suffix
			req.Body = io.NopCloser(strings.NewReader(spec.body))
			req.ContentLength = int64(len(spec.body))
			if spec.noTLS {
				req.TLS = nil
			}
			w := httptest.NewRecorder()
			s.Routes().ServeHTTP(w, req)
			if w.Code != spec.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
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
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, validContentRequest(t))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal(w.Code, w.Body.String())
	}
}
