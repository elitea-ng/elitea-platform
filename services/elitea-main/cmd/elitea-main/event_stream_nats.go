package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// eventsNATSURLEnv names the NATS server elitea-main's live-update plane uses:
// the project SSE stream (/api/v2/events/prompt_lib/{projectID}), every
// domain-event publisher, and the cross-replica canvas presence roster.
//
// It is a NEW name rather than a reuse of GATEWAY_NATS_URL on purpose. That
// variable already means "the NATS the LLM gateway's budget counters live in"
// to the gateway and the scheduler, and gating elitea-main's live updates on
// it would turn "point budget enforcement somewhere else" into a silent change
// to the SSE stream. In the shipped chart both resolve to the same server
// (the top-level `nats` block), so one broker serves both.
//
// The URL takes any form nats.go accepts: nats://host:4222, a comma-separated
// list for a cluster, tls://… for TLS, and user:password@ or token@ userinfo.
const eventsNATSURLEnv = "ELITEA_EVENTS_NATS_URL"

// eventsNATSConnectTimeout bounds the boot dial. Generous next to the
// natsbus.ConnectTimeout used per publish, because it is paid once and a slow
// first dial is not the failure this guards against.
const eventsNATSConnectTimeout = 5 * time.Second

// newEventsNATSConn dials the live-update NATS server.
//
// Returns (nil, nil) when ELITEA_EVENTS_NATS_URL is absent or blank: the
// deployment has no live-update plane, the project SSE route stays unmounted
// (RouterConfig.EventSource is nil-gated in router.go), domain events reach
// only their webhook sinks, and canvas presence stays per-replica. That is a
// declared state, not an accident, and main.go logs it.
//
// A CONFIGURED server that cannot be reached FAILS STARTUP. It is the same
// posture the REDIS_URL ping it replaces had, and for the same reason: the
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

	conn, err := nats.Connect(url,
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
	)
	if err != nil {
		// The URL may carry credentials, so it is never echoed.
		return nil, fmt.Errorf("connect %s: %w", eventsNATSURLEnv, err)
	}
	return conn, nil
}
