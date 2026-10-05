// realtime_dial.go — the production RealtimeDialer, over bifrost/core v1.7.3.
//
// bifrost owns NO socket for the realtime surface. There is no
// core.RealtimeRequest method: the provider supplies a URL, a header set and a
// subprotocol, and the CALLER dials, frames and pumps. Core's own reference use
// of the sequence is internal/llmtests/realtime.go. This file is that sequence
// and nothing else, so the pump in realtime.go depends on a port instead of on
// the core client.

package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// The dial failures a caller can act on. They are values, not formatted
// strings, so the handler can tell a configuration fault from an outage without
// matching on text.
var (
	// ErrRealtimeProviderUnknown means core holds no provider under that key —
	// the credential's provider type names something core does not implement.
	ErrRealtimeProviderUnknown = errors.New("realtime: the provider is not known to bifrost/core")
	// ErrRealtimeUnsupported means the provider exists but serves no realtime
	// API. Most providers are in this class.
	ErrRealtimeUnsupported = errors.New("realtime: the provider does not serve a realtime API")
	// ErrRealtimeNoCredential means the project has no usable key for the
	// provider. Nothing is logged about the key itself.
	ErrRealtimeNoCredential = errors.New("realtime: no credential resolved for the provider")
)

// bifrostRealtimeDialer opens provider realtime sockets through the embedded
// core client.
type bifrostRealtimeDialer struct {
	core   *bifrost.Bifrost
	logger *slog.Logger
}

// NewBifrostRealtimeDialer wraps an embedded bifrost/core client as a
// RealtimeDialer. A nil core returns nil, so a gateway with no core wires no
// realtime route rather than one that panics on the first upgrade.
func NewBifrostRealtimeDialer(core *bifrost.Bifrost, logger *slog.Logger) RealtimeDialer {
	if core == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &bifrostRealtimeDialer{core: core, logger: logger}
}

// DialRealtime performs the whole v1.7.3 realtime handshake sequence.
func (d *bifrostRealtimeDialer) DialRealtime(
	ctx *schemas.BifrostContext,
	provider schemas.ModelProvider,
	model string,
	params url.Values,
) (*RealtimeUpstream, error) {
	p := d.core.GetProviderByKey(provider)
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrRealtimeProviderUnknown, provider)
	}
	rt, ok := p.(schemas.RealtimeProvider)
	if !ok || !rt.SupportsRealtimeAPI() {
		return nil, fmt.Errorf("%w: %s", ErrRealtimeUnsupported, provider)
	}
	// The usage extractor is a SECOND optional interface, and the route cannot
	// bill without it. Refusing here keeps the money rule ("every /llm route is
	// gated and billed") true by construction: a provider whose turns could
	// never be priced never opens a session.
	usage, ok := p.(schemas.RealtimeUsageExtractor)
	if !ok {
		return nil, fmt.Errorf("%w: %s reports no realtime usage", ErrRealtimeUnsupported, provider)
	}

	// The key selection honours the realtime-only AllowedRequests gate, which is
	// why it passes schemas.RealtimeRequest rather than reusing a chat path.
	key, err := d.core.SelectKeyForProviderRequestType(ctx, schemas.RealtimeRequest, provider, model)
	if err != nil {
		return nil, fmt.Errorf("realtime: select key for %s: %w", provider, err)
	}
	if key.Value.GetValue() == "" {
		// core answers a ZERO Key with a NIL error when the supported-key list
		// is empty — bifrost.go, SelectKeyForProviderRequestType:
		// "if len(supportedKeys) == 0 { return schemas.Key{}, nil }". Reading
		// that as success dials the provider with an empty bearer token and
		// gets back an opaque 401, which an operator cannot tell from a real
		// provider outage.
		//
		// This is the SECOND road to that refusal, not the only one: an account
		// whose key carries an empty Value is rejected earlier, by core's own
		// key filter, which returns an error instead. Both end in a refusal.
		// TestDialRealtime_RefusesAnAccountWithNoUsableCredential pins the
		// refusal itself for that reason, and can only reach the earlier road.
		return nil, fmt.Errorf("%w: %s", ErrRealtimeNoCredential, provider)
	}
	recordRealtimeSelectedKey(ctx, key)

	return openRealtimeSocket(ctx, provider, rt, usage, key, model, params)
}

// recordRealtimeSelectedKey writes the id of the key that serves the session
// onto the session's BifrostContext (legacy issue 6709).
//
// bifrost/core writes BifrostContextKeySelectedKeyID only in its own request
// executor. SelectKeyForProviderRequestType does not write it, and realtime
// never goes through that executor. Without this line every billed realtime
// turn logged an empty credential_owner, although a project or platform
// credential served it. The session keeps this ctx for its whole life, so each
// turn's updateUsageUnits reads the id back through
// account.SelectedCredentialOwner.
func recordRealtimeSelectedKey(ctx *schemas.BifrostContext, key schemas.Key) {
	if ctx == nil || key.ID == "" {
		return
	}
	ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, key.ID)
}

// openRealtimeSocket is the half of the handshake that needs NO bifrost core
// client: it builds the URL and the headers from the provider interfaces, dials,
// pins the read limit and reports the NEGOTIATED subprotocol.
//
// WHY IT IS A SEPARATE FUNCTION. DialRealtime's first three steps take a
// *bifrost.Bifrost, which is a concrete struct with no interface behind it. A
// test can therefore only reach them with a live core client, and the whole
// function measured 0.0% coverage as a result — for the one piece of code that
// touches an API nobody here had used before. Everything below the key
// selection is provider-interface work, so a stub schemas.RealtimeProvider
// drives all of it. See realtime_dial_test.go.
func openRealtimeSocket(
	ctx *schemas.BifrostContext,
	provider schemas.ModelProvider,
	rt schemas.RealtimeProvider,
	usage schemas.RealtimeUsageExtractor,
	key schemas.Key,
	model string,
	params url.Values,
) (*RealtimeUpstream, error) {
	// The URL already carries ?model=; do not append it again. The allowlisted
	// caller parameters are merged onto it — `intent` above all, which selects
	// the provider's TRANSCRIPTION session mode and which the only known caller
	// sends on every dial.
	wsURL, uErr := realtimeProviderURL(rt.RealtimeWebSocketURL(key, model), params)
	if uErr != nil {
		// The URL is NOT logged: it is built from the credential's base URL,
		// which can carry userinfo.
		return nil, fmt.Errorf("realtime: build the provider URL for %s: %w", provider, uErr)
	}
	hdrs, hErr := rt.RealtimeHeaders(ctx, key)
	if hErr != nil {
		return nil, fmt.Errorf("realtime: build headers for %s: %v", provider, hErr)
	}
	header := http.Header{}
	for k, v := range hdrs {
		header.Set(k, v)
	}

	// The handshake gets its OWN deadline. The session context deliberately has
	// none — a call runs as long as the caller talks — so without this a
	// black-holed provider would hold the caller's HTTP request open forever.
	dialCtx, cancel := context.WithTimeout(ctx, realtimeDialTimeout)
	defer cancel()

	conn, resp, dErr := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader:   header,
		Subprotocols: realtimeSubprotocols(rt.RealtimeWebSocketSubprotocol()),
	})
	if resp != nil && resp.Body != nil {
		// Dial hands back the handshake response on failure. Its body is never
		// logged: a provider error body can echo request headers, and those
		// carry the bearer token.
		_ = resp.Body.Close()
	}
	if dErr != nil {
		// The URL is NOT logged. RealtimeWebSocketURL is built from the
		// credential's base URL, which can carry userinfo.
		return nil, fmt.Errorf("realtime: dial %s: %w", provider, dErr)
	}
	// Without this the session dies mid-call with close status 1009 the first
	// time a base64 audio frame passes 32 KiB. See realtimeReadLimit.
	conn.SetReadLimit(realtimeReadLimit)

	return &RealtimeUpstream{
		Socket: &wsSocket{conn: conn},
		Codec:  &bifrostRealtimeCodec{provider: rt, usage: usage},
		// The NEGOTIATED subprotocol, never the one the provider DECLARED.
		// RealtimeWebSocketSubprotocol is only what this dial OFFERED, and a
		// provider is free to answer with none. Reporting the declared value
		// made the gateway state a negotiation that may not have happened, and
		// the session log then named a subprotocol the provider had refused.
		Subprotocol: conn.Subprotocol(),
	}, nil
}

// bifrostRealtimeCodec joins the two optional provider interfaces into the one
// surface the pump needs.
type bifrostRealtimeCodec struct {
	provider schemas.RealtimeProvider
	usage    schemas.RealtimeUsageExtractor
}

func (c *bifrostRealtimeCodec) ToBifrostRealtimeEvent(raw json.RawMessage) (*schemas.BifrostRealtimeEvent, error) {
	return c.provider.ToBifrostRealtimeEvent(raw)
}

func (c *bifrostRealtimeCodec) ToProviderRealtimeEvent(ev *schemas.BifrostRealtimeEvent) (json.RawMessage, error) {
	return c.provider.ToProviderRealtimeEvent(ev)
}

func (c *bifrostRealtimeCodec) ShouldStartRealtimeTurn(ev *schemas.BifrostRealtimeEvent) bool {
	return c.provider.ShouldStartRealtimeTurn(ev)
}

func (c *bifrostRealtimeCodec) ExtractRealtimeTurnUsage(raw []byte) *schemas.BifrostLLMUsage {
	return c.usage.ExtractRealtimeTurnUsage(raw)
}

// realtimeProviderURL merges the allowlisted caller parameters onto the
// provider's realtime URL.
//
// It applies realtimeForwardedQuery a SECOND time, on purpose. The handler
// already filters, but this function is the only place a caller-authored string
// can reach the provider URL, and a port whose safety depends on every caller
// remembering to filter is a port that will be called unfiltered one day.
//
// A parameter the provider URL already carries is NEVER overwritten. That is
// what keeps `model` the gateway's: even if the allowlist grew a name bifrost
// also sets, the provider's own value wins.
func realtimeProviderURL(base string, params url.Values) (string, error) {
	if len(params) == 0 {
		return base, nil
	}
	u, err := url.Parse(base)
	if err != nil {
		// url.Parse reports a *url.Error, and that error QUOTES the URL it
		// failed on. The base comes from the credential's base URL, which can
		// carry userinfo, so returning it verbatim puts a credential into the
		// very log line every caller here is careful not to print. Only the
		// reason travels.
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			return "", fmt.Errorf("the provider realtime URL cannot be parsed: %w", uerr.Err)
		}
		return "", errors.New("the provider realtime URL cannot be parsed")
	}
	q := u.Query()
	for name, values := range realtimeForwardedQuery(params) {
		if _, taken := q[name]; taken {
			continue
		}
		for _, v := range values {
			q.Add(name, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var _ RealtimeCodec = (*bifrostRealtimeCodec)(nil)
