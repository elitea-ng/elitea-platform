package httpaction

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAdmit struct {
	visits int
	reject bool
}

func (f *fakeAdmit) Admit(_ context.Context, _ Invocation, _ Request) (Admission, error) {
	f.visits++
	if f.reject {
		return Admission{}, ErrUnauthorized
	}
	return Admission{ExecutionID: "execution", Generation: 3, ProjectID: 7, ActorID: 42, ClaimID: "claim", LeaseEpoch: 2, Deadline: time.Now().Add(time.Minute), OutputBucket: "outputs", PolicyDigest: Digest([]byte("policy"))}, nil
}

type fakeEffects struct {
	mu      sync.Mutex
	receipt *Receipt
	starts  int
	commits int
	fail    bool
}

func (f *fakeEffects) Lookup(_ context.Context, _ Admission, _ Invocation, _ string) (Receipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.receipt != nil {
		return *f.receipt, true, nil
	}
	return Receipt{}, false, nil
}
func (f *fakeEffects) Begin(_ context.Context, a Admission, i Invocation, id string) (Receipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.fail {
		return Receipt{}, false, ErrUnavailable
	}
	if f.receipt != nil {
		return *f.receipt, false, nil
	}
	r := Receipt{SchemaVersion: ReceiptSchema, ActivationID: i.ActivationID, RequestDigest: i.RequestDigest, BindingDigest: i.BindingDigest, EffectID: id, State: "pending"}
	f.receipt = &r
	return r, true, nil
}
func (f *fakeEffects) Commit(_ context.Context, _ Admission, r Receipt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	f.receipt = &r
	return nil
}

type fakeTransport struct {
	calls  int
	status int
	media  string
	body   string
	err    error
}

func (f *fakeTransport) Do(r *http.Request) (*http.Response, error) {
	f.calls++
	if r.URL.Scheme != "https" {
		panic("wrong scheme")
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Header: http.Header{"Content-Type": []string{f.media}}, ContentLength: int64(len(f.body)), Body: io.NopCloser(strings.NewReader(f.body))}, nil
}
func invoke(request string) Invocation {
	digest := shaDigest("fixture-binding")
	return Invocation{SchemaVersion: Schema, NodeID: "fetch", ThreadID: "graph", Step: 4, Request: json.RawMessage(request), BindingDigest: Digest([]byte("fixture-binding")), RequestWireB64: base64.StdEncoding.EncodeToString([]byte(request)), RequestDigest: Digest([]byte(request)), ActivationID: VisitID("graph", "fetch", 4, digest)}
}
func shaDigest(s string) [32]byte {
	var result [32]byte
	data, _ := hex.DecodeString(Digest([]byte(s)))
	copy(result[:], data)
	return result
}
func jsonRequest() string {
	return `{"method":"GET","url":"https://api.example.test/items","response":{"mode":"json"}}`
}
func TestOneEffectAndCommittedResultReuse(t *testing.T) {
	a := &fakeAdmit{}
	e := &fakeEffects{}
	transport := &fakeTransport{status: 200, media: "application/json", body: `{"value":7}`}
	s, _ := New(a, e, nil, nil, transport)
	for range 2 {
		r, err := s.Execute(context.Background(), invoke(jsonRequest()))
		if err != nil || r.State != "completed" || r.Result == nil {
			t.Fatalf("receipt=%#v err=%v", r, err)
		}
	}
	if transport.calls != 1 || e.commits != 1 {
		t.Fatalf("dispatches=%d commits=%d", transport.calls, e.commits)
	}
}
func TestInvalidOrDeniedRequestHasZeroEffects(t *testing.T) {
	for _, mutate := range []func(*Invocation){func(i *Invocation) { i.RequestDigest = strings.Repeat("0", 64) }, func(i *Invocation) { i.ActivationID = strings.Repeat("0", 64) }, func(i *Invocation) { i.SchemaVersion = "v2" }} {
		a := &fakeAdmit{}
		e := &fakeEffects{}
		transport := &fakeTransport{}
		s, _ := New(a, e, nil, nil, transport)
		i := invoke(jsonRequest())
		mutate(&i)
		if _, err := s.Execute(context.Background(), i); err == nil {
			t.Fatal("invalid request admitted")
		}
		if a.visits != 0 || e.starts != 0 || transport.calls != 0 {
			t.Fatal("invalid request has effects")
		}
	}
	a := &fakeAdmit{reject: true}
	e := &fakeEffects{}
	transport := &fakeTransport{}
	s, _ := New(a, e, nil, nil, transport)
	if _, err := s.Execute(context.Background(), invoke(jsonRequest())); err == nil {
		t.Fatal("denial accepted")
	}
	if e.starts != 0 || transport.calls != 0 {
		t.Fatal("denied request dispatched")
	}
}
func TestTimeoutAfterDispatchReconcilesWithoutSecondRequest(t *testing.T) {
	a := &fakeAdmit{}
	e := &fakeEffects{}
	transport := &fakeTransport{err: context.DeadlineExceeded}
	s, _ := New(a, e, nil, nil, transport)
	for range 2 {
		r, err := s.Execute(context.Background(), invoke(jsonRequest()))
		if err != nil || r.State != "uncertain" || r.Result != nil {
			t.Fatal("uncertainty lost")
		}
	}
	if transport.calls != 1 {
		t.Fatal("uncertain external request replayed")
	}
}
func TestStatusAndMalformedResponseStopWithoutSuccessfulProjection(t *testing.T) {
	for _, fixture := range []struct {
		status int
		body   string
		code   string
	}{{302, `{}`, "redirect_refused"}, {401, `{}`, "authentication"}, {403, `{}`, "authorization"}, {429, `{}`, "rate_limited"}, {503, `{}`, "dependency_unavailable"}, {200, `{broken`, "invalid_response"}} {
		a := &fakeAdmit{}
		e := &fakeEffects{}
		transport := &fakeTransport{status: fixture.status, media: "application/json", body: fixture.body}
		s, _ := New(a, e, nil, nil, transport)
		r, err := s.Execute(context.Background(), invoke(jsonRequest()))
		if err != nil || r.State != "failed" || r.Result != nil || r.FailureCode == nil || *r.FailureCode != fixture.code {
			t.Fatalf("fixture=%#v receipt=%#v err=%v", fixture, r, err)
		}
	}
}
func TestOversizedResponseIsUncertainAndNotTruncated(t *testing.T) {
	a := &fakeAdmit{}
	e := &fakeEffects{}
	transport := &fakeTransport{status: 200, media: "application/json", body: strings.Repeat("x", MaxResponse+1)}
	s, _ := New(a, e, nil, nil, transport)
	r, err := s.Execute(context.Background(), invoke(jsonRequest()))
	if err != nil || r.State != "uncertain" || r.Result != nil {
		t.Fatal("oversize fabricated output")
	}
}
func TestRegistrationFailureCannotDispatch(t *testing.T) {
	e := &fakeEffects{fail: true}
	transport := &fakeTransport{}
	s, _ := New(&fakeAdmit{}, e, nil, nil, transport)
	if _, err := s.Execute(context.Background(), invoke(jsonRequest())); err == nil || transport.calls != 0 {
		t.Fatal("unregistered effect dispatched")
	}
}
func TestHeadersAndBodiesFailBeforeEffect(t *testing.T) {
	for _, request := range []string{
		`{"method":"GET","url":"http://api.example.test/items","response":{"mode":"json"}}`,
		`{"method":"POST","url":"https://api.example.test/items","headers":[{"name":"Authorization","value":"PRIVATE_MARKER"}],"response":{"mode":"json"}}`,
		`{"method":"POST","url":"https://api.example.test/items","headers":[{"name":"x-trace","value":"a"},{"name":"X-Trace","value":"b"}],"response":{"mode":"json"}}`,
		`{"method":"GET","url":"https://api.example.test/items","body":{"kind":"json","value":{}},"response":{"mode":"json"}}`,
		`{"method":"POST","url":"https://api.example.test/items?access_token=PRIVATE_MARKER","response":{"mode":"json"}}`,
	} {
		r, err := Parse(invoke(request))
		if err == nil {
			t.Fatalf("unsafe input accepted: %#v", r)
		}
		if strings.Contains(err.Error(), "PRIVATE_MARKER") {
			t.Fatal("private input leaked")
		}
	}
}
func TestJSONNullAndHeadRemainTyped(t *testing.T) {
	request, _ := Parse(invoke(jsonRequest()))
	r, code := Project(request, 200, "application/json", []byte("null"), nil)
	raw, _ := json.Marshal(r)
	if code != "" || !strings.Contains(string(raw), `"value":null`) {
		t.Fatal("null value disappeared")
	}
	request.Method = "HEAD"
	r, code = Project(request, 200, "", nil, nil)
	if code != "" || r.Data.Kind != "empty" {
		t.Fatal("HEAD failed")
	}
}

type fakeCredentials struct {
	calls  int
	reject bool
}

func (f *fakeCredentials) Headers(_ context.Context, _ Admission, _ CredentialReference) (http.Header, error) {
	f.calls++
	if f.reject {
		return nil, ErrUnauthorized
	}
	return http.Header{"Authorization": []string{"Bearer PRIVATE_MARKER"}}, nil
}

type credentialAdmitter struct{ fakeAdmit }

func (f *credentialAdmitter) Admit(c context.Context, i Invocation, r Request) (Admission, error) {
	a, err := f.fakeAdmit.Admit(c, i, r)
	a.CredentialRevision = strings.Repeat("a", 64)
	return a, err
}
func TestCommittedReceiptDoesNotRedeemCredentialsAgain(t *testing.T) {
	a := &credentialAdmitter{}
	e := &fakeEffects{}
	credentials := &fakeCredentials{}
	transport := &fakeTransport{status: 200, media: "application/json", body: "null"}
	s, _ := New(a, e, credentials, nil, transport)
	i := invoke(`{"method":"GET","url":"https://api.example.test/items","credential":{"configuration_id":11},"response":{"mode":"json"}}`)
	first, err := s.Execute(context.Background(), i)
	if err != nil || first.State != "completed" {
		t.Fatal(err)
	}
	credentials.reject = true
	second, err := s.Execute(context.Background(), i)
	if err != nil || second.State != "completed" || credentials.calls != 1 || transport.calls != 1 {
		t.Fatal("committed receipt fetched credentials or dispatched again")
	}
	raw, _ := json.Marshal(second)
	if strings.Contains(string(raw), "PRIVATE_MARKER") {
		t.Fatal("credential persisted in receipt")
	}
}
func TestDuplicateJSONAndExplicitInvalidDefaultsFailBeforeEffects(t *testing.T) {
	for _, request := range []string{
		`{"method":"GET","method":"POST","url":"https://api.example.test/items","response":{"mode":"json"}}`,
		`{"method":"POST","url":"https://api.example.test/items","body":{"kind":"json","value":{"key":1,"key":2}},"response":{"mode":"json"}}`,
		`{"method":"GET","url":"https://api.example.test/items","timeout_ms":0,"response":{"mode":"json"}}`,
		`{"method":"GET","url":"https://api.example.test/items","timeout_ms":null,"response":{"mode":"json"}}`,
		`{"method":"POST","url":"https://api.example.test/items","body":{},"response":{"mode":"json"}}`,
		`{"method":"GET","url":"https://api.example.test/items","headers":[{"name":"X-Trace"}],"response":{"mode":"json"}}`,
		`{"method":"GET","url":"https://api.example.test/items","response":{"mode":"json","max_bytes":0}}`,
	} {
		a := &fakeAdmit{}
		e := &fakeEffects{}
		transport := &fakeTransport{}
		s, _ := New(a, e, nil, nil, transport)
		if _, err := s.Execute(context.Background(), invoke(request)); err == nil || a.visits != 0 || e.starts != 0 || transport.calls != 0 {
			t.Fatal("malformed input crossed effect fence")
		}
	}
}

type fakeArtifacts struct {
	calls int
	bytes []byte
	fail  bool
}

func (f *fakeArtifacts) Read(_ context.Context, _ Admission, _ Artifact, _ uint64) ([]byte, error) {
	return nil, ErrUnauthorized
}
func (f *fakeArtifacts) Write(_ context.Context, _ Admission, id, media string, body []byte) (Artifact, error) {
	f.calls++
	f.bytes = body
	if f.fail {
		return Artifact{}, ErrUnavailable
	}
	digest := Digest(body)
	return Artifact{Reference: "artifact:outputs:" + id, ImmutableVersion: digest, SHA256: digest, ByteLength: uint64(len(body))}, nil
}
func TestLargeTypedResponseUsesArtifactAndRefusesLostStorage(t *testing.T) {
	for _, fail := range []bool{false, true} {
		a := &fakeAdmit{}
		e := &fakeEffects{}
		artifacts := &fakeArtifacts{fail: fail}
		transport := &fakeTransport{status: 200, media: "application/octet-stream", body: strings.Repeat("x", MaxInline+1)}
		s, _ := New(a, e, nil, artifacts, transport)
		i := invoke(`{"method":"GET","url":"https://api.example.test/items","response":{"mode":"artifact"}}`)
		r, err := s.Execute(context.Background(), i)
		if err != nil || artifacts.calls != 1 || len(artifacts.bytes) != MaxInline+1 {
			t.Fatal("bounded bytes were not stored")
		}
		if fail {
			if r.State != "uncertain" || r.Result != nil {
				t.Fatal("storage failure fabricated success")
			}
		} else if r.State != "completed" || r.Result.Data.Kind != "artifact" || r.Result.Data.Reference.ByteLength != MaxInline+1 {
			t.Fatal("large body returned inline")
		}
	}
}

func TestHTTPIdentityMatchesTheCrossLanguageFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/http_frozen_request.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Wire          string `json:"request_wire_utf8"`
			Binding       string `json:"binding_digest"`
			RequestDigest string `json:"request_digest"`
			Thread        string `json:"thread_id"`
			Node          string `json:"node_id"`
			Step          uint64 `json:"step"`
			Execution     string `json:"execution_id"`
			Generation    uint64 `json:"generation"`
			Activation    string `json:"activation_id"`
			Effect        string `json:"effect_id"`
		} `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid fixture")
	}
	for _, f := range fixture.Cases {
		inv := Invocation{ThreadID: f.Thread, NodeID: f.Node, Step: f.Step, ActivationID: f.Activation, RequestDigest: f.RequestDigest, BindingDigest: f.Binding}
		if Digest([]byte(f.Wire)) != f.RequestDigest || VisitID(f.Thread, f.Node, f.Step, BindingBytes(f.Binding)) != f.Activation || EffectID(f.Execution, f.Generation, inv) != f.Effect {
			t.Fatal("cross-language identity mismatch")
		}
		changed := inv
		changed.BindingDigest = Digest([]byte("other"))
		if EffectID(f.Execution, f.Generation, changed) == f.Effect {
			t.Fatal("frozen selection identity lost")
		}
	}
}
