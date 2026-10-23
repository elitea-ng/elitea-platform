package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
)

type recoveryResultStub struct {
	data  []byte
	calls int
	claim ContentClaim
}

func (s *recoveryResultStub) ReadNodeRecoveryResult(_ context.Context, c ContentClaim, _ string, _ string) ([]byte, error) {
	s.calls++
	s.claim = c
	return append([]byte(nil), s.data...), nil
}
func TestNodeRecoveryResultTransportRequiresMTLSAndExactBytes(t *testing.T) {
	data := []byte(`{"schema":"fixture"}`)
	hash := sha256.Sum256(data)
	version := hex.EncodeToString(hash[:])
	store := &recoveryResultStub{data: data}
	s, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		t.Fatal("ordinary input authority used")
		return ContentAuthorization{}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		t.Fatal("input opened")
		return nil, nil
	}), 8192)
	if err != nil {
		t.Fatal(err)
	}
	s.WithNodeRecoveryResults(store)
	scope := contentScope("0123456789abcdef0123456789abcdef", 1)
	claimed := contentClaimContext(context.Background(), nil, "claim-1", bytes.Repeat([]byte{1}, 32))
	contentID := strings.Repeat("3", 64)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		content string
		version string
		code    codes.Code
	}{
		{name: "exact result", ctx: claimed, code: codes.OK},
		{name: "invalid content id", ctx: claimed, content: "x/y", code: codes.InvalidArgument},
		{name: "no TLS", ctx: context.Background(), code: codes.PermissionDenied},
		{name: "version digest mismatch", ctx: claimed, version: strings.Repeat("9", 64), code: codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, c := tc.version, tc.content
			if v == "" {
				v = version
			}
			if c == "" {
				c = contentID
			}
			response, err := s.ReadNodeRecoveryResult(tc.ctx, &runtimev1.ReadNodeRecoveryResultRequestV1{Scope: scope, ContentId: c, Version: v})
			if tc.code != codes.OK {
				requireContentCode(t, err, tc.code)
				return
			}
			if err != nil || string(response.GetResultJson()) != string(data) {
				t.Fatal(err, response)
			}
		})
	}
	if store.calls != 2 || store.claim.ClaimID != "claim-1" || store.claim.Generation != 1 {
		t.Fatal(store)
	}
}
