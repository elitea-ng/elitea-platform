package llmproxy

// realtime_dial_test.go — the PRODUCTION realtime dialer.
//
// WHY THIS FILE EXISTS. Every test in realtime_test.go injects a fake dialer,
// because the pump is what those tests are about. The consequence was measured:
// bifrostRealtimeDialer.DialRealtime and all four bifrostRealtimeCodec methods
// reported 0.0% coverage. The one piece of this route that touches the bifrost
// realtime API — an API nobody here had used before — was the one piece nothing
// ran.
//
// WHAT IT CAN AND CANNOT REACH. DialRealtime's first three steps take a
// *bifrost.Bifrost, a concrete struct with no interface behind it, so they need
// a live core client. Everything after the key selection is work against the
// two optional PROVIDER interfaces, and openRealtimeSocket holds exactly that
// part. A stub schemas.RealtimeProvider and a real WebSocket listener drive all
// of it: the URL, the headers, the offered subprotocol, the NEGOTIATED
// subprotocol and the provider-side read limit.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// ── the stub provider ───────────────────────────────────────────────────────

// stubRealtimeProvider satisfies BOTH optional interfaces the dial needs:
// schemas.RealtimeProvider and schemas.RealtimeUsageExtractor. It records what
// the dial asked it for, so a test can assert the arguments as well as the
// result.
type stubRealtimeProvider struct {
	url         string
	headers     map[string]string
	headerErr   *schemas.BifrostError
	subprotocol string

	// The four codec answers, so the delegation test can tell each apart.
	bifrostEvent  *schemas.BifrostRealtimeEvent
	bifrostErr    error
	providerEvent json.RawMessage
	providerErr   error
	turnStart     bool
	usage         *schemas.BifrostLLMUsage

	mu        sync.Mutex
	urlKey    schemas.Key
	urlModel  string
	hdrKey    schemas.Key
	lastRaw   json.RawMessage
	lastEvent *schemas.BifrostRealtimeEvent
	lastUsage []byte
}

func (p *stubRealtimeProvider) SupportsRealtimeAPI() bool { return true }

func (p *stubRealtimeProvider) RealtimeWebSocketURL(key schemas.Key, model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urlKey, p.urlModel = key, model
	return p.url
}

func (p *stubRealtimeProvider) RealtimeHeaders(_ *schemas.BifrostContext, key schemas.Key) (map[string]string, *schemas.BifrostError) {
	p.mu.Lock()
	p.hdrKey = key
	p.mu.Unlock()
	if p.headerErr != nil {
		return nil, p.headerErr
	}
	return p.headers, nil
}

func (p *stubRealtimeProvider) SupportsRealtimeWebRTC() bool { return false }

func (p *stubRealtimeProvider) ExchangeRealtimeWebRTCSDP(
	*schemas.BifrostContext, schemas.Key, string, string, json.RawMessage,
) (string, *schemas.BifrostError) {
	return "", nil
}

func (p *stubRealtimeProvider) ToBifrostRealtimeEvent(raw json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	p.mu.Lock()
	p.lastRaw = raw
	p.mu.Unlock()
	return p.bifrostEvent, p.bifrostErr
}

func (p *stubRealtimeProvider) ToProviderRealtimeEvent(ev *schemas.BifrostRealtimeEvent) (json.RawMessage, error) {
	p.mu.Lock()
	p.lastEvent = ev
	p.mu.Unlock()
	return p.providerEvent, p.providerErr
}

func (p *stubRealtimeProvider) ShouldStartRealtimeTurn(ev *schemas.BifrostRealtimeEvent) bool {
	p.mu.Lock()
	p.lastEvent = ev
	p.mu.Unlock()
	return p.turnStart
}

func (p *stubRealtimeProvider) RealtimeTurnFinalEvent() schemas.RealtimeEventType {
	return schemas.RTEventResponseDone
}

func (p *stubRealtimeProvider) RealtimeWebRTCDataChannelLabel() string { return "" }

func (p *stubRealtimeProvider) RealtimeWebSocketSubprotocol() string { return p.subprotocol }

func (p *stubRealtimeProvider) ShouldForwardRealtimeEvent(*schemas.BifrostRealtimeEvent) bool {
	return true
}

func (p *stubRealtimeProvider) ShouldAccumulateRealtimeOutput(schemas.RealtimeEventType) bool {
	return false
}

// ExtractRealtimeTurnUsage and ExtractRealtimeTurnOutput are the
// RealtimeUsageExtractor half.
func (p *stubRealtimeProvider) ExtractRealtimeTurnUsage(raw []byte) *schemas.BifrostLLMUsage {
	p.mu.Lock()
	p.lastUsage = append([]byte(nil), raw...)
	p.mu.Unlock()
	return p.usage
}

func (p *stubRealtimeProvider) ExtractRealtimeTurnOutput([]byte) *schemas.ChatMessage { return nil }

var (
	_ schemas.RealtimeProvider       = (*stubRealtimeProvider)(nil)
	_ schemas.RealtimeUsageExtractor = (*stubRealtimeProvider)(nil)
)

// ── the stub provider listener ──────────────────────────────────────────────

// stubProviderServer is a real WebSocket listener that stands in for the
// provider. It records the handshake the dial performed, so the URL and the
// headers are asserted as the PROVIDER saw them and not as this file built them.
type stubProviderServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	query  url.Values
	header http.Header
	conns  chan *websocket.Conn
}

// newStubProviderServer starts a listener that accepts a WebSocket handshake and
// negotiates one of `offer`. An empty `offer` negotiates NO subprotocol, which
// is the case decision Q turns on.
func newStubProviderServer(t *testing.T, offer []string) *stubProviderServer {
	t.Helper()
	s := &stubProviderServer{conns: make(chan *websocket.Conn, 4)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.query = r.URL.Query()
		s.header = r.Header.Clone()
		s.mu.Unlock()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:       offer,
			InsecureSkipVerify: true, // a test client sends no Origin this listener could match
		})
		if err != nil {
			return
		}
		select {
		case s.conns <- conn:
		default:
			conn.CloseNow() //nolint:errcheck // best effort in a test listener
		}
		<-r.Context().Done()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// wsURL returns the ws:// base of the listener.
func (s *stubProviderServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

// handshake returns the query and the headers the provider side received.
func (s *stubProviderServer) handshake() (url.Values, http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.query, s.header
}

// accepted returns the provider-side connection of the newest session.
func (s *stubProviderServer) accepted(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-s.conns:
		t.Cleanup(func() { _ = c.CloseNow() })
		return c
	case <-t.Context().Done():
		t.Fatal("the stub provider never accepted a connection")
		return nil
	}
}

// dialTestKey is the credential the stub resolves. Its value is never asserted
// against a log line: nothing in the dial may print it.
func dialTestKey() schemas.Key {
	return schemas.Key{ID: "k1", Value: schemas.SecretVar{Val: "sk-realtime-test"}}
}

func dialTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

// ── the handshake the dialer builds ─────────────────────────────────────────

// TestOpenRealtimeSocket_BuildsTheURLTheHeadersAndTheOffer covers the whole
// provider-facing handshake in one dial.
//
// The three things asserted are the three a caller cannot see from the outside:
// the provider's own `model` survives, only the ALLOWLISTED caller parameter
// travels with it, and the provider's headers reach the wire. A caller-supplied
// `api-key` is in the input on purpose — the handler filters already, and this
// asserts that the port filters too.
func TestOpenRealtimeSocket_BuildsTheURLTheHeadersAndTheOffer(t *testing.T) {
	srv := newStubProviderServer(t, []string{realtimeSubprotocol})
	prov := &stubRealtimeProvider{
		url:         srv.wsURL() + "/v1/realtime?model=gpt-4o-realtime-preview",
		headers:     map[string]string{"Authorization": "Bearer sk-realtime-test", "OpenAI-Beta": "realtime=v1"},
		subprotocol: realtimeSubprotocol,
	}

	up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(),
		"gpt-4o-realtime-preview",
		url.Values{
			"intent":  []string{"transcription"},
			"api-key": []string{"a caller-chosen credential"},
			"model":   []string{"a caller-chosen model"},
		})
	if err != nil {
		t.Fatalf("openRealtimeSocket: %v", err)
	}
	defer up.Socket.Close(RealtimeCloseNormal, "done")
	srv.accepted(t)

	q, hdr := srv.handshake()
	if got := q["model"]; len(got) != 1 || got[0] != "gpt-4o-realtime-preview" {
		t.Errorf("provider saw model=%v, want exactly [gpt-4o-realtime-preview]: the caller's `model` "+
			"must never reach the provider URL, and the provider's own must never be duplicated", got)
	}
	if got := q.Get("intent"); got != "transcription" {
		t.Errorf("provider saw intent=%q, want \"transcription\": `intent` selects the TRANSCRIPTION "+
			"session mode and the only known caller sends it on every dial", got)
	}
	if _, present := q["api-key"]; present {
		t.Error("the caller's `api-key` reached the provider URL; the allowlist must be applied by this port too")
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-realtime-test" {
		t.Errorf("Authorization = %q, want the provider's own header: RealtimeHeaders is what authenticates the dial", got)
	}
	if got := hdr.Get("OpenAI-Beta"); got != "realtime=v1" {
		t.Errorf("OpenAI-Beta = %q, want realtime=v1: every header RealtimeHeaders returns must reach the wire", got)
	}
	if got := hdr.Get("Sec-WebSocket-Protocol"); got != realtimeSubprotocol {
		t.Errorf("offered subprotocol = %q, want %q: the provider's declared subprotocol is what the dial OFFERS",
			got, realtimeSubprotocol)
	}

	prov.mu.Lock()
	gotModel, gotKey := prov.urlModel, prov.hdrKey.ID
	prov.mu.Unlock()
	if gotModel != "gpt-4o-realtime-preview" {
		t.Errorf("RealtimeWebSocketURL got model %q, want the MAPPED provider name", gotModel)
	}
	if gotKey != "k1" {
		t.Errorf("RealtimeHeaders got key %q, want the key the account resolved", gotKey)
	}
}

// TestOpenRealtimeSocket_ReportsTheNegotiatedSubprotocol pins the value the
// session carries.
//
// The upstream used to report RealtimeWebSocketSubprotocol — what the dial
// OFFERED. An offer is not an agreement: a provider may answer with no
// subprotocol at all, and the gateway then logged a negotiation that never
// happened. The value must come off the opened connection.
func TestOpenRealtimeSocket_ReportsTheNegotiatedSubprotocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serverOK []string
		want     string
	}{
		{"the provider agrees", []string{realtimeSubprotocol}, realtimeSubprotocol},
		{"the provider answers with none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStubProviderServer(t, tc.serverOK)
			// The provider DECLARES "realtime" in both cases. Only the answer differs.
			prov := &stubRealtimeProvider{url: srv.wsURL() + "/v1/realtime", subprotocol: realtimeSubprotocol}

			up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(), "m", nil)
			if err != nil {
				t.Fatalf("openRealtimeSocket: %v", err)
			}
			defer up.Socket.Close(RealtimeCloseNormal, "done")
			srv.accepted(t)

			if up.Subprotocol != tc.want {
				t.Fatalf("upstream subprotocol = %q, want %q: the session must carry the NEGOTIATED "+
					"value, never the one the provider declared", up.Subprotocol, tc.want)
			}
		})
	}
}

// TestOpenRealtimeSocket_PinsTheProviderSideReadLimit is the provider half of
// the 1009 defect.
//
// coder/websocket reads at most 32768 bytes per message by default. A realtime
// frame carries base64 audio well past that, and the failure is not visible at
// setup: the session opens, works, and then dies mid-call with close status
// 1009. The client half of this limit has a test; the provider half had none,
// so deleting the SetReadLimit call below left the whole suite green.
func TestOpenRealtimeSocket_PinsTheProviderSideReadLimit(t *testing.T) {
	srv := newStubProviderServer(t, nil)
	prov := &stubRealtimeProvider{url: srv.wsURL() + "/v1/realtime"}

	up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(), "m", nil)
	if err != nil {
		t.Fatalf("openRealtimeSocket: %v", err)
	}
	defer up.Socket.Close(RealtimeCloseNormal, "done")
	conn := srv.accepted(t)

	// Well past the library default of 32768 and well inside realtimeReadLimit,
	// which is the range every real audio delta lands in.
	const size = 64 << 10
	frame := append([]byte(`{"type":"response.audio.delta","delta":"`), make([]byte, size)...)
	for i := len(`{"type":"response.audio.delta","delta":"`); i < len(frame); i++ {
		frame[i] = 'A'
	}
	frame = append(frame, []byte(`"}`)...)
	if size <= 32768 {
		t.Fatalf("the probe frame is %d bytes; it must exceed the library default of 32768 "+
			"or this test cannot see a missing read limit", size)
	}
	if int64(len(frame)) >= realtimeReadLimit {
		t.Fatalf("the probe frame is %d bytes; it must stay under realtimeReadLimit (%d)",
			len(frame), realtimeReadLimit)
	}

	writeErr := make(chan error, 1)
	go func() { writeErr <- conn.Write(t.Context(), websocket.MessageText, frame) }()

	got, rerr := up.Socket.Read(t.Context())
	if rerr != nil {
		t.Fatalf("reading a %d-byte provider frame failed: %v — the dial must raise the read limit "+
			"above the library default, or a live call dies with close status 1009 minutes in",
			len(frame), rerr)
	}
	if len(got) != len(frame) {
		t.Fatalf("read %d bytes, want %d: the frame arrived truncated", len(got), len(frame))
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("the provider write failed: %v", werr)
	}
}

// TestOpenRealtimeSocket_HeaderFailureIsReported keeps a provider that cannot
// build its own auth headers from producing a dial with none.
func TestOpenRealtimeSocket_HeaderFailureIsReported(t *testing.T) {
	srv := newStubProviderServer(t, nil)
	boom := "the credential has no realtime scope"
	prov := &stubRealtimeProvider{
		url:       srv.wsURL() + "/v1/realtime",
		headerErr: &schemas.BifrostError{Error: &schemas.ErrorField{Message: boom}},
	}

	up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(), "m", nil)
	if err == nil {
		up.Socket.Close(RealtimeCloseNormal, "done")
		t.Fatal("a header failure opened a session; the dial must refuse rather than authenticate with nothing")
	}
	if !strings.Contains(err.Error(), "build headers") {
		t.Errorf("error = %v, want it to name the header step", err)
	}
}

// TestOpenRealtimeSocket_UnparsableProviderURLIsReported covers the branch that
// must NOT log what it failed on: the base URL comes from the credential and can
// carry userinfo.
func TestOpenRealtimeSocket_UnparsableProviderURLIsReported(t *testing.T) {
	prov := &stubRealtimeProvider{url: "://not-a-url"}

	up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(), "m",
		url.Values{"intent": []string{"transcription"}})
	if err == nil {
		up.Socket.Close(RealtimeCloseNormal, "done")
		t.Fatal("an unparsable provider URL opened a session")
	}
	if strings.Contains(err.Error(), "://not-a-url") {
		t.Errorf("error = %v; it must not repeat the provider URL, which can carry userinfo", err)
	}
}

// TestOpenRealtimeSocket_DialFailureNeverRepeatsTheURL covers the same rule on
// the dial itself, which is the branch a real outage takes.
func TestOpenRealtimeSocket_DialFailureNeverRepeatsTheURL(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no realtime here", http.StatusUnauthorized)
	}))
	defer refuse.Close()
	base := "ws" + strings.TrimPrefix(refuse.URL, "http") + "/v1/realtime"
	prov := &stubRealtimeProvider{url: base}

	up, err := openRealtimeSocket(dialTestContext(), "openai", prov, prov, dialTestKey(), "m", nil)
	if err == nil {
		up.Socket.Close(RealtimeCloseNormal, "done")
		t.Fatal("the dial reported success against a listener that refused the handshake")
	}
	if strings.Contains(err.Error(), base) {
		t.Errorf("error = %v; it must not repeat the provider URL", err)
	}
	if !strings.Contains(err.Error(), "dial openai") {
		t.Errorf("error = %v, want it to name the provider and the dial step", err)
	}
}

// ── the codec adapters ──────────────────────────────────────────────────────

// TestBifrostRealtimeCodec_DelegatesToBothProviderInterfaces covers the four
// adapter methods.
//
// They are one line each, which is exactly why they were never run and exactly
// how a crossed wire survives: the codec joins TWO interfaces, and
// ExtractRealtimeTurnUsage must come from the usage extractor while the other
// three come from the provider. A codec that read usage off the wrong value
// bills nothing and no test would have noticed.
func TestBifrostRealtimeCodec_DelegatesToBothProviderInterfaces(t *testing.T) {
	wantEvent := &schemas.BifrostRealtimeEvent{Type: schemas.RTEventResponseDone}
	wantRaw := json.RawMessage(`{"type":"response.done"}`)
	wantUsage := &schemas.BifrostLLMUsage{PromptTokens: 11, CompletionTokens: 22}
	prov := &stubRealtimeProvider{
		bifrostEvent:  wantEvent,
		providerEvent: wantRaw,
		turnStart:     true,
		usage:         wantUsage,
	}
	// The provider and the usage extractor are DELIBERATELY two different
	// values here. A method that reads the wrong field returns the other one's
	// answer, and the assertions below separate them.
	other := &stubRealtimeProvider{usage: &schemas.BifrostLLMUsage{PromptTokens: 999}}
	codec := &bifrostRealtimeCodec{provider: prov, usage: other}

	in := json.RawMessage(`{"type":"response.done","x":1}`)
	gotEvent, err := codec.ToBifrostRealtimeEvent(in)
	if err != nil {
		t.Fatalf("ToBifrostRealtimeEvent: %v", err)
	}
	if gotEvent != wantEvent {
		t.Errorf("ToBifrostRealtimeEvent returned %v, want the provider's own answer", gotEvent)
	}
	prov.mu.Lock()
	sawRaw := string(prov.lastRaw)
	prov.mu.Unlock()
	if sawRaw != string(in) {
		t.Errorf("the provider decoder saw %q, want the raw frame %q", sawRaw, string(in))
	}

	gotRaw, err := codec.ToProviderRealtimeEvent(wantEvent)
	if err != nil {
		t.Fatalf("ToProviderRealtimeEvent: %v", err)
	}
	if string(gotRaw) != string(wantRaw) {
		t.Errorf("ToProviderRealtimeEvent returned %q, want %q", string(gotRaw), string(wantRaw))
	}
	prov.mu.Lock()
	sawEvent := prov.lastEvent
	prov.mu.Unlock()
	if sawEvent != wantEvent {
		t.Error("the provider encoder did not receive the canonical event it was given")
	}

	if !codec.ShouldStartRealtimeTurn(wantEvent) {
		t.Error("ShouldStartRealtimeTurn returned false; the provider answered true")
	}

	gotUsage := codec.ExtractRealtimeTurnUsage([]byte(`{"response":{"usage":{}}}`))
	if gotUsage != other.usage {
		t.Fatalf("ExtractRealtimeTurnUsage returned %+v, want the USAGE EXTRACTOR's answer (%+v). "+
			"The two interfaces are separate values, and a turn priced off the wrong one bills a wrong number",
			gotUsage, other.usage)
	}
	other.mu.Lock()
	sawUsage := string(other.lastUsage)
	other.mu.Unlock()
	if sawUsage != `{"response":{"usage":{}}}` {
		t.Errorf("the usage extractor saw %q, want the RAW terminal frame", sawUsage)
	}
}

// ── the whole dialer, over a real embedded core ─────────────────────────────

// realtimeCoreAccount is a schemas.Account with ONE provider whose base URL
// points at a listener this test owns. It is what lets the production
// DialRealtime run end to end: core resolves the provider and the key, and
// bifrost's OWN OpenAI provider builds the URL, the headers and the subprotocol.
type realtimeCoreAccount struct {
	baseURL  string
	keyValue string
	// keyID is the Key ID the account offers. Empty means "realtime-key".
	keyID string
}

func (realtimeCoreAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (a realtimeCoreAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	id := a.keyID
	if id == "" {
		id = "realtime-key"
	}
	return []schemas.Key{{
		ID:     id,
		Value:  schemas.SecretVar{Val: a.keyValue},
		Models: []string{"*"},
		Weight: 1,
	}}, nil
}

func (a realtimeCoreAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{
		NetworkConfig:            schemas.NetworkConfig{BaseURL: a.baseURL},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 1},
	}, nil
}

// TestDialRealtime_ThroughTheEmbeddedCore runs the PRODUCTION dialer end to end.
//
// Nothing is stubbed below the account: core resolves the provider and the key,
// and bifrost's own OpenAI provider builds the URL, the headers and the
// subprotocol. It is the only test that proves the v1.7.3 handshake sequence in
// DialRealtime is the right sequence — the interface assertions, the
// realtime-only key selection, and the two optional interfaces the codec joins.
func TestDialRealtime_ThroughTheEmbeddedCore(t *testing.T) {
	srv := newStubProviderServer(t, []string{realtimeSubprotocol})
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: realtimeCoreAccount{
			// The provider rewrites http:// to ws:// itself.
			baseURL:  "http" + strings.TrimPrefix(srv.wsURL(), "ws"),
			keyValue: "sk-realtime-core",
		},
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	defer core.Shutdown()

	d := NewBifrostRealtimeDialer(core, nil)
	if d == nil {
		t.Fatal("NewBifrostRealtimeDialer returned nil for a non-nil core")
	}
	up, derr := d.DialRealtime(dialTestContext(), schemas.OpenAI, "gpt-4o-realtime-preview",
		url.Values{"intent": []string{"transcription"}})
	if derr != nil {
		t.Fatalf("DialRealtime: %v", derr)
	}
	defer up.Socket.Close(RealtimeCloseNormal, "done")
	srv.accepted(t)

	q, hdr := srv.handshake()
	if got := q.Get("model"); got != "gpt-4o-realtime-preview" {
		t.Errorf("provider saw model=%q, want the mapped model", got)
	}
	if got := q.Get("intent"); got != "transcription" {
		t.Errorf("provider saw intent=%q; `intent` is the one allowlisted caller parameter", got)
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-realtime-core" {
		t.Errorf("Authorization = %q, want the key core resolved", got)
	}
	if up.Subprotocol != realtimeSubprotocol {
		t.Errorf("negotiated subprotocol = %q, want %q", up.Subprotocol, realtimeSubprotocol)
	}
	if up.Codec == nil {
		t.Fatal("the upstream carries no codec; the route cannot price a turn without one")
	}
	// The codec must be the JOIN of the two optional interfaces, not the
	// provider alone: ExtractRealtimeTurnUsage comes from the usage extractor.
	if _, ok := up.Codec.(*bifrostRealtimeCodec); !ok {
		t.Fatalf("codec is %T, want *bifrostRealtimeCodec", up.Codec)
	}
}

// TestDialRealtime_RefusesAnAccountWithNoUsableCredential pins the outcome that
// nothing else asserted: a project whose account resolves no usable key must
// NEVER reach the provider socket.
//
// It asserts the OUTCOME, not a mechanism, and that is deliberate. DialRealtime
// carries an explicit guard for a zero Key returned with a NIL error, because
// core does exactly that when the supported-key list is empty
// (bifrost.go SelectKeyForProviderRequestType: "if len(supportedKeys) == 0 {
// return schemas.Key{}, nil }"). This test cannot reach that branch: an account
// whose key carries an empty Value is rejected EARLIER, by core's own key
// filter, which returns an error instead. Both roads end in a refusal, which is
// the property that matters, so the test pins the refusal and names the two
// roads rather than asserting one sentinel and passing for the wrong reason.
func TestDialRealtime_RefusesAnAccountWithNoUsableCredential(t *testing.T) {
	srv := newStubProviderServer(t, []string{realtimeSubprotocol})
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: realtimeCoreAccount{
			baseURL: "http" + strings.TrimPrefix(srv.wsURL(), "ws"),
			// The account resolved NO usable credential.
			keyValue: "",
		},
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	defer core.Shutdown()

	d := NewBifrostRealtimeDialer(core, nil)
	up, derr := d.DialRealtime(dialTestContext(), schemas.OpenAI, "gpt-4o-realtime-preview", nil)
	if derr == nil {
		if up != nil && up.Socket != nil {
			up.Socket.Close(RealtimeCloseNormal, "unexpected")
		}
		t.Fatal("the dial SUCCEEDED with no usable credential: the provider would receive an " +
			"empty bearer token and answer an opaque 401, which an operator cannot tell from a " +
			"real provider outage")
	}
	// No socket may be handed back on a refusal — a returned upstream would be
	// leaked, because the caller only closes what it believes it opened.
	if up != nil {
		t.Errorf("the dial refused but still returned an upstream (%+v); it must return nil", up)
	}
}

// TestDialRealtime_RefusalsAreDistinguishable covers the three core-side
// refusals. Each is a VALUE error, so the handler can tell a configuration fault
// from an outage without matching on text.
func TestDialRealtime_RefusalsAreDistinguishable(t *testing.T) {
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account:         zeroProviderAccount{},
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	defer core.Shutdown()
	d := NewBifrostRealtimeDialer(core, nil)

	for _, tc := range []struct {
		name     string
		provider schemas.ModelProvider
		wantErr  error
		wantText string
	}{
		{
			name:     "core holds no provider under that key",
			provider: "a-provider-core-does-not-implement",
			wantErr:  ErrRealtimeProviderUnknown,
		},
		{
			name:     "the provider serves no realtime API",
			provider: schemas.Anthropic,
			wantErr:  ErrRealtimeUnsupported,
		},
		{
			// A realtime provider with no key resolved. The session must not
			// open: a dial with an empty bearer token gets an opaque 401 that
			// no operator can act on.
			name:     "no credential is resolved",
			provider: schemas.OpenAI,
			wantText: "select key for openai",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, derr := d.DialRealtime(dialTestContext(), tc.provider, "gpt-4o-realtime-preview", nil)
			if derr == nil {
				up.Socket.Close(RealtimeCloseNormal, "done")
				t.Fatal("the dial reported success")
			}
			if tc.wantErr != nil && !errors.Is(derr, tc.wantErr) {
				t.Fatalf("error = %v, want it to wrap %v: the handler tells these apart by VALUE", derr, tc.wantErr)
			}
			if tc.wantText != "" && !strings.Contains(derr.Error(), tc.wantText) {
				t.Fatalf("error = %v, want it to name %q", derr, tc.wantText)
			}
		})
	}
}

// TestDialRealtime_RecordsTheCredentialOwner pins legacy issue 6709 for the
// realtime route. bifrost/core writes the selected key id only in its own
// request executor, and realtime never runs that executor. So the dialer must
// write the id itself, or every billed realtime turn logs no credential owner.
func TestDialRealtime_RecordsTheCredentialOwner(t *testing.T) {
	for _, tc := range []struct {
		keyID string
		want  string
	}{
		{keyID: "shared:9:openai", want: requestlog.CredentialOwnerPlatform},
		{keyID: "project-credential-1", want: requestlog.CredentialOwnerProject},
	} {
		t.Run(tc.keyID, func(t *testing.T) {
			srv := newStubProviderServer(t, []string{realtimeSubprotocol})
			core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
				Account: realtimeCoreAccount{
					baseURL:  "http" + strings.TrimPrefix(srv.wsURL(), "ws"),
					keyValue: "sk-realtime-owner",
					keyID:    tc.keyID,
				},
				InitialPoolSize: 1,
			})
			if err != nil {
				t.Fatalf("bifrost.Init: %v", err)
			}
			defer core.Shutdown()

			ctx := dialTestContext()
			up, derr := NewBifrostRealtimeDialer(core, nil).DialRealtime(ctx, schemas.OpenAI, "gpt-4o-realtime-preview", nil)
			if derr != nil {
				t.Fatalf("DialRealtime: %v", derr)
			}
			defer up.Socket.Close(RealtimeCloseNormal, "done")
			srv.accepted(t)

			if got := account.SelectedCredentialOwner(ctx); got != tc.want {
				t.Errorf("credential owner after the dial = %q, want %q", got, tc.want)
			}
		})
	}
}

// A key with no id records nothing, and a nil context does not panic.
func TestRecordRealtimeSelectedKey_IgnoresAnEmptyIDAndANilContext(t *testing.T) {
	recordRealtimeSelectedKey(nil, schemas.Key{ID: "k"})
	ctx := dialTestContext()
	recordRealtimeSelectedKey(ctx, schemas.Key{})
	if got := account.SelectedCredentialOwner(ctx); got != "" {
		t.Errorf("credential owner = %q, want empty for a key with no id", got)
	}
}
