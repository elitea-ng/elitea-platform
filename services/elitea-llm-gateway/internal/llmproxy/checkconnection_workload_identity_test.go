package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// checkconnection_workload_identity_test.go — keyless platform credentials on
// the probe routes. Every refusal asserts the reason AND that no token was
// minted: a refused probe that still minted a token has already produced the
// platform's identity for a destination nobody admitted.

type fakeProbeTokens struct {
	mu        sync.Mutex
	id        string
	access    string
	err       error
	audiences []string
	accesses  int
}

func (f *fakeProbeTokens) IDToken(_ context.Context, audience string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audiences = append(f.audiences, audience)
	return f.id, f.err
}

func (f *fakeProbeTokens) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accesses++
	return f.access, f.err
}

func (f *fakeProbeTokens) minted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.audiences) + f.accesses
}

const probePublicProject = "1"

func wiProbeHandler(enabled bool, tokens *fakeProbeTokens) *Handler {
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true})
	WithWorkloadIdentity(enabled, probePublicProject, tokens)(h)
	return h
}

func doSignedCheckConnection(t *testing.T, h *Handler, project string, body checkConnectionRequest) checkConnectionResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/check_connection", bytes.NewReader(raw))
	req.Header.Set(headerProjectID, project)
	rec := httptest.NewRecorder()
	h.CheckConnection(rec, req)
	return decodeCheckConnectionResponse(t, rec)
}

// TestCheckConnectionWorkloadIdentityRefusals is the probe-side matrix.
func TestCheckConnectionWorkloadIdentityRefusals(t *testing.T) {
	dial := checkConnectionRequest{Type: "ai_dial", APIBase: "https://dial.example.com", AuthMode: "workload_identity"}
	vertex := checkConnectionRequest{
		Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1", AuthMode: "workload_identity",
	}
	vertexWithDoc := vertex
	vertexWithDoc.VertexCredentials = `{"type":"service_account"}`
	dialWithKey := dial
	dialWithKey.APIKey = "k"
	openAI := checkConnectionRequest{Type: "open_ai", APIBase: "https://api.openai.com/v1", AuthMode: "workload_identity"}
	unknown := dial
	unknown.AuthMode = "ambient"

	cases := []struct {
		name    string
		enabled bool
		project string
		body    checkConnectionRequest
		reason  string
	}{
		{"flag off", false, probePublicProject, dial, checkConnectionReasonWorkloadIdentityNotAllowed},
		{"tenant project", true, "7", dial, checkConnectionReasonWorkloadIdentityNotAllowed},
		{"unsigned project", true, "", vertex, checkConnectionReasonWorkloadIdentityNotAllowed},
		{"vertex with a document", true, probePublicProject, vertexWithDoc, checkConnectionReasonInvalidAuthMode},
		{"dial with a key", true, probePublicProject, dialWithKey, checkConnectionReasonInvalidAuthMode},
		{"unsupported type", true, probePublicProject, openAI, checkConnectionReasonWorkloadIdentityNotAllowed},
		{"unknown auth_mode", true, probePublicProject, unknown, checkConnectionReasonInvalidAuthMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens := &fakeProbeTokens{id: "id", access: "access"}
			resp := doSignedCheckConnection(t, wiProbeHandler(tc.enabled, tokens), tc.project, tc.body)
			if resp.Success || resp.Reason != tc.reason {
				t.Fatalf("response = %+v, want reason %q", resp, tc.reason)
			}
			if n := tokens.minted(); n != 0 {
				t.Errorf("a refused probe minted %d tokens", n)
			}
		})
	}
}

// TestCheckConnectionWorkloadIdentityTokenFailure: an admitted payload whose
// token cannot be minted answers the named reason, and the provider is never
// dialled.
func TestCheckConnectionWorkloadIdentityTokenFailure(t *testing.T) {
	tokens := &fakeProbeTokens{err: errors.New("metadata: unavailable")}
	resp := doSignedCheckConnection(t, wiProbeHandler(true, tokens), probePublicProject, checkConnectionRequest{
		Type: "ai_dial", APIBase: "https://dial.example.com/v1", AuthMode: "workload_identity",
	})
	if resp.Success || resp.Reason != checkConnectionReasonWorkloadIdentityToken {
		t.Fatalf("response = %+v, want %q", resp, checkConnectionReasonWorkloadIdentityToken)
	}
	if len(tokens.audiences) != 1 || tokens.audiences[0] != "https://dial.example.com" {
		t.Errorf("audiences = %v, want the api_base origin", tokens.audiences)
	}
}

// TestCheckConnectionWorkloadIdentityEgressRefusalMintsNothing: the token is
// minted only after every destination passed the allowlist.
func TestCheckConnectionWorkloadIdentityEgressRefusalMintsNothing(t *testing.T) {
	tokens := &fakeProbeTokens{id: "id"}
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false})
	WithWorkloadIdentity(true, probePublicProject, tokens)(h)
	resp := doSignedCheckConnection(t, h, probePublicProject, checkConnectionRequest{
		Type: "ai_dial", APIBase: "https://dial.example.com", AuthMode: "workload_identity",
	})
	if resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("response = %+v, want %q", resp, checkConnectionReasonEgress)
	}
	if n := tokens.minted(); n != 0 {
		t.Errorf("minted %d tokens for a destination the allowlist refused", n)
	}
}

// TestKeylessVertexProbeContactsOnlyTheAPIHost: a keyless payload has no
// document and so no token endpoint to gate; the Vertex API host is the only
// target, and the access token is the minted one, never a document exchange.
func TestKeylessVertexProbeContactsOnlyTheAPIHost(t *testing.T) {
	req := checkConnectionRequest{Type: "vertex_ai", VertexProject: "p", VertexLocation: "europe-west4", keyless: true}
	targets, err := checkConnectionVertexTargets(req)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 1 || targets[0] != "https://europe-west4-aiplatform.googleapis.com" {
		t.Fatalf("targets = %v, want the regional Vertex API host only", targets)
	}
	if _, err := mintVertexAccessToken(context.Background(), nil, req); err == nil {
		t.Fatal("a keyless payload with no minted token was not refused")
	}
	req.bearer = "adc-access-token"
	token, err := mintVertexAccessToken(context.Background(), nil, req)
	if err != nil || token != "adc-access-token" {
		t.Fatalf("token = %q, err = %v, want the minted token", token, err)
	}
	// The mint itself goes through the access-token source.
	tokens := &fakeProbeTokens{access: "adc-access-token"}
	h := wiProbeHandler(true, tokens)
	minted := checkConnectionRequest{Type: "vertex_ai", keyless: true}
	if fe := h.mintProbeToken(context.Background(), &minted); fe != nil || minted.bearer != "adc-access-token" {
		t.Fatalf("mint = %+v, bearer = %q", fe, minted.bearer)
	}
}

// TestKeylessDialProbesSendTheBearer: the credential probe and the Anthropic
// model route carry the minted ID token as a bearer and no key header.
func TestKeylessDialProbesSendTheBearer(t *testing.T) {
	fp := newFakeProvider(http.StatusOK)
	defer fp.Close()
	req := checkConnectionRequest{APIBase: fp.URL, keyless: true, bearer: "google-id-token"}
	if err := probeAzureDeployments(context.Background(), fp.Client(), req); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if fp.lastAuth != "Bearer google-id-token" || fp.lastAPIKeyHdr != "" {
		t.Fatalf("Authorization = %q, api-key = %q, want the bearer alone", fp.lastAuth, fp.lastAPIKeyHdr)
	}

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer srv.Close()
	req = checkConnectionRequest{
		APIBase: srv.URL, Model: "claude-sonnet-4-5", DialProtocol: "anthropic", keyless: true, bearer: "google-id-token",
	}
	if err := probeDialCompletion(context.Background(), srv.Client(), req); err != nil {
		t.Fatalf("anthropic probe: %v", err)
	}
	if got.Get("Authorization") != "Bearer google-id-token" || got.Get("X-Api-Key") != "" {
		t.Fatalf("Authorization = %q, x-api-key = %q, want the bearer alone", got.Get("Authorization"), got.Get("X-Api-Key"))
	}
}
