// Package proxy is the hop from elitea-main to a provider service: the
// sub-application host's elitea.subapp.v1.ProviderService, over gRPC with
// mutual TLS (#1359; it was an httputil.ReverseProxy to the host's HTTPS SPI
// listener until then).
//
// THE BROWSER CONTRACT DID NOT MOVE. Every facade route still ends in
// Forward(w, r, providerPath, ...), with the frozen SPI path it always
// built, and Forward answers the facade's caller with the bytes the HTTP SPI
// answered: the acceptance body, the poll body, 204 for a cancel, and the
// same transport-error envelope with the same status for each refusal. What
// changed is underneath: the path selects an RPC, the caller's identity
// travels as signed gRPC metadata (the same HMAC, the same canonical
// string), and the provider's answer comes back as the frozen JSON body the
// RPC carries.
//
// WHAT A CALLER STILL OWNS: what to do with the body, and what its own
// routes are. The provider's status travels back through an Outcome on the
// request context, and the legacy X-Secret header has nowhere to travel any
// more: only the signed identity crosses the hop.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mtlsclient"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
)

// MaxInvokeBytes bounds an invoke body, the provider's own limit: large
// enough for an expanded toolkit configuration.
const MaxInvokeBytes = 4 << 20

// maxResponseBytes bounds one provider answer. A terminal poll carries a
// generation's whole result, so this is generous.
const maxResponseBytes = 64 << 20

// Proxy forwards a facade request to a provider over gRPC with mutual TLS.
type Proxy struct {
	conn           *grpc.ClientConn
	client         subappv1.ProviderServiceClient
	identitySecret []byte
	timeout        time.Duration
	logger         *slog.Logger
}

// Outcome carries the provider's status back to the caller of Forward: the
// HTTP status Forward answered with (the provider's, translated), and on
// request the body. Status stays 0 when the provider was never reached.
//
// This is what the DeepWiki facade forked the whole proxy for (ADR-0023
// context): its invoke path revokes the callback grant it minted when the
// provider refuses the invocation, and needed the upstream status to know.
type Outcome struct {
	Status int

	// CaptureLimit, when positive, asks the hop to keep up to that many bytes
	// of the provider's answer in Body. Zero captures nothing, which is every
	// caller that only wants the status. The caller still receives the whole
	// answer either way.
	CaptureLimit int
	// Body is what was captured: the provider's bytes verbatim.
	Body []byte
	// Truncated reports that the answer was longer than CaptureLimit, so a
	// reader knows Body is a prefix and not a document. A caller that parses
	// Body as JSON must refuse a truncated one rather than guess.
	Truncated bool
}

type outcomeKey struct{}

// WithOutcome attaches an Outcome for Forward to fill in.
func WithOutcome(ctx context.Context, outcome *Outcome) context.Context {
	return context.WithValue(ctx, outcomeKey{}, outcome)
}

// OutcomeFrom reads back the Outcome a caller attached, or nil. A facade's
// test double standing in for Forward reports a status through it the way
// the real hop does.
func OutcomeFrom(ctx context.Context) *Outcome {
	outcome, _ := ctx.Value(outcomeKey{}).(*Outcome)
	return outcome
}

// RouteOption tunes how a facade route gets its hop.
type RouteOption func(*routeOptions)

type routeOptions struct{ hop *Proxy }

// WithHop has the route forward over an already built hop: the one the
// composition root also hands the provider registrar, so a facade holds one
// connection (and one certificate reloader) to its provider, not two. A nil
// hop is no option: the route builds its own.
func WithHop(hop *Proxy) RouteOption {
	return func(o *routeOptions) { o.hop = hop }
}

// ForRoute is the hop a facade route forwards over: the one WithHop handed
// it, or a new one built from cfg as New does.
func ForRoute(cfg facade.Config, envName string, logger *slog.Logger, options ...RouteOption) (*Proxy, error) {
	var chosen routeOptions
	for _, option := range options {
		option(&chosen)
	}
	if chosen.hop != nil {
		return chosen.hop, nil
	}
	return New(cfg, envName, logger)
}

// New builds the hop. envName names the setting that carries the target, so
// a refusal says which variable an operator must fix. The material is loaded
// now (a missing file is a construction error) and re-read when its files
// change; the connection is dialled on the first call.
func New(cfg facade.Config, envName string, logger *slog.Logger) (*Proxy, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := hop.ParseGRPCTarget(cfg.Target, envName); err != nil {
		return nil, err
	}
	hopLogger := logger.With("target", cfg.Target)
	creds, err := mtlsclient.NewCredentials(mtlsclient.Material{
		CertFile: cfg.ClientCertFile, KeyFile: cfg.ClientKeyFile, CAFile: cfg.CAFile,
		ServerName: cfg.ServerName, Target: cfg.Target,
	}, func(err error) {
		hopLogger.Error("provider gRPC client material could not be reloaded", "err", err)
	})
	if err != nil {
		return nil, fmt.Errorf("build provider mTLS credentials: %w", err)
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxResponseBytes),
			grpc.MaxCallSendMsgSize(MaxInvokeBytes+64<<10),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build provider gRPC client: %w", err)
	}
	return &Proxy{
		conn: conn, client: subappv1.NewProviderServiceClient(conn),
		identitySecret: []byte(cfg.IdentitySecret), timeout: cfg.Timeout, logger: logger,
	}, nil
}

// Close releases the connection.
func (p *Proxy) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// Client is the provider service this hop calls, for the registrar that
// shares its connection.
func (p *Proxy) Client() subappv1.ProviderServiceClient { return p.client }

// SignedContext is ctx carrying the caller's identity as signed metadata:
// the X-Elitea-* identity headers llmproxy.SignIdentityHeaders writes,
// lower-cased into gRPC metadata keys. Whatever identity ctx already carried
// outgoing is replaced, not merged.
func SignedContext(ctx context.Context, secret []byte, projectID, userID, tenantID string) context.Context {
	signed := http.Header{}
	llmproxy.SignIdentityHeaders(signed, secret, projectID, userID, tenantID, "")
	md := metadata.MD{}
	for name, values := range signed {
		md.Set(strings.ToLower(name), values...)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// Forward serves providerPath — a frozen SPI path, as providerhost/spi
// builds it — through the provider service, signing the caller's identity.
func (p *Proxy) Forward(
	w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string,
) {
	if p == nil || p.client == nil {
		http.Error(w, "This provider is not enabled.", http.StatusServiceUnavailable)
		return
	}
	// The tenant is part of the canonical string both sides sign, so it must
	// travel even though this facade never reads it.
	tenantID, _ := tenant.TenantFromContext(r.Context())
	ctx := SignedContext(r.Context(), p.identitySecret, projectID, userID, tenantID)
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	// Latent today — no server here sets a WriteTimeout — and called so that
	// adding one later cannot truncate one hop's responses and not another's.
	hop.ClearWriteDeadline(w, p.logger)

	call, err := parseSPIPath(r.Method, providerPath)
	if err != nil {
		p.answer(w, r, http.StatusNotFound, transportError(http.StatusNotFound, "Resource Not Found"))
		return
	}
	switch call.kind {
	case callSlots:
		response, err := p.client.Slots(ctx, &subappv1.SlotsRequest{})
		if err != nil {
			p.refused(w, r, providerPath, err)
			return
		}
		p.answer(w, r, http.StatusOK, response.GetSlotsJson())
	case callInvoke:
		raw, err := io.ReadAll(io.LimitReader(r.Body, MaxInvokeBytes+1))
		if err != nil || len(raw) > MaxInvokeBytes {
			p.answer(w, r, http.StatusBadRequest, transportError(http.StatusBadRequest, "Bad Request"))
			return
		}
		// follow=false: the acceptance is the whole answer. The browser polls
		// for progress, and a poll may land on any elitea-main replica, which
		// reads the invocation from whichever host replica it reaches.
		stream, err := p.client.Invoke(ctx, &subappv1.InvokeRequest{
			Toolkit: call.toolkit, Tool: call.tool, RequestJson: raw,
		})
		if err != nil {
			p.refused(w, r, providerPath, err)
			return
		}
		accepted, err := stream.Recv()
		if err != nil {
			p.refused(w, r, providerPath, err)
			return
		}
		p.answer(w, r, http.StatusOK, accepted.GetBodyJson())
	case callPoll:
		response, err := p.client.GetInvocation(ctx, &subappv1.GetInvocationRequest{
			Toolkit: call.toolkit, Tool: call.tool, InvocationId: call.invocation,
		})
		if err != nil {
			p.refused(w, r, providerPath, err)
			return
		}
		p.answer(w, r, http.StatusOK, response.GetBodyJson())
	case callCancel:
		if _, err := p.client.Cancel(ctx, &subappv1.CancelRequest{
			Toolkit: call.toolkit, Tool: call.tool, InvocationId: call.invocation,
		}); err != nil {
			p.refused(w, r, providerPath, err)
			return
		}
		if outcome := OutcomeFrom(r.Context()); outcome != nil {
			outcome.Status = http.StatusNoContent
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// refused answers what the provider refused with, as the HTTP SPI did: its
// transport-error envelope under the status the refusal maps to. A provider
// that could not be reached is a 503 — never a status from the provider, so
// the Outcome reports 0.
func (p *Proxy) refused(w http.ResponseWriter, r *http.Request, providerPath string, err error) {
	code, message, reached := HTTPStatus(err)
	if !reached {
		p.logger.Error("provider hop failed", "path", providerPath, "error", err)
		http.Error(w, "The provider service is unreachable.", http.StatusServiceUnavailable)
		return
	}
	p.answer(w, r, code, transportError(code, message))
}

// answer writes the provider's JSON body and reports it on the Outcome.
func (p *Proxy) answer(w http.ResponseWriter, r *http.Request, code int, body []byte) {
	if outcome := OutcomeFrom(r.Context()); outcome != nil {
		outcome.Status = code
		if outcome.CaptureLimit > 0 {
			if len(body) > outcome.CaptureLimit {
				outcome.Body, outcome.Truncated = body[:outcome.CaptureLimit], true
			} else {
				outcome.Body = body
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// HTTPStatus is the status and message the HTTP SPI answered for what the
// provider service now refuses with. reached is false for a hop that never
// got an answer from the provider (unreachable, timed out, cancelled).
func HTTPStatus(err error) (code int, message string, reached bool) {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest, "Bad Request", true
	case codes.NotFound:
		return http.StatusNotFound, "Resource Not Found", true
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "Unauthorized", true
	case codes.PermissionDenied:
		return http.StatusForbidden, "Forbidden", true
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return http.StatusServiceUnavailable, "", false
	}
	return http.StatusInternalServerError, "Internal Server Error", true
}

// transportError is the SPI's error envelope, byte for byte what the host's
// HTTP surface wrote (encoding/json, keys sorted).
func transportError(code int, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"errorCode": fmt.Sprint(code),
		"message":   message,
		"details":   []string{},
	})
	return body
}

type callKind int

const (
	callSlots callKind = iota + 1
	callInvoke
	callPoll
	callCancel
)

type spiCall struct {
	kind                      callKind
	toolkit, tool, invocation string
}

var errNotSPI = errors.New("not a provider SPI path")

// parseSPIPath reads back what providerhost/spi built: /slots,
// /tools/{toolkit}/{tool}/invoke and /tools/{toolkit}/{tool}/invocations/{id},
// each segment path-escaped on its own.
func parseSPIPath(method, path string) (spiCall, error) {
	if path == "/slots" && method == http.MethodGet {
		return spiCall{kind: callSlots}, nil
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, segment := range segments {
		unescaped, err := url.PathUnescape(segment)
		if err != nil || unescaped == "" {
			return spiCall{}, errNotSPI
		}
		segments[i] = unescaped
	}
	switch {
	case len(segments) == 4 && segments[0] == "tools" && segments[3] == "invoke" && method == http.MethodPost:
		return spiCall{kind: callInvoke, toolkit: segments[1], tool: segments[2]}, nil
	case len(segments) == 5 && segments[0] == "tools" && segments[3] == "invocations":
		call := spiCall{toolkit: segments[1], tool: segments[2], invocation: segments[4]}
		switch method {
		case http.MethodGet:
			call.kind = callPoll
		case http.MethodDelete:
			call.kind = callCancel
		default:
			return spiCall{}, errNotSPI
		}
		return call, nil
	}
	return spiCall{}, errNotSPI
}
