// Package proxy is the mTLS hop from elitea-main to a provider service.
//
// Extracted in P1.5 with two callers: the DeepWiki facade and the Inventory
// facade. It is the ReverseProxy ASSEMBLY that ADR-0012 named — the transport
// and the signer already live in llmproxy and are already shared three ways.
//
// WHAT A CALLER STILL OWNS: what to do with the body, and what its own
// routes are. Two things DeepWiki once forked this whole file for now live
// here (ADR-0023 H0): the provider's status travels back to the caller
// through an Outcome on the request context, and the legacy X-Secret header
// is stripped on every hop — a header no provider on this platform may
// honour is not one provider's history.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
)

// Proxy forwards a facade request to a provider over mTLS.
type Proxy struct {
	reverse        *httputil.ReverseProxy
	identitySecret []byte
	logger         *slog.Logger
}

// Outcome carries the provider's status back to the caller of Forward. The
// reverse proxy reports it through ModifyResponse, after Forward has already
// handed the request over, so it travels on the request context: a caller
// that needs it attaches one with WithOutcome and reads it after Forward
// returns. Status stays 0 when the provider was never reached.
//
// This is what the DeepWiki facade forked the whole proxy for (ADR-0023
// context): its invoke path revokes the callback grant it minted when the
// provider refuses the invocation, and needed the upstream status to know.
type Outcome struct {
	Status int

	// CaptureLimit, when positive, asks the hop to BUFFER up to that many
	// bytes of the provider's response into Body before it travels on to the
	// caller. Zero captures nothing, which is every caller that only wants
	// the status.
	//
	// It is done here, in ModifyResponse, rather than by wrapping the
	// ResponseWriter, and the reason is written out in material.Invocation:
	// a wrapper has to forward Flush or a streaming response stops streaming,
	// and it puts a Write on a caller-influenced body, which CodeQL reads as
	// a reflected-XSS sink. Reading the upstream body is neither.
	CaptureLimit int
	// Body is what was captured. It is the provider's bytes verbatim, and the
	// caller still receives them: the buffer is put back in front of the
	// remaining stream rather than consumed.
	Body []byte
	// Truncated reports that the response was longer than CaptureLimit, so a
	// reader knows Body is a prefix and not a document. A caller that parses
	// Body as JSON must refuse a truncated one rather than guess.
	Truncated bool
}

type outcomeKey struct{}

// WithOutcome attaches an Outcome for Forward to fill in.
func WithOutcome(ctx context.Context, outcome *Outcome) context.Context {
	return context.WithValue(ctx, outcomeKey{}, outcome)
}

func outcomeFrom(ctx context.Context) *Outcome {
	outcome, _ := ctx.Value(outcomeKey{}).(*Outcome)
	return outcome
}

// OutcomeFrom reads back the Outcome a caller attached, or nil.
//
// The reverse proxy fills it in ModifyResponse and needs no accessor; this
// one is for the OTHER implementations of the same hop — a facade's test
// double standing in for Forward has to report a status the way the real hop
// does, and without this it can only pretend to.
func OutcomeFrom(ctx context.Context) *Outcome { return outcomeFrom(ctx) }

// New builds the hop. envName names the setting that carries the base URL, so
// a refusal says which variable an operator must fix.
func New(cfg facade.Config, envName string, logger *slog.Logger) (*Proxy, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// RequireTLS: a provider refuses non-mTLS traffic, so a plain-HTTP base URL
	// is a facade that 502s on every call. Caught at startup.
	target, err := hop.ParseTarget(cfg.BaseURL, hop.TargetOptions{EnvName: envName, RequireTLS: true})
	if err != nil {
		return nil, err
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = target.Hostname()
	}
	transport, err := llmproxy.NewMTLSTransport(
		cfg.ClientCertFile, cfg.ClientKeyFile, cfg.CAFile, serverName)
	if err != nil {
		return nil, fmt.Errorf("build provider mTLS transport: %w", err)
	}

	reverse := &httputil.ReverseProxy{
		Transport: transport,
		// Negative flushes after every write. A poll is small; a generation's
		// terminal result is not, and buffering it adds latency to the one
		// call a user is waiting on.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Rewrite hands over an outbound request whose Host is already
			// cleared, so the caller's Host never travels. Nothing is assigned
			// here for exactly that reason.
			pr.SetXForwarded()
		},
		ModifyResponse: func(response *http.Response) error {
			if outcome := outcomeFrom(response.Request.Context()); outcome != nil {
				outcome.Status = response.StatusCode
				capture(outcome, response)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("provider proxy failed", "path", r.URL.Path, "error", err)
			http.Error(w, "The provider service is unreachable.", http.StatusServiceUnavailable)
		},
	}
	return &Proxy{reverse: reverse, identitySecret: []byte(cfg.IdentitySecret), logger: logger}, nil
}

// Forward proxies to providerPath, signing the caller's identity.
//
// Whatever the client sent is dropped by SignIdentityHeaders before the signed
// values are written, and the provider strips them again on arrival — a
// spoofed header would have to survive two independent removals.
func (p *Proxy) Forward(
	w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string,
) {
	if p == nil || p.reverse == nil {
		http.Error(w, "This provider is not enabled.", http.StatusServiceUnavailable)
		return
	}
	outbound := r.Clone(r.Context())
	outbound.URL.Path = providerPath
	outbound.URL.RawPath = ""

	// The tenant is part of the canonical string both sides sign, so it must
	// travel even though this facade never reads it.
	// X-Secret is the legacy pylon shared-secret header. The signed identity
	// headers replace it on every provider hop (ADR-0022 decision 6); a copy a
	// caller sends must not travel on to a provider that once honoured it.
	// DeepWiki forked this proxy to do exactly this — folded back (ADR-0023).
	outbound.Header.Del("X-Secret")
	tenantID, _ := tenant.TenantFromContext(r.Context())
	llmproxy.SignIdentityHeaders(outbound.Header, p.identitySecret, projectID, userID, tenantID, "")

	// Latent today — no server here sets a WriteTimeout — and called so that
	// adding one later cannot truncate one hop's responses and not another's.
	hop.ClearWriteDeadline(w, p.logger)
	p.reverse.ServeHTTP(w, outbound)
}

// capture buffers up to outcome.CaptureLimit bytes of the response and puts
// them back in front of the stream.
//
// THE CALLER'S RESPONSE IS NOT CONSUMED. What is read is re-prefixed with an
// io.MultiReader, and the original body stays the Closer, so a capture that
// stops short of the end still forwards every remaining byte. A read error is
// left to travel: returning it here would turn a partial provider response
// into a 502 the caller never had before this hook existed.
func capture(outcome *Outcome, response *http.Response) {
	if outcome.CaptureLimit <= 0 || response.Body == nil {
		return
	}
	// One byte past the limit, so "exactly the limit" and "longer than the
	// limit" are distinguishable rather than both looking complete.
	buffered, err := io.ReadAll(io.LimitReader(response.Body, int64(outcome.CaptureLimit)+1))
	if len(buffered) > outcome.CaptureLimit {
		outcome.Truncated = true
		outcome.Body = buffered[:outcome.CaptureLimit]
	} else {
		outcome.Body = buffered
	}
	if err != nil {
		// The bytes read so far are still worth reporting, and the error
		// still belongs to the client: rebuild the body so the reverse proxy
		// hits the same failure at the same offset it would have hit anyway.
		response.Body = &prefixedBody{
			Reader: io.MultiReader(bytes.NewReader(buffered), errorReader{err}),
			Closer: response.Body,
		}
		return
	}
	response.Body = &prefixedBody{
		Reader: io.MultiReader(bytes.NewReader(buffered), response.Body),
		Closer: response.Body,
	}
}

// prefixedBody is the captured prefix followed by whatever is left, closing
// the ORIGINAL body — the prefix has nothing to close.
type prefixedBody struct {
	io.Reader
	io.Closer
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
