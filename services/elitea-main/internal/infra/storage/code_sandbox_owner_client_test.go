package storage

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

type codeOwnerTestTransport struct {
	calls   int
	status  int
	body    string
	path    string
	query   string
	headers http.Header
}

func (f *codeOwnerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	f.path = r.URL.Path
	f.query = r.URL.RawQuery
	f.headers = r.Header.Clone()
	return &http.Response{StatusCode: f.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(f.body)), ContentLength: int64(len(f.body))}, nil
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
			c := &CodeOwnerClient{requester: claims.RequesterWorkloadIdentity, endpoints: map[string]codeOwnerEndpoint{claims.SupervisorAudience: {origin: "https://owner.invalid", client: &http.Client{Transport: tr}}}}
			if _, err := c.Read(t.Context(), claims, g); err == nil {
				t.Fatal("unknown granted evidence")
			}
			if tr.calls != 1 || tr.query != "" || tr.path != "/elitea.runtime.node-code-recovery.v1/jobs/"+claims.JobKey+"/read" {
				t.Fatal("incorrect owner route/attempts")
			}
		})
	}
	tr := &codeOwnerTestTransport{status: 307, body: `{}`}
	c := &CodeOwnerClient{requester: claims.RequesterWorkloadIdentity, endpoints: map[string]codeOwnerEndpoint{claims.SupervisorAudience: {origin: "https://owner.invalid", client: &http.Client{Transport: tr}}}}
	if _, err := c.Read(t.Context(), claims, g); err == nil || tr.calls != 1 {
		t.Fatal("redirect accepted/retried")
	}
}
