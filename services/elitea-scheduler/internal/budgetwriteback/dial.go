package budgetwriteback

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
)

// DialConfig is the scheduler's NATS connection for the write-back consumer.
type DialConfig struct {
	// URL is GATEWAY_NATS_URL: the NATS the gateway publishes deltas to.
	URL string
	// TLSCAFile, TLSCertFile and TLSKeyFile are GATEWAY_NATS_TLS_*: the
	// scheduler's client identity (#1076). All three or none.
	TLSCAFile   string
	TLSCertFile string
	TLSKeyFile  string
}

// Dial connects as the elitea-scheduler identity. With client material the
// URL must be tls:// and carry no credential; a tls:// URL without material
// is refused. The certificate callbacks re-read their files on every
// handshake, so a cert-manager renewal is presented on the next reconnect.
// The permission table lets this identity subscribe to _INBOX_elitea-scheduler
// only and touch nothing but its own durable consumer.
func Dial(cfg DialConfig, logger *slog.Logger) (*nats.Conn, error) {
	if logger == nil {
		logger = slog.Default()
	}
	names := natsconn.EnvNames("GATEWAY")
	vals := map[string]string{names[0]: cfg.TLSCAFile, names[1]: cfg.TLSCertFile, names[2]: cfg.TLSKeyFile}
	material, err := natsconn.FromEnv("GATEWAY", func(k string) (string, bool) {
		v, ok := vals[k]
		return v, ok
	})
	if err != nil {
		return nil, err
	}
	if err := material.CheckURL(cfg.URL); err != nil {
		return nil, fmt.Errorf("GATEWAY_NATS_URL: %w", err)
	}
	if err := material.Check(); err != nil {
		return nil, err
	}
	opts := []nats.Option{
		nats.Name("elitea-scheduler-budget-writeback"),
		nats.Timeout(time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500 * time.Millisecond),
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityScheduler)),
	}
	if material.Enabled() {
		opts = append(opts,
			nats.Secure(natsconn.BaseTLSConfig()),
			nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs),
		)
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		// The URL may carry a credential in a dev posture; never echo it.
		return nil, fmt.Errorf("connect GATEWAY_NATS_URL: %w", err)
	}
	if material.Enabled() {
		logger.Info("budget write-back: NATS connected", "server", nc.ConnectedUrlRedacted(), "nats_auth", material.Mode(), "tls", true)
	} else {
		logger.Warn("budget write-back: NATS connected without TLS or a client identity (compose posture only; a cluster's NATS refuses this)",
			"server", nc.ConnectedUrlRedacted(), "nats_auth", material.Mode(), "tls", false)
	}
	return nc, nil
}
