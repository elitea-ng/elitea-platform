package llmproxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
)

// Config configures the edge → gateway streaming reverse proxy.
type Config struct {
	// TargetURL is the base URL of elitea-llm-gateway-svc (mTLS-only ClusterIP),
	// e.g. "https://elitea-llm-gateway-svc:8443". Required.
	TargetURL string
	// Transport is the round-tripper used to reach the gateway. When nil,
	// NewMTLSTransport builds one from the TLS* fields.
	Transport http.RoundTripper
	// IdentitySecret is the HMAC key used to sign forwarded identity headers.
	// Optional: an empty secret disables signing (the mTLS transport still
	// authenticates the hop).
	IdentitySecret string

	// mTLS material, used only when Transport is nil.
	ClientCertFile string // PEM client certificate presented to the gateway
	ClientKeyFile  string // PEM client private key
	CAFile         string // PEM CA bundle used to verify the gateway certificate
	ServerName     string // expected gateway certificate SAN (defaults to TargetURL host)

	// Logger receives proxy error events. Defaults to slog.Default().
	Logger *slog.Logger

	// ExecutionVerifier checks an inbound X-Elitea-Execution-Id against a
	// live execution of the caller before the edge signs it. Nil keeps the
	// shape check only. Production composition always sets it: the analytics
	// reads decide active users and run spend from this id.
	ExecutionVerifier ExecutionVerifier
}

// Proxy is elitea-main's whole gateway role: a streaming reverse proxy to
// elitea-llm-gateway-svc. It byte-streams /llm requests and responses without
// reframing the dialect or buffering the SSE stream (design §2, §6.3).
type Proxy struct {
	rp     *httputil.ReverseProxy
	logger *slog.Logger
}

// New builds a streaming reverse proxy from cfg. It returns an error if the
// target URL is invalid or the mTLS transport cannot be constructed.
func New(cfg Config) (*Proxy, error) {
	// NOT RequireTLS: the gateway is reachable over plain HTTP in a development
	// stack, and refusing that would break a supported configuration. The
	// DeepWiki hop does require it, because its peer refuses non-mTLS traffic.
	// The difference is a property of the two peers, not an inconsistency.
	target, err := hop.ParseTarget(cfg.TargetURL, hop.TargetOptions{
		EnvName: "LLM_GATEWAY_URL",
	})
	if err != nil {
		return nil, fmt.Errorf("llmproxy: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	transport := cfg.Transport
	if transport == nil {
		serverName := cfg.ServerName
		if serverName == "" {
			serverName = target.Hostname()
		}
		transport, err = NewMTLSTransport(cfg.ClientCertFile, cfg.ClientKeyFile, cfg.CAFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("llmproxy: build mTLS transport: %w", err)
		}
	}

	secret := []byte(cfg.IdentitySecret)
	verifier := cfg.ExecutionVerifier

	rp := &httputil.ReverseProxy{
		// FlushInterval < 0 flushes to the client immediately after every proxy
		// write, so the SSE stream the gateway already flushed is not re-buffered
		// on this second hop (design §6.3, §9.5).
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// SetURL preserves the inbound path; keep the caller's Host off the
			// outbound request so the gateway sees its own service host.
			pr.Out.Host = target.Host
			// Strip client-spoofed identity headers and inject the edge-resolved,
			// signed identity (X-Elitea-Project-Id / X-Elitea-User-Id /
			// X-Elitea-Tenant-Id + X-Elitea-Identity-Signature). The gateway
			// trusts these only on the mTLS-internal network (design §2, §6.1).
			injectIdentity(pr.In.Context(), pr.Out.Header, secret, verifier)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Ensure no downstream proxy (Traefik/nginx) buffers the stream; the
			// gateway sets this too, but re-assert it end-to-end (design §6.3).
			resp.Header.Set("X-Accel-Buffering", "no")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// An audio request the edge stopped (body ceiling, deadline) gets
			// its own named refusal, not "gateway unavailable".
			if writeAudioRefusal(w, r, err) {
				logger.Warn("llmproxy: audio request stopped at the edge", "err", err, "path", r.URL.Path)
				return
			}
			logger.Error("llmproxy: upstream error", "err", err, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"llm gateway unavailable","type":"service_unavailable","code":"upstream_unavailable"}}`))
		},
	}

	return &Proxy{rp: rp, logger: logger}, nil
}

// ServeHTTP proxies the request to the gateway. It clears the per-connection
// write deadline before streaming so the http.Server's global WriteTimeout does
// not hard-kill a long-lived SSE response on the /llm path (design §9.5). This
// is the edge-side equivalent of the gateway's WriteTimeout: 0.
//
// The /llm/v1/audio routes are not streams. They get a body ceiling and a
// deadline here (audio_limits.go) before the request leaves the edge.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r, release, ok := limitAudioRequest(w, r)
	if !ok {
		return
	}
	defer release()
	hop.ClearWriteDeadline(w, p.logger)
	p.rp.ServeHTTP(w, r)
}

// NewMTLSTransport builds an HTTP/1.1 transport that presents the given client
// certificate and verifies the gateway against caFile. HTTP/2 is disabled
// (ForceAttemptHTTP2=false, empty NextProtos) so the streaming reverse proxy
// runs over HTTP/1.1 as the design mandates (design §9.1).
func NewMTLSTransport(certFile, keyFile, caFile, serverName string) (*http.Transport, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from ca bundle %q", caFile)
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		// No ResponseHeaderTimeout / WriteTimeout: SSE responses are long-lived.
		TLSHandshakeTimeout: 10 * time.Second,
	}, nil
}
