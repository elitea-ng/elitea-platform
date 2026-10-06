package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

const (
	runtimeNATSConnectTimeout = 5 * time.Second
	// runtimeNATSRequestTimeout bounds one JetStream API call (publish ack,
	// stream info, direct get) when the caller's context has no deadline.
	runtimeNATSRequestTimeout = 5 * time.Second
	runtimeNATSDrainTimeout   = 5 * time.Second
)

// NATSConnConfig is the least-privilege NATS surface the runtime plane and the
// operator preflight need: a URL and the elitea-main-runtime identity.
type NATSConnConfig struct {
	URL      string
	Material natsconn.Material
	// Name is the connection name the server shows in connz.
	Name string
}

// NewRuntimeNATSConn dials the runtime plane's NATS connection as the
// elitea-main-runtime identity (docs/runtime-command-bus.md). The client
// certificate and CA are read again on every handshake, so a cert-manager
// renewal is presented on the next reconnect.
//
// An unreachable server fails startup: a runtime plane that cannot publish
// commands must not report ready. After the first dial the client reconnects
// forever; a publish during an outage fails and the command stays in the
// PostgreSQL outbox.
func NewRuntimeNATSConn(config NATSConnConfig, logger *slog.Logger) (*nats.Conn, error) {
	url := strings.TrimSpace(config.URL)
	if url == "" {
		return nil, errors.New("runtime NATS URL is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := config.Material.CheckURL(url); err != nil {
		return nil, fmt.Errorf("%s: %w", runtimeNATSURLEnv, err)
	}
	if err := config.Material.Check(); err != nil {
		return nil, err
	}
	name := config.Name
	if name == "" {
		name = natsconn.IdentityMainRuntime
	}
	opts := []nats.Option{
		nats.Name(name),
		// The permission table lets elitea-main-runtime subscribe to its own
		// inbox prefix only (publish acks, stream/consumer info, direct gets).
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityMainRuntime)),
		nats.Timeout(runtimeNATSConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500 * time.Millisecond),
		nats.DrainTimeout(runtimeNATSDrainTimeout),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("runtime NATS disconnected; reconnecting", "err", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("runtime NATS reconnected", "server", c.ConnectedUrlRedacted())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			logger.Warn("runtime NATS asynchronous error", "subject", subject, "err", err)
		}),
	}
	if config.Material.Enabled() {
		opts = append(opts,
			nats.Secure(natsconn.BaseTLSConfig()),
			nats.ClientTLSConfig(config.Material.ClientCertificate, config.Material.RootCAs),
		)
	}
	conn, err := nats.Connect(url, opts...)
	if err != nil {
		// The URL is never echoed: CheckURL refused credentials, but a
		// reviewer should not have to know that.
		return nil, fmt.Errorf("connect %s: %w", runtimeNATSURLEnv, err)
	}
	if config.Material.Enabled() {
		logger.Info("runtime NATS connected", "server", conn.ConnectedUrlRedacted(), "nats_auth", config.Material.Mode(), "tls", true)
	} else {
		logger.Warn("runtime NATS connected without TLS or a client identity (compose posture only; a cluster's NATS refuses this)",
			"server", conn.ConnectedUrlRedacted(), "nats_auth", config.Material.Mode(), "tls", false)
	}
	return conn, nil
}

// NewRuntimeJetStream is the JetStream context every runtime publisher and
// reader uses, with a bounded default per-call timeout.
func NewRuntimeJetStream(conn *nats.Conn) (jetstream.JetStream, error) {
	if conn == nil {
		return nil, errors.New("runtime NATS connection is required")
	}
	return jetstream.New(conn, jetstream.WithDefaultTimeout(runtimeNATSRequestTimeout))
}

// consumerFor is the durable the bootstrap creates for a contract stream.
func consumerFor(stream string) string {
	return commandbus.KnownStreams[stream]
}

// bindCommandStreams verifies every stream a configured route publishes into
// and returns their handles. A stream shared by two routes must keep the
// longer route's commands, so its MaxAge requirement is the longest deadline
// of the routes on it.
func bindCommandStreams(ctx context.Context, js jetstream.JetStream, config Config, toolkit standaloneToolkitRoute) ([]jetstream.Stream, error) {
	deadlines := map[string]time.Duration{config.CommandStream: validationDeadlineTTL}
	messageBytes := map[string]int{config.CommandStream: productionTransportMessageBytes}
	need := func(stream string, deadline time.Duration) {
		if deadline > deadlines[stream] {
			deadlines[stream] = deadline
		}
		if productionIndexTransportMessageBytes > messageBytes[stream] {
			messageBytes[stream] = productionIndexTransportMessageBytes
		}
	}
	if config.IndexIngestDispatchEnabled {
		need(config.IndexIngestCommandStream, indexDeadlineTTL)
	}
	if config.AgentExecutionDispatchEnabled {
		need(config.AgentExecutionCommandStream, agentDeadlineTTL)
	}
	if toolkit.enabled {
		need(toolkit.stream, toolkitCallToolDeadlineTTL)
	}
	streams := make([]string, 0, len(deadlines))
	for stream := range deadlines {
		streams = append(streams, stream)
	}
	sort.Strings(streams)
	handles := make([]jetstream.Stream, 0, len(streams))
	for _, stream := range streams {
		handle, err := commandbus.BindStream(ctx, js, commandbus.StreamRequirement{
			Stream:          stream,
			MinMaxAge:       deadlines[stream] + commandbus.MaxAgeMargin,
			MaxMessageBytes: messageBytes[stream],
		})
		if err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	return handles, nil
}
