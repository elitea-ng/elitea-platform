package proxy

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The refusal reasons. Each is the value of the RefusalHeader, a label of
// egress_proxy_refusals_total and the reason of a connection log line. The
// set is fixed, so every series is present from the first scrape.
const (
	ReasonProxyConnectionLimit = "proxy-connection-limit"
	ReasonTokenMissing         = "token-missing"
	ReasonTokenInvalid         = "token-invalid"
	ReasonTokenExpired         = "token-expired"
	ReasonUnsupported          = "protocol-not-supported"
	ReasonBadTarget            = "bad-target"
	ReasonHostNotAllowed       = "host-not-allowed"
	ReasonPortNotAllowed       = "port-not-allowed"
	ReasonIPLiteralNotAllowed  = "ip-literal-not-allowed"
	ReasonResolveFailed        = "resolve-failed"
	ReasonAddressNotPublic     = "address-not-public"
	ReasonRunConnectionLimit   = "run-connection-limit"
	ReasonRunByteLimit         = "run-byte-limit"
	ReasonDraining             = "draining"

	// The ClientHello peek of a CONNECT tunnel (clienthello.go). These come
	// after the 200: the tunnel closes, and the reason is in the log line,
	// the counter and the run summary, never in a header.
	ReasonSNIMismatch          = "sni-mismatch"
	ReasonSNIMissing           = "sni-missing"
	ReasonNotTLS               = "not-tls"
	ReasonSNITimeout           = "sni-timeout"
	ReasonClientHelloTooLarge  = "client-hello-too-large"
	ReasonClientHelloMalformed = "client-hello-malformed"
)

var allReasons = []string{
	ReasonProxyConnectionLimit, ReasonTokenMissing, ReasonTokenInvalid, ReasonTokenExpired,
	ReasonUnsupported, ReasonBadTarget, ReasonHostNotAllowed, ReasonPortNotAllowed,
	ReasonIPLiteralNotAllowed, ReasonResolveFailed, ReasonAddressNotPublic,
	ReasonRunConnectionLimit, ReasonRunByteLimit, ReasonDraining,
	ReasonSNIMismatch, ReasonSNIMissing, ReasonNotTLS, ReasonSNITimeout,
	ReasonClientHelloTooLarge, ReasonClientHelloMalformed,
}

// The outcomes of a connection that passed every check.
const (
	OutcomeOK            = "ok"
	OutcomeRefused       = "refused"
	OutcomeUpstreamError = "upstream-error"
	OutcomeIdleTimeout   = "idle-timeout"
	OutcomeLifetime      = "lifetime"
	OutcomeTokenExpired  = "token-expired"
	OutcomeByteLimit     = "byte-limit"
	OutcomeShutdown      = "shutdown"
)

var allOutcomes = []string{
	OutcomeOK, OutcomeRefused, OutcomeUpstreamError, OutcomeIdleTimeout,
	OutcomeLifetime, OutcomeTokenExpired, OutcomeByteLimit, OutcomeShutdown,
}

// Metrics holds the proxy's counters. The repository's Go services write the
// Prometheus text format by hand from a fixed list (elitea-llm-gateway's
// /metrics), with no client library; this follows that convention.
type Metrics struct {
	outcomes          map[string]*atomic.Int64
	refusals          map[string]*atomic.Int64
	bytesUp           atomic.Int64
	bytesDown         atomic.Int64
	activeConnections atomic.Int64
	activeTunnels     atomic.Int64
	keyReloadErrors   atomic.Int64

	mu      sync.Mutex
	gauges  map[string]func() int64
	helpFor map[string]string
}

// NewMetrics builds the counter set with every label value present.
func NewMetrics() *Metrics {
	m := &Metrics{
		outcomes: make(map[string]*atomic.Int64, len(allOutcomes)),
		refusals: make(map[string]*atomic.Int64, len(allReasons)),
		gauges:   map[string]func() int64{},
		helpFor:  map[string]string{},
	}
	for _, o := range allOutcomes {
		m.outcomes[o] = new(atomic.Int64)
	}
	for _, r := range allReasons {
		m.refusals[r] = new(atomic.Int64)
	}
	return m
}

func (m *Metrics) outcome(o string) {
	if c, ok := m.outcomes[o]; ok {
		c.Add(1)
	}
}

func (m *Metrics) refusal(reason string) {
	if c, ok := m.refusals[reason]; ok {
		c.Add(1)
	}
	m.outcome(OutcomeRefused)
}

// RefusalCount reads one refusal counter, for tests.
func (m *Metrics) RefusalCount(reason string) int64 {
	if c, ok := m.refusals[reason]; ok {
		return c.Load()
	}
	return 0
}

// KeyReloadFailed counts a failed key-set reload.
func (m *Metrics) KeyReloadFailed() { m.keyReloadErrors.Add(1) }

// Gauge registers a gauge read at scrape time.
func (m *Metrics) Gauge(name, help string, read func() int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = read
	m.helpFor[name] = help
}

// ServeHTTP writes the Prometheus text exposition.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	family := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	family("egress_proxy_connections_total", "counter", "Proxy requests by outcome. Refused requests count once, under refused.")
	for _, o := range allOutcomes {
		fmt.Fprintf(&b, "egress_proxy_connections_total{outcome=%q} %d\n", o, m.outcomes[o].Load())
	}
	family("egress_proxy_refusals_total", "counter", "Refused proxy requests by reason.")
	for _, r := range allReasons {
		fmt.Fprintf(&b, "egress_proxy_refusals_total{reason=%q} %d\n", r, m.refusals[r].Load())
	}
	family("egress_proxy_bytes_total", "counter", "Bytes relayed: up is sandbox to destination, down is destination to sandbox.")
	fmt.Fprintf(&b, "egress_proxy_bytes_total{direction=\"up\"} %d\n", m.bytesUp.Load())
	fmt.Fprintf(&b, "egress_proxy_bytes_total{direction=\"down\"} %d\n", m.bytesDown.Load())
	family("egress_proxy_client_connections", "gauge", "Open client connections to this replica.")
	fmt.Fprintf(&b, "egress_proxy_client_connections %d\n", m.activeConnections.Load())
	family("egress_proxy_active_upstreams", "gauge", "Open upstream connections (tunnels and forwarded requests).")
	fmt.Fprintf(&b, "egress_proxy_active_upstreams %d\n", m.activeTunnels.Load())
	family("egress_proxy_key_reload_errors_total", "counter", "Failed reloads of the run-token public key set. The last good set stays in use.")
	fmt.Fprintf(&b, "egress_proxy_key_reload_errors_total %d\n", m.keyReloadErrors.Load())

	m.mu.Lock()
	names := make([]string, 0, len(m.gauges))
	for name := range m.gauges {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		family(name, "gauge", m.helpFor[name])
		fmt.Fprintf(&b, "%s %d\n", name, m.gauges[name]())
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, b.String())
}
