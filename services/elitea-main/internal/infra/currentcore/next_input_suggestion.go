// Package currentcore holds the outbound client for the suggestion policy of
// the current platform.
//
// DELIBERATE GAP — issue #334. This package records the gap. It does not repair
// it.
//
// No readable repository implements
// `GET /api/v2/elitea_core/next_input_suggestion_config/prompt_lib/{projectID}`.
// A search covers this monorepo, the pylon runtime, the plugin repositories, the
// pylon runtime clones, the ADR-0008 worktrees, the frontends, the documents and
// the Python SDK. It finds the path in four places. The four places are the
// request below, its own unit test, the hybrid Traefik rule and the CI check for
// that rule. The consumer is public. The server is not.
//
// This call is not dead code. Every chat send, regeneration, continuation and
// ad-hoc turn reaches it: the agent turn routes that call it are registered in
// internal/api/production_router.go. The POLICY path below is a different
// thing, and no router in this repository registers that one.
//
// Both shipped deployments make the call fail, and for the SAME reason: no
// server answers the path. The hybrid edge sends it to legacy Centry, which
// registers no such rule. The standalone stack calls this process, which
// registers no such route, so the answer is 404. The caller reads a failure as
// "no policy" and continues
// (internal/application/agentexecution/start.go:281-295). Each turn therefore
// carries `next_input_suggestion: null`.
//
// The standalone stack used to fail one layer EARLIER, on transport rather than
// on routing: it aimed `https://elitea-main:8080` at this process's own
// cleartext listener and every turn logged "server gave HTTP response to HTTPS
// client". That hid the real gap behind a configuration fault. The self-call
// origin is now derived from the listener
// (internal/runtimecomposition/config.go, `currentMainBaseURL`), so what the
// operator sees is the gap this package documents.
//
// The issue names two repairs. Both need the product owner. To own the policy,
// you must invent its shape and its storage, because the original is unreadable.
// To drop the feature, you must delete recent work (pull request #231) for a
// server that the issue puts on an unpublished branch.
//
// This package makes the failure visible instead. Each of the eight failure
// paths below returned one bare sentinel. No path gave a cause, and no path
// wrote a log line. Each path now names its cause and wraps the sentinel. The
// client reports the fault once for each process. The request, the response
// contract and the turn do not change.
package currentcore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	maxCurrentCoreBaseURLBytes        = 2048
	maxCurrentCoreTokenBytes          = 16 * 1024
	maxNextInputSuggestionPolicyBytes = 4 * 1024
	maxRuntimeCABytes                 = 1 << 20
	defaultRequestTimeout             = 3 * time.Second
)

var ErrNextInputSuggestionPolicyUnavailable = errors.New(
	"current next-input-suggestion policy is unavailable",
)

type ActorTokenIssuer interface {
	IssueToken(context.Context, int64) (string, error)
}

type NextInputSuggestionResolver struct {
	baseURL *url.URL
	issuer  ActorTokenIssuer
	client  *http.Client
	timeout time.Duration
	// reported keeps the first failure at warning level and every later one at
	// debug level. The fault this guards against is a DEPLOYMENT condition — a
	// route that is absent, an origin that speaks cleartext — so it is identical
	// on every request, and one line for each chat turn would bury the operator
	// in copies of one fact. One loud line for each process is the granularity
	// that matches the fault.
	reported sync.Once
}

// unavailable reports the cause and returns the caller's sentinel. Every failure
// path used to return `ErrNextInputSuggestionPolicyUnavailable` on its own, so
// eight different faults — no token, no route, a cleartext origin, a body over
// the cap, a field the contract does not name — were one indistinguishable
// value. `%w` on the sentinel keeps `errors.Is` true for every existing caller.
func (resolver *NextInputSuggestionResolver) unavailable(
	ctx context.Context,
	reason string,
	cause error,
) error {
	attributes := []any{"reason", reason, "endpoint", resolver.baseURL.String()}
	if cause != nil {
		attributes = append(attributes, "error", cause)
	}
	resolver.reported.Do(func() {
		slog.WarnContext(ctx, "next-input-suggestion policy is unavailable; the turn carries no policy (#334)", attributes...)
	})
	slog.DebugContext(ctx, "next-input-suggestion policy lookup failed", attributes...)
	if cause != nil {
		return fmt.Errorf("%w: %s: %w", ErrNextInputSuggestionPolicyUnavailable, reason, cause)
	}
	return fmt.Errorf("%w: %s", ErrNextInputSuggestionPolicyUnavailable, reason)
}

func NewNextInputSuggestionResolver(
	baseURL string,
	issuer ActorTokenIssuer,
	client *http.Client,
) (*NextInputSuggestionResolver, error) {
	parsed, err := parseCurrentCoreBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if issuer == nil || client == nil || client.Transport == nil {
		return nil, errors.New("current Core policy resolver dependencies are required")
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &NextInputSuggestionResolver{
		baseURL: parsed,
		issuer:  issuer,
		client:  &clientCopy,
		timeout: defaultRequestTimeout,
	}, nil
}

func NewTLSClient(caFile string) (*http.Client, error) {
	contents, err := securefile.Read(caFile, maxRuntimeCABytes, securefile.PublicMaterial)
	if err != nil {
		return nil, fmt.Errorf("load current Core runtime CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(contents) {
		return nil, errors.New("current Core runtime CA file contains no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = defaultRequestTimeout
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
	}
	return &http.Client{Transport: transport}, nil
}

func (resolver *NextInputSuggestionResolver) ResolveNextInputSuggestionPolicy(
	ctx context.Context,
	projectID int64,
	actorUserID int64,
) (json.RawMessage, error) {
	if ctx == nil || projectID <= 0 || actorUserID <= 0 {
		return nil, resolver.unavailable(context.Background(), "invalid actor or project", nil)
	}
	requestContext, cancel := context.WithTimeout(ctx, resolver.timeout)
	defer cancel()

	token, err := resolver.issuer.IssueToken(requestContext, actorUserID)
	if err != nil {
		return nil, resolver.unavailable(ctx, "actor token was not issued", err)
	}
	if !validBearerToken(token) {
		return nil, resolver.unavailable(ctx, "actor token is not a usable bearer token", nil)
	}
	endpoint := *resolver.baseURL
	endpoint.Path = "/api/v2/elitea_core/next_input_suggestion_config/prompt_lib/" +
		strconv.FormatInt(projectID, 10)
	request, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		endpoint.String(),
		nil,
	)
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy request was not built", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := resolver.client.Do(request)
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy request did not reach the current platform", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxNextInputSuggestionPolicyBytes))
		// 404 here is the shape of #334: the path is routed, and no server
		// answers it.
		return nil, resolver.unavailable(ctx,
			"policy endpoint answered "+strconv.Itoa(response.StatusCode), nil)
	}
	body, err := io.ReadAll(io.LimitReader(
		response.Body,
		maxNextInputSuggestionPolicyBytes+1,
	))
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy body was not read", err)
	}
	if len(body) > maxNextInputSuggestionPolicyBytes {
		return nil, resolver.unavailable(ctx, "policy body is larger than the limit", nil)
	}
	if !validJSONObject(body) {
		return nil, resolver.unavailable(ctx, "policy body is not a JSON object", nil)
	}
	var policy struct {
		Enabled          bool `json:"enabled"`
		MinResponseChars int  `json:"min_response_chars"`
		TimeoutSeconds   int  `json:"timeout_seconds"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, resolver.unavailable(ctx, "policy body does not match the contract", err)
	}
	if policy.MinResponseChars < 1 || policy.MinResponseChars > 100_000 ||
		policy.TimeoutSeconds < 1 || policy.TimeoutSeconds > 300 {
		return nil, resolver.unavailable(ctx, "policy values are outside the permitted range", nil)
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy was not re-encoded", err)
	}
	return canonical, nil
}

// parseCurrentCoreBaseURL accepts the two origins that can answer this call.
//
// An https origin is an address that terminates TLS: the hybrid stack's edge,
// an ingress. An http origin is accepted only on a LOOPBACK host, because that
// is this process's own cleartext listener — `cmd/elitea-main` starts one
// plain `http.Server` and never a TLS one, so a SELF call has no other shape.
// The request then stays on the loopback interface and the actor bearer token
// it carries never reaches a network.
//
// Cleartext to any other host is refused here as it is in
// `internal/runtimecomposition/config.go`, which validates the same string one
// layer up: it would put that token on the wire.
func parseCurrentCoreBaseURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxCurrentCoreBaseURLBytes || strings.TrimSpace(raw) != raw {
		return nil, errors.New("current Core base URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("current Core base URL must be an HTTPS origin, " +
			"or an HTTP origin on a loopback address")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("current Core base URL may only use HTTP on a loopback address")
	}
	parsed.Path = ""
	return parsed, nil
}

// loopbackHost reports whether a host name names this machine only.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func validBearerToken(token string) bool {
	return token != "" && len(token) <= maxCurrentCoreTokenBytes && utf8.ValidString(token) &&
		!strings.ContainsAny(token, "\x00\r\n")
}

func validJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return json.Valid(trimmed) && len(trimmed) >= 2 &&
		trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
