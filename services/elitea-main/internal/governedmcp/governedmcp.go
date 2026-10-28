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
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// ProviderType is the grant and target provider type.
const ProviderType = mcpadapter.ProviderType

const (
	defaultTimeout   = 10 * time.Second
	maxMetadataBytes = 64 << 10
	maxTokenBytes    = 16 << 10
	maxScopes        = 32
	custodyVersion   = 1
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
	// HTTPClient reaches resource and authorization servers. Nil is a TLS
	// client with no redirects. Its Timeout is replaced by Timeout.
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
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	} else {
		copied := *client
		client = &copied
	}
	client.Timeout = cfg.Timeout
	// Metadata, registration and token endpoints never redirect; following
	// one would send a code or a secret somewhere else.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{cfg: cfg, http: client, now: time.Now}, nil
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
	resourceMeta, err := s.protectedResource(ctx, resource)
	if err != nil {
		return nil, "", err
	}
	as, err := s.authorizationServer(ctx, resourceMeta.AuthorizationServers[0])
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

// resourceMetadata is the RFC 9728 document.
type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// serverMetadata is the RFC 8414 / OpenID document, with the MCP CIMD flag.
type serverMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

// protectedResource reads the RFC 9728 metadata of resource: the path
// inserted well-known URI first, then the host root. The document must name
// the same resource.
func (s *Service) protectedResource(ctx context.Context, resource string) (resourceMetadata, error) {
	u, _ := url.Parse(resource)
	candidates := []string{wellKnown(u, "oauth-protected-resource", true)}
	if strings.Trim(u.Path, "/") != "" {
		candidates = append(candidates, wellKnown(u, "oauth-protected-resource", false))
	}
	for _, candidate := range candidates {
		var meta resourceMetadata
		if !s.getJSON(ctx, candidate, &meta) {
			continue
		}
		canonical, err := mcpadapter.CanonicalResource(meta.Resource)
		if err != nil || canonical != resource || len(meta.AuthorizationServers) == 0 || !validURL(meta.AuthorizationServers[0]) {
			return resourceMetadata{}, ErrMetadata
		}
		return meta, nil
	}
	return resourceMetadata{}, ErrMetadata
}

// authorizationServer reads RFC 8414 metadata, then OpenID discovery. The
// issuer must be the one asked for, endpoints https, and S256 supported.
func (s *Service) authorizationServer(ctx context.Context, issuer string) (serverMetadata, error) {
	u, err := httpsURL(issuer)
	if err != nil {
		return serverMetadata{}, ErrMetadata
	}
	candidates := []string{
		wellKnown(u, "oauth-authorization-server", true),
		strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration",
	}
	for _, candidate := range candidates {
		var meta serverMetadata
		if !s.getJSON(ctx, candidate, &meta) {
			continue
		}
		if strings.TrimSuffix(meta.Issuer, "/") != strings.TrimSuffix(issuer, "/") || !validURL(meta.AuthorizationEndpoint) ||
			!validURL(meta.TokenEndpoint) || (meta.RegistrationEndpoint != "" && !validURL(meta.RegistrationEndpoint)) ||
			!slices.Contains(meta.CodeChallengeMethodsSupported, "S256") {
			return serverMetadata{}, ErrMetadata
		}
		return meta, nil
	}
	return serverMetadata{}, ErrMetadata
}

// wellKnown is the well-known URI of u: path inserted (RFC 8414 §3.1,
// RFC 9728 §3.1) or at the host root.
func wellKnown(u *url.URL, name string, insertPath bool) string {
	path := ""
	if insertPath {
		path = strings.TrimSuffix(u.EscapedPath(), "/")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String() + "/.well-known/" + name + path
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

func (s *Service) getJSON(ctx context.Context, target string, into any) bool {
	raw, status, err := s.do(ctx, http.MethodGet, target, "", nil)
	return err == nil && status == http.StatusOK && json.Unmarshal(raw, into) == nil
}

func (s *Service) do(ctx context.Context, method, target, contentType string, body []byte) ([]byte, int, error) {
	if _, err := httpsURL(target); err != nil {
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
