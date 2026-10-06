package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
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
// The server must have JetStream enabled (canvas presence keeps its rosters
// in a KV bucket, newCanvasPresenceStore); the shipped NATS chart does.
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

// eventsNATSReplicasEnv sets the replica count of the JetStream KV bucket
// canvas presence keeps its rosters in. Unset means 1, right for the scale-1
// NATS profile; an HA (3-node) server may take 3. Presence is ephemeral, so
// R1 on an HA server only empties rosters for one heartbeat on a node loss.
const eventsNATSReplicasEnv = "ELITEA_EVENTS_NATS_REPLICAS"

// newCanvasPresenceStore opens (creating it if absent) the presence KV bucket
// on the live-update connection. Errors stop startup: a configured
// live-update plane whose JetStream is missing would otherwise serve presence
// from a per-replica roster while claiming to be shared.
func newCanvasPresenceStore(
	ctx context.Context,
	conn *nats.Conn,
	lookup func(string) (string, bool),
) (*v2canvaspresence.NATSStore, error) {
	if conn == nil {
		return nil, errors.New("a NATS connection is required")
	}
	replicas, err := eventsNATSReplicas(lookup)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	createCtx, cancel := context.WithTimeout(ctx, eventsNATSConnectTimeout)
	defer cancel()
	return v2canvaspresence.NewNATSStore(createCtx, js, v2canvaspresence.NATSStoreConfig{Replicas: replicas})
}

// eventsNATSReplicas reads ELITEA_EVENTS_NATS_REPLICAS: unset or blank is 1,
// anything but a positive integer is refused rather than defaulted, so a typo
// cannot quietly downgrade an HA bucket to one replica.
func eventsNATSReplicas(lookup func(string) (string, bool)) (int, error) {
	raw, present := lookup(eventsNATSReplicasEnv)
	raw = strings.TrimSpace(raw)
	if !present || raw == "" {
		return 1, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", eventsNATSReplicasEnv)
	}
	return parsed, nil
}
