package httpaction

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeAdmitter struct {
	admission Admission
	err       error
	calls     int
}

func (f *fakeAdmitter) Admit(context.Context, Invocation) (Admission, error) {
	f.calls++
	return f.admission, f.err
}

type fakeRules struct {
	rule  Rule
	err   error
	calls int
}

func (f *fakeRules) Rule(_ context.Context, projectID int64, name string) (Rule, error) {
	f.calls++
	if f.err != nil {
		return Rule{}, f.err
	}
	if projectID != f.rule.ProjectID || name != f.rule.Name {
		return Rule{}, policyDenied()
	}
	return f.rule, nil
}

type fakeEffects struct {
	lookup      *Receipt
	lookupErr   error
	beginErr    error
	noDispatch  bool
	begun       []Intent
	committed   []Receipt
	commitCtxOK bool
	commitErr   error
}

func (f *fakeEffects) Lookup(context.Context, Admission, Invocation) (Receipt, bool, error) {
	if f.lookupErr != nil {
		return Receipt{}, false, f.lookupErr
	}
	if f.lookup != nil {
		return *f.lookup, true, nil
	}
	return Receipt{}, false, nil
}

func (f *fakeEffects) Begin(_ context.Context, admitted Admission, inv Invocation, intent Intent) (Receipt, bool, error) {
	if f.beginErr != nil {
		return Receipt{}, false, f.beginErr
	}
	f.begun = append(f.begun, intent)
	receipt := Receipt{SchemaVersion: ReceiptSchema, ActivationID: inv.ActivationID, RequestDigest: intent.RequestDigest, BindingDigest: inv.BindingDigest, EffectID: intent.EffectID, State: "dispatching"}
	return receipt, !f.noDispatch, nil
}

func (f *fakeEffects) Commit(ctx context.Context, _ Admission, receipt Receipt) error {
	f.commitCtxOK = ctx.Err() == nil
	f.committed = append(f.committed, receipt)
	return f.commitErr
}

type fakeCredentials struct {
	headers http.Header
	calls   int
	last    http.Header
}

func (f *fakeCredentials) Resolve(context.Context, Admission, int32) (Credential, error) {
	f.calls++
	f.last = f.headers.Clone()
	f.headers = f.last
	return Credential{Headers: f.last, Revision: strings.Repeat("c", 64)}, nil
}

type fakeArtifacts struct{ writes int }

func (f *fakeArtifacts) Read(context.Context, Admission, Artifact, uint64) ([]byte, error) {
	return nil, errors.New("unused")
}

func (f *fakeArtifacts) Write(_ context.Context, _ Admission, bucket, effect, _ string, data []byte) (Artifact, error) {
	f.writes++
	sum := Digest(data)
	return Artifact{Reference: "artifact:" + bucket + ":http-effects/" + effect + "/" + sum + ".bin", ImmutableVersion: sum, SHA256: sum, ByteLength: uint64(len(data))}, nil
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type allowTools bool

func (a allowTools) Permits(string, string) bool { return bool(a) }

type serviceHarness struct {
	admit       *fakeAdmitter
	rules       *fakeRules
	effects     *fakeEffects
	credentials *fakeCredentials
	artifacts   *fakeArtifacts
	requests    []*http.Request
	bodies      [][]byte
	respond     func(*http.Request) (*http.Response, error)
	service     *Service
}

func newHarness(t *testing.T, method, request string) *serviceHarness {
	t.Helper()
	node, err := ParseNode([]byte(`{"id":"fetch","type":"http","revision":2,"operation":"tracker.item","request":` + request + `,"response":{"mode":"json"},"timeout_ms":1000,"output":["out"]}`))
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: 4, ProjectID: 9, Revision: 2, Name: "tracker.item", Origin: "https://api.example.com", PathTemplate: "/items/{item}", Method: method, ConfigurationID: 7, Toolkit: "tracker", Tool: "item", OutputBucket: "http-results", Enabled: true}
	rule.Normalize()
	h := &serviceHarness{
		admit:       &fakeAdmitter{admission: Admission{ExecutionID: "exec", Generation: 1, ProjectID: 9, ActorID: 5, ClaimID: "claim", LeaseEpoch: 1, Deadline: time.Now().Add(time.Minute), Template: node.Template, Tools: allowTools(true)}},
		rules:       &fakeRules{rule: rule},
		effects:     &fakeEffects{},
		credentials: &fakeCredentials{headers: http.Header{"Authorization": {"Bearer sentinel-secret"}}},
		artifacts:   &fakeArtifacts{},
	}
	h.respond = func(*http.Request) (*http.Response, error) { return jsonResponse(200, `{"ok":true}`), nil }
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		h.requests = append(h.requests, r)
		body, _ := io.ReadAll(r.Body)
		h.bodies = append(h.bodies, body)
		return h.respond(r)
	})
	h.service, err = New(h.admit, h.rules, h.effects, h.credentials, h.artifacts, transport)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func invocationFor(t *testing.T, h *serviceHarness, attempt uint32, inputs string) Invocation {
	t.Helper()
	raw, err := CanonicalJSON([]byte(inputs))
	if err != nil {
		t.Fatal(err)
	}
	binding := strings.Repeat("d", 64)
	inv := Invocation{SchemaVersion: Schema, NodeID: "fetch", ThreadID: "graph", Step: 1, Attempt: attempt, BindingDigest: binding, InputsB64: base64.StdEncoding.EncodeToString(raw), InputsDigest: Digest(raw)}
	inv.ActivationID = VisitID(inv.ThreadID, inv.NodeID, inv.Step, BindingBytes(binding))
	return inv
}

func TestExecuteDispatchesOnceAfterBeginAndCommitsReceipt(t *testing.T) {
	h := newHarness(t, "POST", `{"path":{"item":{"from":"k"}},"body":{"kind":"json","from":"b"},"idempotency_key":{"from":"r"}}`)
	receipt, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"body":{"x":1},"idempotency_key":"req-1","path":{"item":"a/b"}}`))
	if err != nil || receipt.State != "completed" || receipt.Result == nil || receipt.Result.Status != 200 {
		t.Fatalf("%+v %v", receipt, err)
	}
	if len(h.requests) != 1 || len(h.effects.begun) != 1 || len(h.effects.committed) != 1 {
		t.Fatalf("dispatches=%d begins=%d commits=%d", len(h.requests), len(h.effects.begun), len(h.effects.committed))
	}
	sent := h.requests[0]
	if sent.URL.String() != "https://api.example.com/items/a%2Fb" || sent.Method != "POST" || sent.Header.Get("Idempotency-Key") != "req-1" || sent.Header.Get("Content-Type") != "application/json" || string(h.bodies[0]) != `{"x":1}` {
		t.Fatalf("request %s %s %v %q", sent.Method, sent.URL, sent.Header, h.bodies[0])
	}
	if h.credentials.calls != 1 || len(h.credentials.last) != 0 {
		t.Fatal("credential headers were not resolved once and zeroed after the call")
	}
	intent := h.effects.begun[0]
	if intent.Retryable || intent.CredentialRevision != strings.Repeat("c", 64) || intent.Rule.Revision != 2 || intent.RequestDigest != Digest(intent.RequestWire) {
		t.Fatalf("intent %+v", intent)
	}
}

func TestExecuteReturnsStoredReceiptWithoutRedemptionOrDispatch(t *testing.T) {
	h := newHarness(t, "GET", `{"path":{"item":{"from":"k"}}}`)
	h.effects.lookup = &Receipt{State: "completed"}
	if receipt, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"path":{"item":"i"}}`)); err != nil || receipt.State != "completed" {
		t.Fatal(receipt, err)
	}
	if h.rules.calls != 0 || h.credentials.calls != 0 || len(h.requests) != 0 || len(h.effects.begun) != 0 {
		t.Fatal("a stored receipt was followed by rule, credential or dispatch work")
	}
	h.effects.lookup = nil
	h.effects.noDispatch = true
	if _, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"path":{"item":"i"}}`)); err != nil || len(h.requests) != 0 {
		t.Fatal("Begin without dispatch still dispatched", err)
	}
}

func TestExecuteTypedRefusalsHappenBeforeAnyEffect(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*serviceHarness)
		code  string
		is    error
	}{
		"rule missing":   {func(h *serviceHarness) { h.rules.rule.Name = "other" }, CodePolicyDenied, ErrUnauthorized},
		"rule disabled":  {func(h *serviceHarness) { h.rules.rule.Enabled = false }, CodePolicyDenied, ErrUnauthorized},
		"tool blocked":   {func(h *serviceHarness) { h.admit.admission.Tools = allowTools(false) }, CodePolicyDenied, ErrUnauthorized},
		"retry POST (C)": {func(h *serviceHarness) {}, CodePolicyDenied, ErrUnauthorized},
		"admit refused":  {func(h *serviceHarness) { h.admit.err = ErrUnauthorized }, "", ErrUnauthorized},
		"nondeterminism": {func(h *serviceHarness) { h.effects.lookupErr = &Failure{Code: CodeNonDetermined} }, CodeNonDetermined, ErrNonDeterministic},
		"in flight":      {func(h *serviceHarness) { h.effects.lookupErr = ErrDispatchInFlight }, "", ErrDispatchInFlight},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "POST", `{"path":{"item":{"from":"k"}}}`)
			tc.setup(h)
			attempt := uint32(0)
			if name == "retry POST (C)" {
				attempt = 1
			}
			_, err := h.service.Execute(t.Context(), invocationFor(t, h, attempt, `{"path":{"item":"i"}}`))
			if !errors.Is(err, tc.is) || FailureCode(err) != tc.code {
				t.Fatalf("got %v (%q)", err, FailureCode(err))
			}
			if len(h.effects.begun) != 0 || len(h.requests) != 0 {
				t.Fatal("refusal reached Begin or the network")
			}
		})
	}
}

func TestExecuteInputsTooLargeBeforeAdmission(t *testing.T) {
	h := newHarness(t, "GET", `{"path":{"item":{"from":"k"}}}`)
	inv := invocationFor(t, h, 0, `{"path":{"item":"i"}}`)
	inv.InputsB64 = strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxInputsBytes+1))
	if _, err := h.service.Execute(t.Context(), inv); FailureCode(err) != CodeInputsTooBig || h.admit.calls != 0 {
		t.Fatalf("%v admit=%d", err, h.admit.calls)
	}
}

// D2 effect classes: safe methods retry (I); POST/PUT/PATCH/DELETE only with
// replay=idempotency_key and a bound key, re-sending the same key (I).
func TestExecuteEffectClasses(t *testing.T) {
	get := newHarness(t, "GET", `{"path":{"item":{"from":"k"}}}`)
	if _, err := get.service.Execute(t.Context(), invocationFor(t, get, 1, `{"path":{"item":"i"}}`)); err != nil || len(get.effects.begun) != 1 || !get.effects.begun[0].Retryable {
		t.Fatalf("GET attempt 1: %v", err)
	}
	post := newHarness(t, "POST", `{"path":{"item":{"from":"k"}},"idempotency_key":{"from":"r"}}`)
	post.rules.rule.Replay = ReplayIdempotencyKey
	if _, err := post.service.Execute(t.Context(), invocationFor(t, post, 1, `{"idempotency_key":"same-key","path":{"item":"i"}}`)); err != nil || !post.effects.begun[0].Retryable || post.requests[0].Header.Get("Idempotency-Key") != "same-key" {
		t.Fatalf("POST with idempotency key attempt 1: %v", err)
	}
	unkeyed := newHarness(t, "POST", `{"path":{"item":{"from":"k"}}}`)
	unkeyed.rules.rule.Replay = ReplayIdempotencyKey
	if _, err := unkeyed.service.Execute(t.Context(), invocationFor(t, unkeyed, 1, `{"path":{"item":"i"}}`)); FailureCode(err) != CodePolicyDenied {
		t.Fatalf("POST without a bound key retried: %v", err)
	}
}

func TestExecuteUncertainOnTransportErrorAndDetachedCommit(t *testing.T) {
	h := newHarness(t, "POST", `{"path":{"item":{"from":"k"}}}`)
	ctx, cancel := context.WithCancel(t.Context())
	h.respond = func(r *http.Request) (*http.Response, error) {
		cancel() // the Worker disconnects mid-call
		if r.Context().Err() != nil {
			return nil, errors.New("dispatch followed the caller's cancellation")
		}
		return nil, errors.New("connection reset mid-response")
	}
	receipt, err := h.service.Execute(ctx, invocationFor(t, h, 0, `{"path":{"item":"i"}}`))
	if err != nil || receipt.State != "uncertain" || *receipt.FailureCode != "reconciliation_required" || !h.effects.commitCtxOK {
		t.Fatalf("%+v %v commitCtxOK=%v", receipt, err, h.effects.commitCtxOK)
	}
}

func TestExecuteRedirectIsAFailedReceiptNeverFollowed(t *testing.T) {
	h := newHarness(t, "GET", `{"path":{"item":{"from":"k"}}}`)
	h.respond = func(*http.Request) (*http.Response, error) {
		response := jsonResponse(302, "")
		response.Header.Set("Location", "https://169.254.169.254/")
		return response, nil
	}
	receipt, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"path":{"item":"i"}}`))
	if err != nil || receipt.State != "failed" || *receipt.FailureCode != "redirect_refused" || len(h.requests) != 1 {
		t.Fatalf("%+v %v requests=%d", receipt, err, len(h.requests))
	}
}

func TestExecuteResponseBoundaries(t *testing.T) {
	big := func(size int) string { return `"` + strings.Repeat("a", size-2) + `"` }
	for name, tc := range map[string]struct {
		size         int
		bucket       string
		state, code  string
		artifactKind bool
	}{
		"inline 512 KiB":            {MaxInline, "http-results", "completed", "", false},
		"inline 512 KiB + 1":        {MaxInline + 1, "http-results", "completed", "", true},
		"inline + 1 without bucket": {MaxInline + 1, "", "failed", "response_too_large", false},
		"response 2 MiB":            {MaxResponse, "http-results", "completed", "", true},
		"response 2 MiB + 1":        {MaxResponse + 1, "http-results", "failed", "response_too_large", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "GET", `{"path":{"item":{"from":"k"}}}`)
			h.rules.rule.OutputBucket = tc.bucket
			body := big(tc.size)
			h.respond = func(*http.Request) (*http.Response, error) { return jsonResponse(200, body), nil }
			receipt, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"path":{"item":"i"}}`))
			if err != nil || receipt.State != tc.state {
				t.Fatalf("%+v %v", receipt, err)
			}
			if tc.code != "" && *receipt.FailureCode != tc.code {
				t.Fatalf("failure %s", *receipt.FailureCode)
			}
			if tc.state == "completed" && (receipt.Result.Data.Kind == "artifact") != tc.artifactKind {
				t.Fatalf("data kind %s", receipt.Result.Data.Kind)
			}
		})
	}
}

func TestExecuteRefusesCredentialHeaderCollision(t *testing.T) {
	h := newHarness(t, "GET", `{"path":{"item":{"from":"k"}},"headers":[{"name":"x-token","value":"node"}]}`)
	h.credentials.headers = http.Header{"X-Token": {"secret"}}
	if _, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"path":{"item":"i"}}`)); FailureCode(err) != CodePolicyDenied || len(h.effects.begun) != 0 {
		t.Fatalf("collision: %v", err)
	}
}

// FU-073: the transport port cannot hold a redirect-following client.
func TestTransportPortRejectsHTTPClient(t *testing.T) {
	port := reflect.TypeOf((*Transport)(nil)).Elem()
	if reflect.TypeOf(&http.Client{}).Implements(port) || reflect.TypeOf(http.Client{}).Implements(port) {
		t.Fatal("an *http.Client satisfies the HTTP action transport port")
	}
	if !reflect.TypeOf(&http.Transport{}).Implements(port) {
		t.Fatal("the guarded *http.Transport no longer satisfies the port")
	}
	if _, err := New(&fakeAdmitter{}, &fakeRules{}, &fakeEffects{}, nil, nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal("service built without a transport")
	}
}

func TestReceiptAndErrorsCarryNoSecretOrBody(t *testing.T) {
	h := newHarness(t, "POST", `{"path":{"item":{"from":"k"}},"body":{"kind":"text","from":"b","content_type":"text/plain"}}`)
	h.respond = func(*http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"leak":"sentinel-response"}`), nil
	}
	receipt, err := h.service.Execute(t.Context(), invocationFor(t, h, 0, `{"body":"sentinel-body","path":{"item":"sentinel-path"}}`))
	if err != nil || receipt.State != "failed" {
		t.Fatal(receipt, err)
	}
	encoded, _ := json.Marshal(receipt)
	for _, sentinel := range []string{"sentinel-secret", "sentinel-body", "sentinel-path", "sentinel-response"} {
		if bytes.Contains(encoded, []byte(sentinel)) {
			t.Fatalf("receipt carries %s", sentinel)
		}
	}
}
