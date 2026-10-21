package llmproxy

// proxy_realtime.go — the browser's realtime WebSocket ends here and continues
// as a GatewayService Realtime session.
//
// THE GATEWAY DECIDES THE HANDSHAKE. It runs every admission step, the
// WebSocket handshake checks and the Origin allowlist
// (LLM_REALTIME_ALLOWED_ORIGINS) on the headers this edge forwards, exactly
// as websocket.Accept applies them. A refusal is its HTTP status and body,
// copied to the browser. Only an accepted session (101) is accepted here, and
// the accept skips the Origin check because the gateway already applied it to
// the same request.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	// realtimeReadLimit caps one browser message: the gateway's own cap
	// (realtimeReadLimit in its llmproxy/realtime.go).
	realtimeReadLimit = 1 << 20
	// realtimePingTimeout bounds one browser ping the gateway asked for.
	realtimePingTimeout = 30 * time.Second
	// maxCloseReason is the WebSocket limit of a close reason, in bytes.
	maxCloseReason = 123
)

func (p *Proxy) serveRealtime(w http.ResponseWriter, r *http.Request, req *Request) {
	session, resp, err := p.gateway.Realtime(r.Context(), req)
	if err != nil {
		p.writeHopError(w, r, err)
		return
	}
	if session == nil {
		defer func() { _ = resp.Body.Close() }()
		p.writeResponse(w, r, resp)
		return
	}
	defer session.End()

	opts := &websocket.AcceptOptions{
		// The gateway applied the Origin allowlist to this request.
		InsecureSkipVerify: true,
	}
	if sub := resp.Header.Get("Sec-WebSocket-Protocol"); sub != "" {
		opts.Subprotocols = []string{sub}
	}
	for name, values := range resp.Header {
		if strings.HasPrefix(name, "Sec-Websocket-") || name == "Upgrade" || name == "Connection" {
			continue
		}
		w.Header()[name] = append([]string(nil), values...)
	}
	w.Header().Set("X-Accel-Buffering", "no")
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		// Accept wrote the refusal. The gateway checked the same request, so
		// this is not expected; end the gateway session.
		p.logger.Warn("llmproxy: the realtime accept failed after the gateway accepted", "err", err)
		_ = session.Close(int(websocket.StatusInternalError), "edge accept failed")
		return
	}
	conn.SetReadLimit(realtimeReadLimit)
	relayRealtime(r.Context(), conn, session)
}

// relayRealtime pumps one session until either side ends it.
func relayRealtime(ctx context.Context, conn *websocket.Conn, session RealtimeSession) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Browser → gateway.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				code, reason := int(websocket.StatusAbnormalClosure), ""
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					code, reason = int(ce.Code), ce.Reason
				}
				_ = session.Close(code, reason)
				return
			}
			if err := session.Send(data, typ == websocket.MessageBinary); err != nil {
				return
			}
		}
	}()

	// Gateway → browser.
	for {
		ev, err := session.Recv()
		if err != nil {
			// The session ended without a close status: a broken hop. The
			// browser sees the connection drop, as it did when the proxied
			// connection dropped.
			_ = conn.CloseNow()
			return
		}
		switch {
		case ev.Closed:
			_ = conn.Close(websocket.StatusCode(ev.CloseCode), truncateCloseReason(ev.CloseReason)) //nolint:gosec // a close status fits
			return
		case ev.PingID != 0:
			wg.Add(1)
			go func(id uint64) {
				defer wg.Done()
				pingCtx, cancelPing := context.WithTimeout(ctx, realtimePingTimeout)
				defer cancelPing()
				if conn.Ping(pingCtx) == nil {
					_ = session.Pong(id)
				}
			}(ev.PingID)
		default:
			typ := websocket.MessageText
			if ev.Binary {
				typ = websocket.MessageBinary
			}
			if err := conn.Write(ctx, typ, ev.Message); err != nil {
				_ = session.Close(int(websocket.StatusAbnormalClosure), "")
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// truncateCloseReason keeps a close reason within the WebSocket limit
// without cutting a UTF-8 character.
func truncateCloseReason(reason string) string {
	if len(reason) <= maxCloseReason {
		return reason
	}
	cut := maxCloseReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}
