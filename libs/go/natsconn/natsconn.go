// Package natsconn holds what every elitea NATS client needs to agree on: the
// client identities the server's permission table names, the inbox prefix each
// identity may subscribe to, and the TLS material a client presents.
//
// It is standard library only on purpose. elitea-main and elitea-scheduler sit
// in go.work, and elitea-llm-gateway is a standalone module with its own Go
// toolchain that takes this one through a `replace`. A nats.go dependency here
// would put a third nats.go version into the gateway's module graph for the
// sake of three lines each client writes itself:
//
//	opts = append(opts, nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityMain)))
//	if material.Enabled() {
//		opts = append(opts, nats.Secure(natsconn.BaseTLSConfig()),
//			nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs))
//	}
//
// # Identity
//
// A client is identified by the URI SAN of its certificate,
// spiffe://<trust domain>/nats/<identity>. The NATS server runs with
// `verify_and_map`, so the SAN is the user name and the permission table in
// deploy/helm/nats/values.yaml decides which account the user is in and what
// it may publish and subscribe to there. Nothing in a URL is a credential: URLs are tls://host:port
// and can sit in a ConfigMap.
//
// # Rotation
//
// cert-manager renews client certificates in place. nats.go calls the
// ClientCertificate and RootCAs callbacks on every TLS handshake
// (nats.go makeTLSConn), and both read the files again, so a renewed
// certificate is presented on the next reconnect without a restart.
package natsconn

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The client identities. Each is the last path segment of the certificate's
// URI SAN and the suffix of the client's inbox prefix. The NATS chart's
// permission table (deploy/helm/nats/values.yaml) declares each in exactly
// one account; a new identity needs a row there first.
const (
	// MAIN account.
	IdentityMain          = "elitea-main"
	IdentityBootstrapMain = "elitea-nats-bootstrap-main"

	// GATEWAY account.
	IdentityGateway          = "elitea-llm-gateway"
	IdentityBootstrapGateway = "elitea-nats-bootstrap-gateway"

	// SCHEDULER account: no JetStream, no streams, no bootstrap. The
	// scheduler drains GATEWAY's budget-writeback consumer through service
	// imports (SchedulerGatewayJSAPIPrefix).
	IdentityScheduler = "elitea-scheduler"

	// RUNTIME account: the runtime command bus (elitea.rt.v1.>,
	// docs/runtime-command-bus.md).
	//
	// IdentityMainRuntime is elitea-main's SECOND identity: the command bus
	// producer and the execution-replay wake-up, configured by
	// EnvPrefixRuntime (ELITEA_RUNTIME_NATS_URL and ELITEA_RUNTIME_NATS_TLS_*),
	// separate from the live-update plane's elitea-main identity so that
	// neither plane's grants reach the other's account.
	IdentityMainRuntime = "elitea-main-runtime"
	IdentityBootstrapRuntime = "elitea-nats-bootstrap-runtime"

	// WORKER account: the command bus CONSUMER, presented by the Rust and the
	// Python worker alike, and the bootstrap of the one asset WORKER owns,
	// the ELITEA_RT_V1_DEADLETTER bucket. The worker reaches RUNTIME's
	// durables only through service imports (WorkerRuntimeJSAPIPrefix).
	IdentityWorker          = "elitea-worker"
	IdentityBootstrapWorker = "elitea-nats-bootstrap-worker"
)

// The accounts, one per plane. A subject or a stream in one account is not
// reachable from another, and every account's only writer is its owner. The
// cross-account flows are GATEWAY's export of the per-project soft-alert
// subject to MAIN, GATEWAY's service exports of the budget-writeback
// consumer's INFO, MSG.NEXT and ACK subjects to SCHEDULER, and RUNTIME's
// service exports of the three worker durables' INFO, MSG.NEXT and ACK
// subjects to WORKER.
const (
	AccountMain      = "MAIN"
	AccountGateway   = "GATEWAY"
	AccountScheduler = "SCHEDULER"
	AccountRuntime   = "RUNTIME"
	AccountWorker    = "WORKER"
)

// SchedulerGatewayJSAPIPrefix is the JetStream API prefix under which the
// SCHEDULER account imports GATEWAY's budget-writeback consumer API
// ($JS.API.CONSUMER.{INFO,MSG.NEXT}.GATEWAY_BUDGET_DELTAS.budget-writeback).
// SCHEDULER has no JetStream of its own, and an account without JetStream
// answers every $JS.API request itself ("JetStream not enabled for
// account"), so the import is mapped to this prefix and the scheduler opens
// JetStream with jetstream.NewWithAPIPrefix(nc, SchedulerGatewayJSAPIPrefix).
// The NATS chart's import "to" subjects must carry the same prefix
// (templates/guards.yaml pins them).
//
// Why the scheduler is not simply a GATEWAY user: the server publishes a
// JetStream API answer (a pull's deliveries, a consumer's info) to the
// requester's reply subject without checking it against the requester's
// permissions, so in GATEWAY a pull with reply gateway.budget.delta copied
// deltas back into GATEWAY_BUDGET_DELTAS. Across a service import the answer
// lands on that reply subject in SCHEDULER, where nothing stores it.
const SchedulerGatewayJSAPIPrefix = "JS.GATEWAY.API"

// WorkerRuntimeJSAPIPrefix is the JetStream API prefix under which the
// WORKER account imports RUNTIME's command-bus consumer API: exactly
// $JS.API.CONSUMER.{INFO,MSG.NEXT}.<stream>.<durable> for the three worker
// durables. The workers open the command-bus JetStream context with this
// prefix when they present a client identity (async-nats
// jetstream::with_prefix, nats-py nc.jetstream(prefix=...)); WORKER's own
// JetStream, which holds only the dead-letter bucket, keeps the default
// $JS.API prefix. The NATS chart's import "to" subjects must carry the same
// prefix (templates/guards.yaml pins them).
//
// Why the worker is not a RUNTIME user: the server publishes a JetStream API
// answer (a pull's deliveries, a consumer's or stream's info) to the
// requester's reply subject without checking it against the requester's
// permissions, so in RUNTIME a pull or info request whose reply named
// elitea.rt.v1.<route>.d.<token> made the server store the answer in that
// command stream — a signed command copied across routes, or junk filling a
// stream to its MaxMsgs so the producer is refused, with no grant that could
// remove it. Across a service import the answer lands on that reply subject
// in WORKER, whose only stream is the worker's own dead-letter bucket.
const WorkerRuntimeJSAPIPrefix = "JS.RUNTIME.API"

// EnvPrefixRuntime configures the runtime command bus producer
// (IdentityMainRuntime): ELITEA_RUNTIME_NATS_URL and the three
// ELITEA_RUNTIME_NATS_TLS_*_FILE names EnvNames returns for it.
const EnvPrefixRuntime = "ELITEA_RUNTIME"

// AccountOf returns the account the permission table declares identity in,
// or "" for an identity it does not declare.
func AccountOf(identity string) string {
	switch identity {
	case IdentityMain, IdentityBootstrapMain:
		return AccountMain
	case IdentityGateway, IdentityBootstrapGateway:
		return AccountGateway
	case IdentityScheduler:
		return AccountScheduler
	case IdentityMainRuntime, IdentityBootstrapRuntime:
		return AccountRuntime
	case IdentityWorker, IdentityBootstrapWorker:
		return AccountWorker
	}
	return ""
}

// Identities lists every identity the permission table declares.
func Identities() []string {
	return []string{
		IdentityMain, IdentityBootstrapMain,
		IdentityGateway, IdentityBootstrapGateway,
		IdentityScheduler,
		IdentityMainRuntime, IdentityBootstrapRuntime,
		IdentityWorker, IdentityBootstrapWorker,
	}
}

// DefaultTrustDomain is the SPIFFE trust domain the chart's URI SANs use.
const DefaultTrustDomain = "elitea.internal"

// IdentityURI returns the URI SAN a certificate for identity carries, and the
// user name the NATS server maps it to.
func IdentityURI(trustDomain, identity string) string {
	return "spiffe://" + trustDomain + "/nats/" + identity
}

// InboxPrefix is the reply-subject prefix a client must use. The permission
// table lets each identity subscribe to its own prefix only, so a client
// cannot read another client's JetStream API replies or acks.
func InboxPrefix(identity string) string {
	return "_INBOX_" + identity
}

// Material is a client's TLS material, as file paths. The zero value means no
// TLS: the plaintext, no-auth posture compose runs by default.
type Material struct {
	CAFile   string
	CertFile string
	KeyFile  string
	// envPrefix is kept for error messages only.
	envPrefix string
}

// Env names for prefix, e.g. "GATEWAY" -> GATEWAY_NATS_TLS_CA_FILE.
func caEnv(prefix string) string   { return prefix + "_NATS_TLS_CA_FILE" }
func certEnv(prefix string) string { return prefix + "_NATS_TLS_CERT_FILE" }
func keyEnv(prefix string) string  { return prefix + "_NATS_TLS_KEY_FILE" }

// EnvNames lists the three variables FromEnv reads for prefix, in CA, cert,
// key order. The chart renders exactly these.
func EnvNames(prefix string) [3]string {
	return [3]string{caEnv(prefix), certEnv(prefix), keyEnv(prefix)}
}

// FromEnv reads <PREFIX>_NATS_TLS_CA_FILE, <PREFIX>_NATS_TLS_CERT_FILE and
// <PREFIX>_NATS_TLS_KEY_FILE. All three unset or blank is the plaintext
// posture. Any other combination is refused: a certificate without its key,
// or a client certificate the client cannot verify the server against, is a
// half-configured deployment, and connecting anyway would either fail at the
// handshake with a less useful error or silently skip server verification.
func FromEnv(prefix string, lookup func(string) (string, bool)) (Material, error) {
	if lookup == nil {
		return Material{}, errors.New("natsconn: environment lookup is required")
	}
	read := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	m := Material{
		CAFile:    read(caEnv(prefix)),
		CertFile:  read(certEnv(prefix)),
		KeyFile:   read(keyEnv(prefix)),
		envPrefix: prefix,
	}
	if m.CAFile == "" && m.CertFile == "" && m.KeyFile == "" {
		return Material{envPrefix: prefix}, nil
	}
	var missing []string
	if m.CAFile == "" {
		missing = append(missing, caEnv(prefix))
	}
	if m.CertFile == "" {
		missing = append(missing, certEnv(prefix))
	}
	if m.KeyFile == "" {
		missing = append(missing, keyEnv(prefix))
	}
	if len(missing) > 0 {
		return Material{}, fmt.Errorf("natsconn: NATS TLS is half-configured: %s set, %s unset; set all three or none",
			strings.Join(present(m, prefix), ", "), strings.Join(missing, ", "))
	}
	return m, nil
}

func present(m Material, prefix string) []string {
	var out []string
	if m.CAFile != "" {
		out = append(out, caEnv(prefix))
	}
	if m.CertFile != "" {
		out = append(out, certEnv(prefix))
	}
	if m.KeyFile != "" {
		out = append(out, keyEnv(prefix))
	}
	return out
}

// Enabled reports whether the client presents a certificate.
func (m Material) Enabled() bool { return m.CertFile != "" }

// Mode is the posture a client logs at boot: "mtls" or "none".
func (m Material) Mode() string {
	if m.Enabled() {
		return "mtls"
	}
	return "none"
}

// ClientCertificate reads the certificate and key from disk. It has the
// signature of nats.TLSCertHandler and is called on every handshake.
func (m Material) ClientCertificate() (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("natsconn: load NATS client certificate: %w", err)
	}
	return cert, nil
}

// RootCAs reads the CA bundle from disk. It has the signature of
// nats.RootCAsHandler and is called on every handshake.
func (m Material) RootCAs() (*x509.CertPool, error) {
	pem, err := os.ReadFile(m.CAFile)
	if err != nil {
		return nil, fmt.Errorf("natsconn: read NATS CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("natsconn: NATS CA bundle %s holds no PEM certificate", m.CAFile)
	}
	return pool, nil
}

// Check reads every file once, so a missing or unreadable file fails at boot
// with the file named rather than at the first handshake.
func (m Material) Check() error {
	if !m.Enabled() {
		return nil
	}
	if _, err := m.ClientCertificate(); err != nil {
		return err
	}
	_, err := m.RootCAs()
	return err
}

// BaseTLSConfig is the tls.Config a client passes to nats.Secure before
// nats.ClientTLSConfig: TLS 1.3 only. nats.go builds a TLS 1.2 floor itself
// when no config is given, and the NATS server negotiates 1.3 with every
// client this repository ships.
func BaseTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

// CheckURL validates a NATS server URL (or a comma-separated list) against
// the material.
//
// With client material every URL must be tls://. nats.go would upgrade a
// nats:// URL to TLS once Secure is set, so this is not about the wire: it is
// about the configuration saying what it does. A nats:// URL in a values file
// next to a certificate reads as plaintext to every reviewer, and the render
// gates refuse nats:// for the same reason. A URL carrying user:password@ or
// token@ is refused too: the certificate is the identity, and a credential in
// a URL ends up in a ConfigMap.
//
// Without client material a tls:// URL is refused: the server requires a
// client certificate, so the handshake would fail with an error that reads
// like a trust problem rather than a missing setting.
func (m Material) CheckURL(raw string) error {
	urls := strings.Split(raw, ",")
	for _, one := range urls {
		one = strings.TrimSpace(one)
		if one == "" {
			continue
		}
		scheme := "nats"
		hasUser := false
		if strings.Contains(one, "://") {
			u, err := url.Parse(one)
			if err != nil {
				// Never echo the URL: it may carry a credential.
				return fmt.Errorf("natsconn: a NATS URL does not parse")
			}
			scheme = strings.ToLower(u.Scheme)
			hasUser = u.User != nil
		} else {
			hasUser = strings.Contains(one, "@")
		}
		if m.Enabled() {
			if scheme != "tls" {
				return fmt.Errorf("natsconn: NATS client material is set (%s), so every NATS URL must use tls://, and one uses %s://",
					certEnv(m.envPrefix), scheme)
			}
			if hasUser {
				return errors.New("natsconn: a NATS URL carries user information; with mTLS the certificate is the identity, so the URL must carry no credential")
			}
			continue
		}
		if scheme == "tls" {
			return fmt.Errorf("natsconn: a NATS URL uses tls:// and no client material is set; set %s, %s and %s",
				caEnv(m.envPrefix), certEnv(m.envPrefix), keyEnv(m.envPrefix))
		}
	}
	return nil
}
