package budgetwriteback

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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
// is refused (CheckDialConfig is the same check without a dial). The certificate callbacks re-read their files on every
// handshake, so a cert-manager renewal is presented on the next reconnect.
// The permission table puts this identity in the SCHEDULER account, lets it
// subscribe to _INBOX_elitea-scheduler only and reach nothing but its own
// durable consumer in GATEWAY (JetStream opens it with the import prefix).
func Dial(cfg DialConfig, logger *slog.Logger) (*nats.Conn, error) {
	if logger == nil {
		logger = slog.Default()
	}
	material, err := cfg.material()
	if err != nil {
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

// CheckDialConfig refuses what Dial would refuse before dialling: half a
// client identity, unreadable certificate files, or a URL that disagrees with
// the identity. No retry fixes any of these, so the scheduler reports them as
// misconfigured instead of retrying.
func CheckDialConfig(cfg DialConfig) error {
	_, err := cfg.material()
	return err
}

func (cfg DialConfig) material() (natsconn.Material, error) {
	names := natsconn.EnvNames(envPrefix)
	vals := map[string]string{names[0]: cfg.TLSCAFile, names[1]: cfg.TLSCertFile, names[2]: cfg.TLSKeyFile}
	material, err := natsconn.FromEnv(envPrefix, func(k string) (string, bool) {
		v, ok := vals[k]
		return v, ok
	})
	if err != nil {
		return natsconn.Material{}, err
	}
	if err := material.CheckURL(cfg.URL); err != nil {
		return natsconn.Material{}, fmt.Errorf("GATEWAY_NATS_URL: %w", err)
	}
	if err := material.Check(); err != nil {
		return natsconn.Material{}, err
	}
	return material, nil
}

// envPrefix is the scheduler's NATS environment prefix: GATEWAY_NATS_URL and
// GATEWAY_NATS_TLS_*, the names of the NATS that holds the GATEWAY plane the
// scheduler drains. The scheduler's own identity is in the SCHEDULER account.
const envPrefix = "GATEWAY"

// JetStream opens the scheduler's JetStream handle on nc, a connection Dial
// made with cfg.
//
// With a client identity the server is the NATS chart's, where the scheduler
// is in the SCHEDULER account: no JetStream of its own, and GATEWAY's
// budget-writeback consumer API imported under
// natsconn.SchedulerGatewayJSAPIPrefix, so the handle uses that prefix (#1076).
// Without one (compose's plaintext posture, one global account) the stream is
// in the scheduler's own account and the default $JS.API prefix reaches it.
// Ack subjects come from each delivery's reply subject either way.
func (cfg DialConfig) JetStream(nc *nats.Conn) (jetstream.JetStream, error) {
	material, err := cfg.material()
	if err != nil {
		return nil, err
	}
	if material.Enabled() {
		return jetstream.NewWithAPIPrefix(nc, natsconn.SchedulerGatewayJSAPIPrefix)
	}
	return jetstream.New(nc)
}

// Connector dials lazily and keeps the connection: the Supervisor calls
// JetStream on every attach attempt, so a server that was down at boot is
// dialled again on the next attempt instead of disabling the consumer for
// the life of the process.
type Connector struct {
	Config DialConfig
	Logger *slog.Logger

	mu sync.Mutex
	nc *nats.Conn
	js jetstream.JetStream
}

// JetStream returns the JetStream handle, dialling first if there is no
// connection yet. Once dialled, nats.go reconnects on its own.
func (c *Connector) JetStream() (jetstream.JetStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.js != nil {
		return c.js, nil
	}
	nc, err := Dial(c.Config, c.Logger)
	if err != nil {
		return nil, err
	}
	js, err := c.Config.JetStream(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	c.nc, c.js = nc, js
	return js, nil
}

// Close closes the connection, if one was made.
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nc != nil {
		c.nc.Close()
	}
}
