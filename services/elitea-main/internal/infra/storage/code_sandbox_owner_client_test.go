package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// codeOwnerTestTransport is the supervisor end of the owner RPCs. A status
// other than 200 answers with a gRPC refusal.
type codeOwnerTestTransport struct {
	runtimev1.SandboxSupervisorServiceClient
	calls   int
	status  int
	body    string
	path    string
	request []byte
}

func (f *codeOwnerTestTransport) answer(method, jobKey string, request []byte) ([]byte, error) {
	f.calls++
	f.path = method + "/" + jobKey
	f.request = request
	if f.status != 200 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "refused")
	}
	return []byte(f.body), nil
}

func (f *codeOwnerTestTransport) CodeOwnerRecoveryRead(_ context.Context, r *runtimev1.CodeOwnerRecoveryReadRequestV1, _ ...grpc.CallOption) (*runtimev1.CodeOwnerRecoveryReadResponseV1, error) {
	raw, err := f.answer("CodeOwnerRecoveryRead", r.GetJobKey(), r.GetRequestJson())
	return &runtimev1.CodeOwnerRecoveryReadResponseV1{ResponseJson: raw}, err
}

func (f *codeOwnerTestTransport) CodeOwnerRecoverySealNoEffect(_ context.Context, r *runtimev1.CodeOwnerRecoverySealNoEffectRequestV1, _ ...grpc.CallOption) (*runtimev1.CodeOwnerRecoverySealNoEffectResponseV1, error) {
	raw, err := f.answer("CodeOwnerRecoverySealNoEffect", r.GetJobKey(), r.GetRequestJson())
	return &runtimev1.CodeOwnerRecoverySealNoEffectResponseV1{ResponseJson: raw}, err
}

func (f *codeOwnerTestTransport) CodePlatformOwnerReadRetainedRuntime(_ context.Context, r *runtimev1.CodePlatformOwnerReadRetainedRuntimeRequestV1, _ ...grpc.CallOption) (*runtimev1.CodePlatformOwnerReadRetainedRuntimeResponseV1, error) {
	raw, err := f.answer("CodePlatformOwnerReadRetainedRuntime", r.GetJobKey(), r.GetRequestJson())
	return &runtimev1.CodePlatformOwnerReadRetainedRuntimeResponseV1{ResponseJson: raw}, err
}

func (f *codeOwnerTestTransport) CodePlatformOwnerReadPendingCall(_ context.Context, r *runtimev1.CodePlatformOwnerReadPendingCallRequestV1, _ ...grpc.CallOption) (*runtimev1.CodePlatformOwnerReadPendingCallResponseV1, error) {
	raw, err := f.answer("CodePlatformOwnerReadPendingCall", r.GetJobKey(), r.GetRequestJson())
	return &runtimev1.CodePlatformOwnerReadPendingCallResponseV1{ResponseJson: raw}, err
}

func (f *codeOwnerTestTransport) CodePlatformOwnerPublishReply(_ context.Context, r *runtimev1.CodePlatformOwnerPublishReplyRequestV1, _ ...grpc.CallOption) (*runtimev1.CodePlatformOwnerPublishReplyResponseV1, error) {
	raw, err := f.answer("CodePlatformOwnerPublishReply", r.GetJobKey(), r.GetRequestJson())
	return &runtimev1.CodePlatformOwnerPublishReplyResponseV1{ResponseJson: raw}, err
}

func codeGrantFixture(t *testing.T) (*CodeOwnerGrantSigner, ed25519.PublicKey, code.GrantClaims) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewCodeOwnerGrantSigner("active", key, "spiffe://elitea/main", []string{"spiffe://elitea/supervisor/one"})
	if err != nil {
		t.Fatal(err)
	}
	activation := strings.Repeat("1", 64)
	effect := code.DispatchActivation(activation, 1)
	c := code.GrantClaims{Schema: "elitea.sandbox.node-code-recovery-grant.v1", TenantID: "7", ProjectID: 7, ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, ClaimID: "23456789abcdef0123456789abcdef01", ClaimAttempt: 2, LeaseEpoch: 3, FenceSHA256: strings.Repeat("2", 64), ActivationID: activation, NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, ExpectedRevision: 2, ReceiptSHA256: strings.Repeat("3", 64), DispatchActivation: effect, JobKey: code.JobKey("0123456789abcdef0123456789abcdef", effect), RequestDigest: strings.Repeat("4", 64), BindingSHA256: strings.Repeat("5", 64), SupervisorAudience: "spiffe://elitea/supervisor/one", RequesterWorkloadIdentity: "spiffe://elitea/main", Operation: "read", IssuedAtMillis: 1000, ExpiresAtMillis: 21000}
	return signer, pub, c
}
func TestCodeOwnerGrantDomainAndBoundedLifetime(t *testing.T) {
	s, pub, c := codeGrantFixture(t)
	g, err := s.SignRecovery(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(g.ClaimsBase64URL)
	sig, _ := base64.RawURLEncoding.DecodeString(g.SignatureBase64URL)
	framed := append([]byte(code.RecoveryDomain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(framed[len(code.RecoveryDomain):], uint64(len(raw)))
	framed = append(framed, raw...)
	if !ed25519.Verify(pub, framed, sig) {
		t.Fatal("owning signature invalid")
	}
	framed[0] ^= 1
	if ed25519.Verify(pub, framed, sig) {
		t.Fatal("signature crossed purpose domain")
	}
	for _, tc := range []struct {
		name   string
		change func(*code.GrantClaims)
	}{{"expired lifetime", func(c *code.GrantClaims) { c.ExpiresAtMillis = c.IssuedAtMillis }}, {"overlong lifetime", func(c *code.GrantClaims) { c.ExpiresAtMillis = c.IssuedAtMillis + 30001 }}, {"different Main", func(c *code.GrantClaims) { c.RequesterWorkloadIdentity = "spiffe://other/main" }}, {"different Supervisor", func(c *code.GrantClaims) { c.SupervisorAudience = "spiffe://other/owner" }}, {"ordinary submit", func(c *code.GrantClaims) { c.Operation = "submit" }}, {"missing binding", func(c *code.GrantClaims) { c.BindingSHA256 = "" }}, {"changed effect", func(c *code.GrantClaims) { c.DispatchActivation = strings.Repeat("9", 64) }}} {
		t.Run(tc.name, func(t *testing.T) {
			changed := c
			tc.change(&changed)
			if _, err := s.SignRecovery(changed); err == nil {
				t.Fatal("invalid authority signed")
			}
		})
	}
}
func TestCodeOwnerClientOneAttemptAndNoEvidenceFromUnknownStates(t *testing.T) {
	s, _, claims := codeGrantFixture(t)
	g, _ := s.SignRecovery(claims)
	for _, state := range []string{"pending", "failed", "cancelled", "uncertain", "missing", "conflict"} {
		t.Run(state, func(t *testing.T) {
			raw, _ := json.Marshal(code.Response{Schema: "elitea.sandbox.node-code-recovery-response.v1", State: state, Receipt: json.RawMessage("null")})
			tr := &codeOwnerTestTransport{status: 200, body: string(raw)}
			c := &CodeOwnerClient{requester: claims.RequesterWorkloadIdentity, endpoints: map[string]codeOwnerEndpoint{claims.SupervisorAudience: {client: tr}}}
			if _, err := c.Read(t.Context(), claims, g); err == nil {
				t.Fatal("unknown granted evidence")
			}
			if tr.calls != 1 || tr.path != "CodeOwnerRecoveryRead/"+claims.JobKey {
				t.Fatal("incorrect owner route/attempts")
			}
		})
	}
	tr := &codeOwnerTestTransport{status: 307, body: `{}`}
	c := &CodeOwnerClient{requester: claims.RequesterWorkloadIdentity, endpoints: map[string]codeOwnerEndpoint{claims.SupervisorAudience: {client: tr}}}
	if _, err := c.Read(t.Context(), claims, g); err == nil || tr.calls != 1 {
		t.Fatal("refusal accepted/retried")
	}
}
