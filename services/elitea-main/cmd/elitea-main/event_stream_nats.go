package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"

	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/natsbus"
)

// eventsNATSURLEnv names the NATS server elitea-main's live-update plane uses:
// the project SSE stream (/api/v2/events/prompt_lib/{projectID}) and the
// cross-replica canvas presence roster and its publish. Domain events are not
// on it (newDomainEventsPublisher).
//
// It is a NEW name rather than a reuse of GATEWAY_NATS_URL on purpose. That
// variable already means "the NATS the LLM gateway's budget counters live in"
// to the gateway and the scheduler, and gating elitea-main's live updates on
// it would turn "point budget enforcement somewhere else" into a silent change
// to the SSE stream. In the shipped chart both resolve to the same server
// (the top-level `nats` block), so one broker serves both.
//
// The URL takes any form nats.go accepts: nats://host:4222 or a
// comma-separated list for a cluster. With the client identity below set it
// must be tls:// and carry no credential (#1076); the chart renders exactly
// that.
const eventsNATSURLEnv = "ELITEA_EVENTS_NATS_URL"

// elitea-main's NATS client identity (#1076): a certificate from the
// dedicated NATS CA whose URI SAN spiffe://elitea.internal/nats/elitea-main
// the server maps to main's user in the chart's permission table. All three
// or none (natsconn.FromEnv); the files are re-read on every handshake, so a
// cert-manager renewal is picked up on reconnect. None of them is the
// compose posture: plaintext, no identity, logged at WARN.
const (
	eventsNATSPrefix         = "ELITEA_EVENTS"
	eventsNATSTLSCAFileEnv   = "ELITEA_EVENTS_NATS_TLS_CA_FILE"
	eventsNATSTLSCertFileEnv = "ELITEA_EVENTS_NATS_TLS_CERT_FILE"
	eventsNATSTLSKeyFileEnv  = "ELITEA_EVENTS_NATS_TLS_KEY_FILE"
)

// eventsNATSConnectTimeout bounds the boot dial. Generous next to the
// natsbus.ConnectTimeout used per publish, because it is paid once and a slow
// first dial is not the failure this guards against.
const eventsNATSConnectTimeout = 5 * time.Second

// newEventsNATSConn dials the live-update NATS server.
//
// Returns (nil, nil) when ELITEA_EVENTS_NATS_URL is absent or blank: the
// deployment has no live-update plane, the project SSE route stays unmounted
// (RouterConfig.EventSource is nil-gated in router.go) and canvas presence
// stays per-replica. That is a
// declared state, not an accident, and main.go logs it.
//
// The server must have JetStream enabled and the presence KV bucket the
// nats-bootstrap Job creates (newCanvasPresenceStore binds to it).
//
// A CONFIGURED server that cannot be reached FAILS STARTUP. It is the same
// posture the REDIS_URL ping it replaced had, and for the same reason: the
// alternative is a process that boots "healthy" with the route quietly
// unregistered — a 404 indistinguishable from a typo'd path, which is the
// whole failure mode of #152 and what TestNilGatedRouterFieldsAreWiredOrDeclared
// exists to make visible. Once the first dial succeeds the client reconnects
// forever (MaxReconnects(-1)): an outage after boot degrades live updates
// only, and never takes the API process down.
func newEventsNATSConn(lookup func(string) (string, bool), logger *slog.Logger) (*nats.Conn, error) {
	if lookup == nil {
		return nil, errors.New("environment lookup is required")
	}
	raw, present := lookup(eventsNATSURLEnv)
	url := strings.TrimSpace(raw)
	if !present || url == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	material, err := natsconn.FromEnv(eventsNATSPrefix, lookup)
	if err != nil {
		return nil, err
	}
	if err := material.CheckURL(url); err != nil {
		return nil, fmt.Errorf("%s: %w", eventsNATSURLEnv, err)
	}
	if err := material.Check(); err != nil {
		return nil, err
	}

	opts := []nats.Option{
		// The permission table lets main subscribe to its own inbox prefix
		// only (JetStream API replies, the presence watcher's deliveries).
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityMain)),
	}
	if material.Enabled() {
		opts = append(opts,
			nats.Secure(natsconn.BaseTLSConfig()),
			nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs),
		)
	}
	conn, err := nats.Connect(url, append(opts,
		nats.Name("elitea-main"),
		nats.Timeout(eventsNATSConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("live-update NATS disconnected; reconnecting", "err", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("live-update NATS reconnected", "server", c.ConnectedUrlRedacted())
		}),
		nats.ErrorHandler(newAsyncErrorLogger(logger, asyncErrorLogInterval, time.Now).handle),
		// Bounds the drain natsbus.EventBus.Close starts at shutdown, on the
		// client side too.
		nats.DrainTimeout(natsbus.CloseTimeout),
	)...)
	if err != nil {
		// The URL may carry credentials, so it is never echoed.
		return nil, fmt.Errorf("connect %s: %w", eventsNATSURLEnv, err)
	}
	if material.Enabled() {
		logger.Info("live-update NATS connected", "server", conn.ConnectedUrlRedacted(), "nats_auth", material.Mode(), "tls", true)
	} else {
		logger.Warn("live-update NATS connected without TLS or a client identity (compose posture only; a cluster's NATS refuses this)",
			"server", conn.ConnectedUrlRedacted(), "nats_auth", material.Mode(), "tls", false)
	}
	return conn, nil
}

// asyncErrorLogInterval is the shortest gap between two log lines for the same
// (error, subject) pair. A slow SSE client trips ErrSlowConsumer on every
// message nats.go has to drop, which in a burst is thousands a second.
const asyncErrorLogInterval = 30 * time.Second

// asyncErrorLogMaxKeys caps the dedup table. Subjects are per project, so the
// table is bounded only by the number of projects; past the cap it is reset,
// which at worst logs a few extra lines.
const asyncErrorLogMaxKeys = 1024

// asyncErrorLogger is the connection's nats.ErrorHandler. Without one, nats.go
// drops messages for a slow consumer (a subscription whose pending buffer is
// full — e.g. an SSE client that stopped reading) silently.
//
// It logs the error and the SUBJECT only, never a payload, and at most once
// per asyncErrorLogInterval for each (error, subject) pair, reporting how many
// occurrences were suppressed in between.
type asyncErrorLogger struct {
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu    sync.Mutex
	state map[string]*asyncErrorState
}

type asyncErrorState struct {
	lastLogged time.Time
	suppressed int
}

func newAsyncErrorLogger(logger *slog.Logger, interval time.Duration, now func() time.Time) *asyncErrorLogger {
	return &asyncErrorLogger{logger: logger, interval: interval, now: now, state: map[string]*asyncErrorState{}}
}

func (l *asyncErrorLogger) handle(_ *nats.Conn, sub *nats.Subscription, err error) {
	if err == nil {
		return
	}
	subject := ""
	if sub != nil {
		subject = sub.Subject
	}
	key := err.Error() + "\x00" + subject

	l.mu.Lock()
	now := l.now()
	st, ok := l.state[key]
	if ok && now.Sub(st.lastLogged) < l.interval {
		st.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := 0
	if ok {
		suppressed = st.suppressed
	}
	if !ok && len(l.state) >= asyncErrorLogMaxKeys {
		l.state = map[string]*asyncErrorState{}
	}
	l.state[key] = &asyncErrorState{lastLogged: now}
	l.mu.Unlock()

	attrs := []any{"subject", subject, "err", err, "suppressed_since_last_log", suppressed}
	if errors.Is(err, nats.ErrSlowConsumer) {
		if sub != nil {
			if dropped, derr := sub.Dropped(); derr == nil {
				attrs = append(attrs, "dropped_total", dropped)
			}
		}
		l.logger.Warn("live-update NATS slow consumer: messages dropped for a subscriber that is not keeping up", attrs...)
		return
	}
	l.logger.Warn("live-update NATS async error", attrs...)
}

// newCanvasPresenceStore binds to the presence KV bucket on the live-update
// connection and starts the replica's mirror of it (v2canvaspresence.NATSStore;
// the caller must Close it). It does not create the bucket: the nats-bootstrap
// Job owns it (#1076), and a missing one stops startup with that Job named,
// rather than serving presence from a per-replica roster while claiming to be
// shared.
func newCanvasPresenceStore(ctx context.Context, conn *nats.Conn) (*v2canvaspresence.NATSStore, error) {
	if conn == nil {
		return nil, errors.New("a NATS connection is required")
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	bindCtx, cancel := context.WithTimeout(ctx, eventsNATSConnectTimeout)
	defer cancel()
	store, err := v2canvaspresence.NewNATSStore(bindCtx, js, v2canvaspresence.NATSStoreConfig{})
	if err != nil {
		return nil, err
	}
	// Resync the presence mirror after every reconnect, chained after the
	// logging handler newEventsNATSConn installed rather than replacing it.
	// The ordered consumer behind the mirror recovers from a reconnect on its
	// own; the resync is what also covers a server that came back without the
	// messages it had (an R1 bucket on a replaced node).
	previous := conn.Opts.ReconnectedCB
	conn.SetReconnectHandler(func(c *nats.Conn) {
		if previous != nil {
			previous(c)
		}
		store.Resync()
	})
	return store, nil
}
