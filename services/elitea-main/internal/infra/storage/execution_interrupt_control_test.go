package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

type interruptControlStub struct {
	fetches, acks int
	err           error
	body          []byte
	ack           []byte
}

func (s *interruptControlStub) FetchExecutionInterrupts(context.Context, ContentClaim) ([]byte, error) {
	s.fetches++
	return s.body, s.err
}

func (s *interruptControlStub) AckExecutionInterrupt(_ context.Context, _ ContentClaim, raw []byte) ([]byte, error) {
	s.acks++
	s.ack = raw
	return s.body, s.err
}

func interruptServer(t *testing.T, control ExecutionInterruptControlStore) *ContentServer {
	t.Helper()
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
	if control == nil {
		return s
	}
	return s.WithExecutionInterrupts(control)
}

var interruptScope = contentScope("0123456789abcdef0123456789abcdef", 1)

func interruptClaimed() context.Context {
	return contentClaimContext(context.Background(), nil, "claim-1", bytes.Repeat([]byte{1}, 32))
}

func callInterrupt(s *ContentServer, ctx context.Context, rpc string, ack []byte) ([]byte, error) {
	if rpc == "fetch" {
		response, err := s.FetchExecutionInterrupts(ctx, &runtimev1.FetchExecutionInterruptsRequestV1{Scope: interruptScope})
		return response.GetFetchJson(), err
	}
	response, err := s.AcknowledgeExecutionInterrupt(ctx, &runtimev1.AcknowledgeExecutionInterruptRequestV1{Scope: interruptScope, AckJson: ack})
	return response.GetOutcomeJson(), err
}

// TestExecutionInterruptRPCsMapTypedRefusalsToFixedCodes pins the contract
// v1.1 §7.1 refusal table on the gRPC plane and that no status message echoes
// ledger data.
func TestExecutionInterruptRPCsMapTypedRefusalsToFixedCodes(t *testing.T) {
	for _, spec := range []struct {
		err  error
		code codes.Code
	}{
		{nil, codes.OK},
		{ErrInterruptClaimRefused, codes.PermissionDenied},
		{ErrInterruptRequestInvalid, codes.InvalidArgument},
		{ErrInterruptNotFound, codes.NotFound},
		{ErrInterruptConflict, codes.AlreadyExists},
		{fmt.Errorf("wrapped: %w", ErrInterruptConflict), codes.AlreadyExists},
		{errors.New("database is down: secret-ish detail"), codes.Unavailable},
	} {
		for _, rpc := range []string{"fetch", "ack"} {
			control := &interruptControlStub{err: spec.err, body: []byte(`{"schema":"x"}`)}
			body, err := callInterrupt(interruptServer(t, control), interruptClaimed(), rpc, []byte(`{"interrupt_key":"k"}`))
			if spec.err == nil {
				if err != nil || string(body) != `{"schema":"x"}` {
					t.Fatalf("%s success: %q %v", rpc, body, err)
				}
				continue
			}
			requireContentCode(t, err, spec.code)
			if strings.Contains(grpcstatus.Convert(err).Message(), "secret-ish") {
				t.Fatalf("%s status leaks: %v", rpc, err)
			}
		}
	}
}

func TestExecutionInterruptRPCsBoundTheRequestBeforeTheStore(t *testing.T) {
	control := &interruptControlStub{body: []byte(`{}`)}
	s := interruptServer(t, control)
	unpeered := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(grpcauth.MetadataClaimID, "claim-1", grpcauth.MetadataClaimFence, string(bytes.Repeat([]byte{1}, 32))))
	for name, spec := range map[string]struct {
		ctx  context.Context
		rpc  string
		ack  []byte
		code codes.Code
	}{
		"ack without a body":       {interruptClaimed(), "ack", nil, codes.InvalidArgument},
		"ack over 1024 bytes":      {interruptClaimed(), "ack", bytes.Repeat([]byte(" "), MaxExecutionInterruptAckBytes+1), codes.InvalidArgument},
		"no verified peer":         {unpeered, "fetch", nil, codes.PermissionDenied},
		"a stale (short) fence":    {contentClaimContext(context.Background(), nil, "claim-1", []byte{1}), "fetch", nil, codes.PermissionDenied},
		"ack at 1024 bytes passes": {interruptClaimed(), "ack", bytes.Repeat([]byte(" "), MaxExecutionInterruptAckBytes), codes.OK},
	} {
		before := control.fetches + control.acks
		_, err := callInterrupt(s, spec.ctx, spec.rpc, spec.ack)
		if spec.code == codes.OK {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		requireContentCode(t, err, spec.code)
		if control.fetches+control.acks != before {
			t.Errorf("%s reached the store", name)
		}
	}
	if !bytes.Equal(control.ack, bytes.Repeat([]byte(" "), MaxExecutionInterruptAckBytes)) {
		t.Fatal("the store did not receive the exact ACK bytes")
	}
}

func TestExecutionInterruptRPCsAreUnimplementedWithoutAStore(t *testing.T) {
	for _, rpc := range []string{"fetch", "ack"} {
		_, err := callInterrupt(interruptServer(t, nil), interruptClaimed(), rpc, []byte(`{}`))
		requireContentCode(t, err, codes.Unimplemented)
	}
}
