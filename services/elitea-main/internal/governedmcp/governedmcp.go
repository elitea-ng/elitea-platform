// Package governedmcp is the outbound half of governed MCP (ADR-0039,
// issue #1123): a remote MCP server's OAuth, held server side and released
// only through the credential broker.
//
// The server's authorization server is its own and differs per target, so
// it is discovered from the target's canonical resource URI: RFC 9728
// protected-resource metadata, then RFC 8414 (or OpenID) authorization
// server metadata. Service plugs that into internal/provideroauth's consent
// flow (provideroauth.TargetAuthorizers) and token source
// (provideroauth.Refresher), so the grant lives in provider_oauth_grants
// under provider type "mcp" and refreshes single-flight like every other
// provider grant (#1121).
//
// Every request (metadata, registration, code exchange, refresh) goes
// through the MCP authorization egress guard (ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST),
// the one the eliteacore MCP OAuth proxies use: the guard validates each URL
// before it is fetched and dials only an address it has just checked. The
// metadata walk is internal/mcpoauth's, shared with those proxies. A
// tenant-chosen MCP server therefore cannot make Main fetch from, or POST a
// code to, a private or loopback authorization server the operator did not
// allow.
//
// Every authorization request, code exchange and refresh carries the RFC
// 8707 `resource` parameter: the target's canonical resource URI. A Client
// ID Metadata Document is preferred; dynamic client registration is used
// only when the authorization server lacks CIMD support and the operator
// allows it.
//
// What a refresh needs (token endpoint, client id, client secret of a
// dynamically registered client, resource) is sealed into the refresh
// token the grant stores, so no schema change is needed and nothing leaves
// the grant's AES-GCM custody. No token, code, verifier or response body is
// logged or put in an error.
package governedmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	mcpadapter "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// ProviderType is the grant and target provider type.
const ProviderType = mcpadapter.ProviderType

const (
	defaultTimeout   = 10 * time.Second
	maxMetadataBytes = 64 << 10
	maxTokenBytes    = 16 << 10
	maxScopes        = 32
	// maxAuthorizationServers bounds the RFC 9728 list; Begin uses the first.
	maxAuthorizationServers = 16
	custodyVersion          = 1
)

var (
	// ErrRegistrationUnavailable is an authorization server with no CIMD
	// support where dynamic registration is not allowed or not offered.
	ErrRegistrationUnavailable = fmt.Errorf("%w: no permitted client registration", provideroauth.ErrProviderRejected)
	// ErrMetadata is a resource or authorization server whose metadata is
	// missing, malformed or does not name itself.
	ErrMetadata = fmt.Errorf("%w: unusable OAuth metadata", provideroauth.ErrProviderRejected)
)

var scopePattern = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]{1,256}$`)

// Config is the outbound composition.
type Config struct {
	// RedirectURI is exact: …/api/v2/credential_consent/mcp:callback.
	RedirectURI string
	// ClientMetadataURL is the Client ID Metadata Document URL, which is the
	// client_id under CIMD (Main serves it, ClientMetadata).
	ClientMetadataURL string
	// ClientName is the registered client name.
	ClientName string
	// AllowDynamicRegistration permits RFC 7591 registration with a server
	// that has no CIMD support. Default false.
	AllowDynamicRegistration bool
	// Guard is the MCP authorization egress guard. Required.
	Guard *egress.Guard
	// HTTPClient supplies the TLS trust of the client that reaches resource
	// and authorization servers; nil is the system roots. Its dialer, proxy
	// and redirect policy are always the guard's and none, and its Timeout
	// is replaced by Timeout.
	HTTPClient *http.Client
	// Timeout bounds one request. Default 10s, at most
	// provideroauth.MaxTimeout.
	Timeout time.Duration
}

// Service is provideroauth.TargetAuthorizers and provideroauth.Refresher
// for provider type "mcp". Safe for concurrent use.
type Service struct {
	cfg  Config
	http *http.Client
	meta mcpoauth.Fetcher
	now  func() time.Time
}

var (
	_ provideroauth.TargetAuthorizers = (*Service)(nil)
	_ provideroauth.Refresher         = (*Service)(nil)
)

// New validates cfg and builds the service.
func New(cfg Config) (*Service, error) {
	if !strings.HasSuffix(cfg.RedirectURI, provideroauth.CallbackPathPrefix+ProviderType+provideroauth.CallbackPathSuffix) {
		return nil, errors.New("governed MCP: the redirect URI must end with /api/v2/credential_consent/mcp:callback")
	}
	if _, err := httpsURL(cfg.ClientMetadataURL); err != nil {
		return nil, fmt.Errorf("governed MCP: client metadata URL: %w", err)
	}
	if strings.TrimSpace(cfg.ClientName) == "" {
		cfg.ClientName = "Elitea"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > provideroauth.MaxTimeout {
		return nil, fmt.Errorf("governed MCP: the timeout must be at most %s", provideroauth.MaxTimeout)
	}
	if cfg.Guard == nil {
		return nil, errors.New("governed MCP: the MCP authorization egress guard is required")
	}
	client := guardedClient(cfg.Guard, cfg.HTTPClient)
	client.Timeout = cfg.Timeout
	return &Service{
		cfg: cfg, http: client, now: time.Now,
		meta: mcpoauth.Fetcher{
			Do: client.Do, Validate: cfg.Guard.Validate,
			Flags: []string{"client_id_metadata_document_supported"},
		},
	}, nil
}

// guardedClient is the guard's transport with base's TLS trust: the dialer
// is the guard's, there is no proxy (the guard would check the proxy's
// address instead), and no redirect is followed, since metadata,
// registration and token endpoints never redirect and following one would
// send a code or a secret somewhere else.
func guardedClient(guard *egress.Guard, base *http.Client) *http.Client {
	transport := guard.Transport()
	if base != nil {
		if configured, ok := base.Transport.(*http.Transport); ok && configured.TLSClientConfig != nil {
			transport.TLSClientConfig = configured.TLSClientConfig.Clone()
		}
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Provider is "mcp".
func (s *Service) Provider() string { return ProviderType }

// RedirectURI is the exact callback.
func (s *Service) RedirectURI() string { return s.cfg.RedirectURI }

// Timeout bounds one token request.
func (s *Service) Timeout() time.Duration { return s.cfg.Timeout }

// binding is the per-target client registration of one consent. It is
// sealed with the PKCE verifier between start and callback, and (as
// custody) with the refresh token afterwards.
type binding struct {
	Version               int    `json:"v"`
	Resource              string `json:"resource"`
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint"`
	ClientID              string `json:"client_id"`
	ClientSecret          string `json:"client_secret,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	RefreshToken          string `json:"refresh_token,omitempty"`
}

func (b binding) valid() bool {
	if b.Version != custodyVersion || b.ClientID == "" || len(b.ClientID) > 2048 || len(b.ClientSecret) > maxTokenBytes {
		return false
	}
	for _, endpoint := range []string{b.Resource, b.Issuer, b.TokenEndpoint} {
		if _, err := httpsURL(endpoint); err != nil {
			return false
		}
	}
	return b.AuthorizationEndpoint == "" || validURL(b.AuthorizationEndpoint)
}

func validURL(raw string) bool { _, err := httpsURL(raw); return err == nil }

// Begin discovers the target's authorization server and registers the
// client: CIMD when the server supports it, else dynamic registration when
// allowed.
func (s *Service) Begin(ctx context.Context, target credentialgrant.TargetBinding) (provideroauth.Authorizer, string, error) {
	resource, err := targetResource(target)
	if err != nil {
		return nil, "", err
	}
	resourceMeta, as, err := s.discover(ctx, resource)
	if err != nil {
		return nil, "", err
	}
	b := binding{
		Version: custodyVersion, Resource: resource, Issuer: as.Issuer,
		AuthorizationEndpoint: as.AuthorizationEndpoint, TokenEndpoint: as.TokenEndpoint,
		Scope: scopeOf(resourceMeta.ScopesSupported),
	}
	switch {
	case as.ClientIDMetadataDocumentSupported:
		b.ClientID = s.cfg.ClientMetadataURL
	case s.cfg.AllowDynamicRegistration && as.RegistrationEndpoint != "":
		b.ClientID, b.ClientSecret, err = s.register(ctx, as.RegistrationEndpoint, b.Scope)
		if err != nil {
			return nil, "", err
		}
	default:
		return nil, "", ErrRegistrationUnavailable
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		return nil, "", ErrMetadata
	}
	return authorizer{service: s, binding: b}, string(encoded), nil
}

// Resume rebuilds the start's authorizer from its sealed binding. A binding
// for another resource than the target is refused.
func (s *Service) Resume(_ context.Context, target credentialgrant.TargetBinding, sealed string) (provideroauth.Authorizer, error) {
	resource, err := targetResource(target)
	if err != nil {
		return nil, err
	}
	var b binding
	if json.Unmarshal([]byte(sealed), &b) != nil || !b.valid() || b.Resource != resource || b.AuthorizationEndpoint == "" {
		return nil, provideroauth.ErrStateInvalid
	}
	return authorizer{service: s, binding: b}, nil
}

func targetResource(target credentialgrant.TargetBinding) (string, error) {
	if target.ProviderType != ProviderType {
		return "", provideroauth.ErrTargetNotFound
	}
	resource, err := mcpadapter.CanonicalResource(target.TargetIdentity)
	if err != nil || resource != target.TargetIdentity {
		return "", provideroauth.ErrTargetNotFound
	}
	return resource, nil
}

func scopeOf(supported []string) string {
	var scopes []string
	for _, scope := range supported {
		if len(scopes) == maxScopes {
			break
		}
		if scopePattern.MatchString(scope) && !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return strings.Join(scopes, " ")
}

// authorizer is one consent's authorization request and code exchange.
type authorizer struct {
	service *Service
	binding binding
}

// AuthorizationURL carries the RFC 8707 resource and the S256 challenge.
func (a authorizer) AuthorizationURL(state, challenge string) string {
	u, _ := url.Parse(a.binding.AuthorizationEndpoint)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", a.binding.ClientID)
	q.Set("redirect_uri", a.service.cfg.RedirectURI)
	if a.binding.Scope != "" {
		q.Set("scope", a.binding.Scope)
	}
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", a.binding.Resource)
	u.RawQuery = q.Encode()
	return u.String()
}

// Exchange trades the code with the verifier, the exact redirect URI and
// the resource. A refresh token comes back sealed with what its refresh
// needs.
func (a authorizer) Exchange(ctx context.Context, code, verifier string) (provideroauth.TokenResponse, error) {
	if code == "" || len(code) > maxTokenBytes || verifier == "" {
		return provideroauth.TokenResponse{}, provideroauth.ErrProviderRejected
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.service.cfg.RedirectURI},
		"client_id": {a.binding.ClientID}, "code_verifier": {verifier}, "resource": {a.binding.Resource},
	}
	return a.service.token(ctx, a.binding, form)
}

// Refresh opens the custody the exchange sealed, refreshes with the
// resource, and seals a rotated refresh token again. A provider that does
// not rotate answers no refresh token, and the grant keeps its own.
func (s *Service) Refresh(ctx context.Context, sealed string) (provideroauth.TokenResponse, error) {
	var b binding
	if json.Unmarshal([]byte(sealed), &b) != nil || !b.valid() || b.RefreshToken == "" {
		return provideroauth.TokenResponse{}, provideroauth.ErrInvalidGrant
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {b.RefreshToken},
		"client_id": {b.ClientID}, "resource": {b.Resource},
	}
	return s.token(ctx, b, form)
}

func (s *Service) token(ctx context.Context, b binding, form url.Values) (provideroauth.TokenResponse, error) {
	if b.ClientSecret != "" {
		form.Set("client_secret", b.ClientSecret)
	}
	body := []byte(form.Encode())
	defer clear(body)
	raw, status, err := s.do(ctx, http.MethodPost, b.TokenEndpoint, "application/x-www-form-urlencoded", body)
	defer clear(raw)
	if err != nil {
		return provideroauth.TokenResponse{}, provideroauth.ErrProviderUnavailable
	}
	var parsed struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
		TokenType    string          `json:"token_type"`
		Error        string          `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &parsed)
	switch {
	case status >= 500 || status == http.StatusTooManyRequests:
		return provideroauth.TokenResponse{}, provideroauth.ErrProviderUnavailable
	case status != http.StatusOK:
		if decodeErr == nil && parsed.Error == "invalid_grant" {
			return provideroauth.TokenResponse{}, provideroauth.ErrInvalidGrant
		}
		return provideroauth.TokenResponse{}, provideroauth.ErrProviderRejected
	case decodeErr != nil, parsed.AccessToken == "", len(parsed.AccessToken) > maxTokenBytes,
		len(parsed.RefreshToken) > maxTokenBytes, !strings.EqualFold(parsed.TokenType, "bearer"):
		return provideroauth.TokenResponse{}, provideroauth.ErrProviderRejected
	}
	out := provideroauth.TokenResponse{AccessToken: parsed.AccessToken}
	if seconds, ok := expiresIn(parsed.ExpiresIn); ok {
		out.ExpiresAt = s.now().Add(time.Duration(seconds) * time.Second).UTC().Truncate(time.Microsecond)
	}
	out.Scopes = strings.Fields(scopeOf(strings.Fields(parsed.Scope)))
	if parsed.RefreshToken != "" {
		b.RefreshToken = parsed.RefreshToken
		custody, err := json.Marshal(b)
		if err != nil {
			return provideroauth.TokenResponse{}, provideroauth.ErrProviderRejected
		}
		out.RefreshToken = string(custody)
	}
	return out, nil
}

func expiresIn(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	seconds, err := json.Number(strings.Trim(string(raw), `"`)).Int64()
	if err != nil || seconds < 1 || seconds > 366*24*3600 {
		return 0, false
	}
	return seconds, true
}

// resourceMetadata is the part of the RFC 9728 document Begin uses.
type resourceMetadata struct {
	AuthorizationServers []string
	ScopesSupported      []string
}

// serverMetadata is the part of the RFC 8414 / OpenID document Begin uses,
// with the MCP CIMD flag.
type serverMetadata struct {
	Issuer                            string
	AuthorizationEndpoint             string
	TokenEndpoint                     string
	RegistrationEndpoint              string
	ClientIDMetadataDocumentSupported bool
}

// discover walks resource's metadata with internal/mcpoauth's walk, through
// the guard. The RFC 9728 document must name the same canonical resource,
// and its first authorization server must answer with https endpoints and
// S256.
func (s *Service) discover(ctx context.Context, resource string) (resourceMetadata, serverMetadata, error) {
	endpoint, err := httpsURL(resource)
	if err != nil {
		return resourceMetadata{}, serverMetadata{}, ErrMetadata
	}
	found, err := s.meta.Discover(ctx, endpoint, "", maxAuthorizationServers, false, nil)
	if err != nil || len(found.Issuers) == 0 {
		return resourceMetadata{}, serverMetadata{}, ErrMetadata
	}
	named, _ := found.Resource["resource"].(string)
	if canonical, err := mcpadapter.CanonicalResource(named); err != nil || canonical != resource || !validURL(found.Servers[0]) {
		return resourceMetadata{}, serverMetadata{}, ErrMetadata
	}
	scopes, _ := mcpoauth.BoundedMetadataList(found.Resource["scopes_supported"])
	issuer := found.Issuers[0]
	text := func(key string) string { value, _ := issuer[key].(string); return value }
	as := serverMetadata{
		Issuer: text("issuer"), AuthorizationEndpoint: text("authorization_endpoint"),
		TokenEndpoint: text("token_endpoint"), RegistrationEndpoint: text("registration_endpoint"),
	}
	as.ClientIDMetadataDocumentSupported, _ = issuer["client_id_metadata_document_supported"].(bool)
	methods, _ := issuer["code_challenge_methods_supported"].([]string)
	if !validURL(as.AuthorizationEndpoint) || !validURL(as.TokenEndpoint) ||
		(as.RegistrationEndpoint != "" && !validURL(as.RegistrationEndpoint)) || !slices.Contains(methods, "S256") {
		return resourceMetadata{}, serverMetadata{}, ErrMetadata
	}
	return resourceMetadata{AuthorizationServers: found.Servers, ScopesSupported: scopes}, as, nil
}

// register is RFC 7591 registration of a client for the authorization code
// grant with the exact redirect URI.
func (s *Service) register(ctx context.Context, endpoint, scope string) (string, string, error) {
	request := map[string]any{
		"client_name": s.cfg.ClientName, "redirect_uris": []string{s.cfg.RedirectURI},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "client_secret_post",
	}
	if scope != "" {
		request["scope"] = scope
	}
	body, _ := json.Marshal(request)
	raw, status, err := s.do(ctx, http.MethodPost, endpoint, "application/json", body)
	defer clear(raw)
	if err != nil {
		return "", "", provideroauth.ErrProviderUnavailable
	}
	var registered struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if (status != http.StatusCreated && status != http.StatusOK) || json.Unmarshal(raw, &registered) != nil ||
		registered.ClientID == "" || len(registered.ClientID) > 2048 || len(registered.ClientSecret) > maxTokenBytes {
		return "", "", ErrRegistrationUnavailable
	}
	return registered.ClientID, registered.ClientSecret, nil
}

func (s *Service) do(ctx context.Context, method, target, contentType string, body []byte) ([]byte, int, error) {
	if _, err := httpsURL(target); err != nil {
		return nil, 0, err
	}
	// The refusal before any connection; the guarded dialer checks again.
	if err := s.cfg.Guard.Validate(ctx, target); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		// The error names the URL only; it is still not returned.
		return nil, 0, errors.New("governed MCP: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil || len(raw) > maxMetadataBytes {
		clear(raw)
		return nil, 0, errors.New("governed MCP: response too large")
	}
	return raw, resp.StatusCode, nil
}

// httpsURL accepts an absolute https URL with a host and no user info or
// fragment.
func httpsURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" ||
		strings.ContainsAny(raw, " \t\r\n#") || len(raw) > 2048 {
		return nil, errors.New("not an absolute https URL without user info or fragment")
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return nil, errors.New("invalid port")
		}
	}
	return u, nil
}

// EnvelopeServes is the target binding rule an outbound MCP call applies:
// an envelope's access token is sent only to the server whose canonical
// resource URI is the envelope's target identity. A token released for
// server A is never sent to server B.
func EnvelopeServes(targetIdentity, serverURL string) bool {
	canonical, err := mcpadapter.CanonicalResource(serverURL)
	return err == nil && targetIdentity != "" && canonical == targetIdentity
}
