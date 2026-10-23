// Package cancelbus is the NATS implementation of spi.CancelBus: the
// sub-application host's cancel fan-out across its replicas (issue #1359).
//
// A cancel of invocation <id> of application <app> is one core-NATS message
// on elitea.subapp.<app>.cancel.<id> with an empty body; every replica of the
// application's host subscribes to elitea.subapp.<app>.cancel.* and stops the
// run if it owns it. Core NATS and not JetStream, deliberately: a cancel is
// worth delivering to the replicas that are up NOW, and replaying one to a
// replica that starts later would find nothing to stop — its runs are new.
//
// Identity. In a cluster the host presents its application's client
// certificate (natsconn.SubappIdentity: elitea-deepwiki, elitea-inventory),
// which the NATS server maps into the SUBAPP account, where each identity may
// publish and subscribe to its own application's cancel subjects only.
package cancelbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/elitea-ng/elitea-platform/libs/go/natsconn"
)

// ErrInvalidID refuses an invocation id that is not one NATS subject token.
var ErrInvalidID = errors.New("cancelbus: an invocation id must be one subject token")

// Bus fans cancels out over one NATS connection.
type Bus struct {
	conn   *nats.Conn
	app    string
	logger *slog.Logger
}

// New is a bus for app over conn. The connection is the caller's to close.
func New(conn *nats.Conn, app string, logger *slog.Logger) (*Bus, error) {
	if conn == nil {
		return nil, errors.New("cancelbus: a NATS connection is required")
	}
	if !token(app) {
		return nil, fmt.Errorf("cancelbus: application %q is not one subject token", app)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{conn: conn, app: app, logger: logger}, nil
}

// token reports whether s is a non-empty NATS subject token: no separator,
// no wildcard, no whitespace.
func token(s string) bool {
	return s != "" && !strings.ContainsAny(s, ".*> \t\r\n")
}

// Publish announces the cancel and flushes it, so a caller that returns
// success has put it on the wire.
func (b *Bus) Publish(ctx context.Context, invocationID string) error {
	if !token(invocationID) {
		return ErrInvalidID
	}
	if err := b.conn.Publish(natsconn.SubappCancelSubject(b.app, invocationID), nil); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	return b.conn.FlushWithContext(ctx)
}

// Subscribe calls stop for every cancel announced until ctx ends. The
// subscription is flushed before it returns, so a cancel published after
// Subscribe returned reaches it.
func (b *Bus) Subscribe(ctx context.Context, stop func(invocationID string)) error {
	prefix := strings.TrimSuffix(natsconn.SubappCancelWildcard(b.app), "*")
	subscription, err := b.conn.Subscribe(natsconn.SubappCancelWildcard(b.app), func(message *nats.Msg) {
		id := strings.TrimPrefix(message.Subject, prefix)
		if !token(id) {
			return
		}
		stop(id)
	})
	if err != nil {
		return err
	}
	if err := b.conn.Flush(); err != nil {
		_ = subscription.Unsubscribe()
		return err
	}
	go func() {
		<-ctx.Done()
		if err := subscription.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			b.logger.Debug("cancel subscription not removed", "error", err)
		}
	}()
	return nil
}

// Config is how a host reaches NATS.
type Config struct {
	URL      string
	Material natsconn.Material
	// App is the application (deepwiki, inventory); it names the identity
	// and the subjects.
	App string
}

// Connect dials NATS as the application's identity. An unreachable server
// fails the start: a host whose cancels cannot reach the replica running the
// invocation must not report ready as if they could. After the first dial
// the client reconnects for ever.
func Connect(cfg Config, logger *slog.Logger) (*nats.Conn, error) {
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		return nil, errors.New("cancelbus: a NATS URL is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	identity := natsconn.SubappIdentity(cfg.App)
	if identity == "" {
		return nil, fmt.Errorf("cancelbus: application %q has no NATS identity", cfg.App)
	}
	if err := cfg.Material.CheckURL(url); err != nil {
		return nil, err
	}
	if err := cfg.Material.Check(); err != nil {
		return nil, err
	}
	options := []nats.Option{
		nats.Name(identity),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(identity)),
		nats.Timeout(5 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500 * time.Millisecond),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("cancel bus disconnected; reconnecting", "error", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("cancel bus reconnected", "server", c.ConnectedUrlRedacted())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, subscription *nats.Subscription, err error) {
			subject := ""
			if subscription != nil {
				subject = subscription.Subject
			}
			logger.Warn("cancel bus asynchronous error", "subject", subject, "error", err)
		}),
	}
	if cfg.Material.Enabled() {
		options = append(options,
			nats.Secure(natsconn.BaseTLSConfig()),
			nats.ClientTLSConfig(cfg.Material.ClientCertificate, cfg.Material.RootCAs))
	}
	conn, err := nats.Connect(url, options...)
	if err != nil {
		return nil, fmt.Errorf("cancelbus: connect: %w", err)
	}
	if cfg.Material.Enabled() {
		logger.Info("cancel bus connected", "server", conn.ConnectedUrlRedacted(), "identity", identity, "tls", true)
	} else {
		logger.Warn("cancel bus connected without TLS or a client identity (compose posture only; a cluster's NATS refuses this)",
			"server", conn.ConnectedUrlRedacted())
	}
	return conn, nil
}
