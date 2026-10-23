package llmproxy

// realtime_accept.go — the ONE step of the realtime route that depends on the
// transport the caller arrived on.
//
// Over HTTP the caller's WebSocket ends here: websocket.Accept hijacks the
// connection. Over the GatewayService Realtime RPC (internal/grpcapi) the
// caller's WebSocket ends at elitea-main, and its messages ride a gRPC stream.
// Everything before the accept (mapModel, the price gate, checkBudget, the
// session bound) and everything after it (the provider dial, the pump, billing)
// is the same code for both.
//
// THE CHECKS ARE THE SAME. CheckRealtimeHandshake repeats, byte for byte in
// status and message, what websocket.Accept refuses: a request that is not a
// version-13 handshake, and an Origin the allowlist does not admit. The Origin
// rule is the library's: an absent Origin passes, an Origin whose host equals
// the request Host passes, and otherwise a pattern in OriginPatterns must match.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/coder/websocket"
)

// RealtimeAcceptOptions are the accept-side settings of a realtime session.
type RealtimeAcceptOptions struct {
	// Subprotocols are the subprotocols this surface offers.
	Subprotocols []string
	// OriginPatterns is the Origin allowlist (config.RealtimeAllowedOrigins).
	OriginPatterns []string
	// ReadLimit caps one caller message, in bytes.
	ReadLimit int64
}

// RealtimeAcceptor completes the caller side of a realtime handshake. It
// returns the caller's socket and the subprotocol it negotiated. On a refusal
// it has already written the HTTP response to w and returns an error.
type RealtimeAcceptor func(w http.ResponseWriter, r *http.Request, opts RealtimeAcceptOptions) (RealtimeSocket, string, error)

type realtimeAcceptorKey struct{}

// WithRealtimeAcceptor returns a context whose realtime route accepts through
// accept instead of websocket.Accept. internal/grpcapi sets it on every
// request it builds for the Realtime RPC.
func WithRealtimeAcceptor(ctx context.Context, accept RealtimeAcceptor) context.Context {
	return context.WithValue(ctx, realtimeAcceptorKey{}, accept)
}

// RealtimeAcceptorFrom returns the acceptor of ctx, or the WebSocket one. The
// realtime handler calls it; it is exported for the gRPC bridge's tests.
func RealtimeAcceptorFrom(ctx context.Context) RealtimeAcceptor {
	if accept, ok := ctx.Value(realtimeAcceptorKey{}).(RealtimeAcceptor); ok && accept != nil {
		return accept
	}
	return acceptWebSocket
}

// acceptWebSocket is the HTTP transport's accept: the caller's connection is
// hijacked and served as a WebSocket.
func acceptWebSocket(w http.ResponseWriter, r *http.Request, opts RealtimeAcceptOptions) (RealtimeSocket, string, error) {
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   opts.Subprotocols,
		OriginPatterns: opts.OriginPatterns,
	})
	if err != nil {
		return nil, "", err
	}
	if opts.ReadLimit > 0 {
		client.SetReadLimit(opts.ReadLimit)
	}
	return &wsSocket{conn: client}, client.Subprotocol(), nil
}

// CheckRealtimeHandshake applies websocket.Accept's refusals to r without
// hijacking anything. On a refusal it writes the same status and plain-text
// message websocket.Accept writes and returns the error. On success it
// returns the subprotocol websocket.Accept would have negotiated, and sets the
// Sec-WebSocket-Protocol response header when there is one.
func CheckRealtimeHandshake(w http.ResponseWriter, r *http.Request, opts RealtimeAcceptOptions) (string, error) {
	if status, err := verifyRealtimeClientRequest(w, r); err != nil {
		http.Error(w, err.Error(), status)
		return "", err
	}
	if err := authenticateRealtimeOrigin(r, opts.OriginPatterns); err != nil {
		if errors.Is(err, path.ErrBadPattern) {
			log.Printf("websocket: %v", err)
			err = errors.New(http.StatusText(http.StatusForbidden))
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return "", err
	}
	subprotocol := selectRealtimeSubprotocol(r, opts.Subprotocols)
	if subprotocol != "" {
		w.Header().Set("Sec-WebSocket-Protocol", subprotocol)
	}
	return subprotocol, nil
}

// verifyRealtimeClientRequest is coder/websocket's verifyClientRequest.
func verifyRealtimeClientRequest(w http.ResponseWriter, r *http.Request) (int, error) {
	if !r.ProtoAtLeast(1, 1) {
		return http.StatusUpgradeRequired, fmt.Errorf("WebSocket protocol violation: handshake request must be at least HTTP/1.1: %q", r.Proto)
	}
	if !headerHasToken(r.Header, "Connection", "Upgrade") {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		return http.StatusUpgradeRequired, fmt.Errorf("WebSocket protocol violation: Connection header %q does not contain Upgrade", r.Header.Get("Connection"))
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		return http.StatusUpgradeRequired, fmt.Errorf("WebSocket protocol violation: Upgrade header %q does not contain websocket", r.Header.Get("Upgrade"))
	}
	if r.Method != http.MethodGet {
		return http.StatusMethodNotAllowed, fmt.Errorf("WebSocket protocol violation: handshake request method is not GET but %q", r.Method)
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return http.StatusBadRequest, fmt.Errorf("unsupported WebSocket protocol version (only 13 is supported): %q", r.Header.Get("Sec-WebSocket-Version"))
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) == 0 {
		return http.StatusBadRequest, errors.New("WebSocket protocol violation: missing Sec-WebSocket-Key")
	}
	if len(keys) > 1 {
		return http.StatusBadRequest, errors.New("WebSocket protocol violation: multiple Sec-WebSocket-Key headers")
	}
	key := strings.TrimSpace(keys[0])
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return http.StatusBadRequest, fmt.Errorf("WebSocket protocol violation: invalid Sec-WebSocket-Key %q, must be a 16 byte base64 encoded string", key)
	}
	return 0, nil
}

// authenticateRealtimeOrigin is coder/websocket's authenticateOrigin.
func authenticateRealtimeOrigin(r *http.Request, patterns []string) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("failed to parse Origin header %q: %w", origin, err)
	}
	if strings.EqualFold(r.Host, u.Host) {
		return nil
	}
	for _, pattern := range patterns {
		target := u.Host
		if strings.Contains(pattern, "://") {
			target = u.Scheme + "://" + u.Host
		}
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
		if err != nil {
			return fmt.Errorf("failed to parse path pattern %q: %w", pattern, err)
		}
		if matched {
			return nil
		}
	}
	if u.Host == "" {
		return fmt.Errorf("request Origin %q is not a valid URL with a host", origin)
	}
	return fmt.Errorf("request Origin %q is not authorized for Host %q", u.Host, r.Host)
}

// selectRealtimeSubprotocol is coder/websocket's selectSubprotocol.
func selectRealtimeSubprotocol(r *http.Request, offered []string) string {
	asked := headerTokens(r.Header, "Sec-WebSocket-Protocol")
	for _, sp := range offered {
		for _, cp := range asked {
			if strings.EqualFold(sp, cp) {
				return cp
			}
		}
	}
	return ""
}

func headerHasToken(h http.Header, key, token string) bool {
	for _, t := range headerTokens(h, key) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}

func headerTokens(h http.Header, key string) []string {
	var tokens []string
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	return tokens
}
