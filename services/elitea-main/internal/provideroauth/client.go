package provideroauth

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
)

// Flavor is the token endpoint dialect of a provider.
type Flavor string

const (
	// FlavorAtlassian is Atlassian OAuth 2.0 (3LO): JSON token requests,
	// audience=api.atlassian.com and prompt=consent on the authorization
	// request, and a refresh token that rotates on every use (a refresh
	// token is single use).
	FlavorAtlassian Flavor = "atlassian"
	// FlavorAha is Aha! OAuth2: form-encoded token requests. A refresh
	// token, when the provider returns one, is kept until it returns another.
	FlavorAha Flavor = "aha"
)

// Atlassian 3LO defaults. Aha! endpoints are per account and have none.
const (
	AtlassianAuthorizeURL = "https://auth.atlassian.com/authorize"
	AtlassianTokenURL     = "https://auth.atlassian.com/oauth/token"
	atlassianAudience     = "api.atlassian.com"
)

const (
	defaultProviderTimeout = 10 * time.Second
	maxTokenResponseBytes  = 64 << 10
	maxTokenBytes          = 16 << 10
	// CallbackPathPrefix and CallbackPathSuffix frame the only redirect URI
	// path a provider may be configured with.
	CallbackPathPrefix = "/api/v2/credential_consent/"
	CallbackPathSuffix = ":callback"
)

var (
	// ErrInvalidGrant is the provider's invalid_grant: the code or refresh
	// token is unknown, used or expired. The user must consent again.
	ErrInvalidGrant = errors.New("provideroauth: the provider refused the grant")
	// ErrProviderRejected is any other refusal of a token request.
	ErrProviderRejected = errors.New("provideroauth: the provider refused the token request")
	// ErrProviderUnavailable is a network failure, a timeout or a 5xx.
	ErrProviderUnavailable = errors.New("provideroauth: the provider token endpoint is unavailable")
)

var scopePattern = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]{1,256}$`)

// ProviderConfig is one provider type's OAuth application.
type ProviderConfig struct {
	// Provider is the adapter id (jira, confluence, aha).
	Provider     string
	Flavor       Flavor
	ClientID     string
	ClientSecret string
	AuthorizeURL string
	TokenURL     string
	// RedirectURI is exact: https (http only for a loopback host), no query
	// or fragment, path /api/v2/credential_consent/<provider>:callback.
	RedirectURI string
	Scopes      []string
	// Timeout bounds one token request. Default 10s.
	Timeout time.Duration
}

// String keeps the client secret out of logs.
func (p ProviderConfig) String() string {
	return fmt.Sprintf("provideroauth.ProviderConfig{provider=%s flavor=%s client_secret=[redacted]}", p.Provider, p.Flavor)
}

// GoString keeps the client secret out of %#v.
func (p ProviderConfig) GoString() string { return p.String() }

func (p ProviderConfig) validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("provideroauth: provider %q: %s", p.Provider, fmt.Sprintf(format, args...))
	}
	if !providerPattern.MatchString(p.Provider) {
		return fail("the provider type must be one lowercase segment")
	}
	if p.Flavor != FlavorAtlassian && p.Flavor != FlavorAha {
		return fail("flavor %q is unknown", p.Flavor)
	}
	if strings.TrimSpace(p.ClientID) == "" || strings.TrimSpace(p.ClientSecret) == "" {
		return fail("a client id and a client secret are required")
	}
	if err := endpoint(p.AuthorizeURL); err != nil {
		return fail("authorize URL: %v", err)
	}
	if err := endpoint(p.TokenURL); err != nil {
		return fail("token URL: %v", err)
	}
	if err := redirectURI(p.RedirectURI, p.Provider); err != nil {
		return fail("redirect URI: %v", err)
	}
	// Atlassian 3LO needs scopes (offline_access for a refresh token); Aha!
	// OAuth2 has none.
	if (p.Flavor == FlavorAtlassian && len(p.Scopes) == 0) || len(p.Scopes) > 64 {
		return fail("between 1 and 64 scopes are required")
	}
	for i, scope := range p.Scopes {
		if !scopePattern.MatchString(scope) || slices.Contains(p.Scopes[:i], scope) {
			return fail("scope %d is malformed or repeated", i)
		}
	}
	if p.Timeout < 0 {
		return fail("a negative timeout")
	}
	return nil
}

// loopback reports whether host names this machine; only a loopback
// endpoint may be plain http (tests and a local emulator).
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func absolute(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" ||
		strings.ContainsAny(raw, " \t\r\n#") {
		return nil, errors.New("not an absolute URL without user info or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, errors.New("plain http is allowed only for a loopback host")
		}
	default:
		return nil, errors.New("the scheme must be https")
	}
	return u, nil
}

func endpoint(raw string) error {
	_, err := absolute(raw)
	return err
}

func redirectURI(raw, provider string) error {
	u, err := absolute(raw)
	if err != nil {
		return err
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("a query is not allowed")
	}
	// A base path in front is allowed (a deployment behind a path prefix);
	// the callback route itself is fixed.
	if want := CallbackPathPrefix + provider + CallbackPathSuffix; !strings.HasSuffix(u.Path, want) || u.RawPath != "" {
		return fmt.Errorf("the path must end with %s", want)
	}
	return nil
}

// TokenResponse is a verified token endpoint answer. It is credential
// material: never log it or put it in durable input.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is zero when the provider stated no lifetime.
	ExpiresAt time.Time
	Scopes    []string
}

// String keeps the tokens out of logs.
func (TokenResponse) String() string { return "provideroauth.TokenResponse{tokens=[redacted]}" }

// GoString keeps the tokens out of %#v.
func (t TokenResponse) GoString() string { return t.String() }

// Client is one provider's token endpoint client.
type Client struct {
	cfg  ProviderConfig
	http *http.Client
	now  func() time.Time
}

// NewClient validates cfg and builds the client. A nil httpClient is a
// client with the configured timeout, TLS verification and no redirects.
func NewClient(cfg ProviderConfig, httpClient *http.Client) (*Client, error) {
	cfg.Scopes = slices.Clone(cfg.Scopes)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultProviderTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	} else {
		copied := *httpClient
		httpClient = &copied
	}
	httpClient.Timeout = cfg.Timeout
	// A token endpoint never redirects; following one would send the
	// client secret and the code somewhere else.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{cfg: cfg, http: httpClient, now: time.Now}, nil
}

// Provider is the provider type the client serves.
func (c *Client) Provider() string { return c.cfg.Provider }

// RedirectURI is the exact configured redirect URI.
func (c *Client) RedirectURI() string { return c.cfg.RedirectURI }

// Timeout is the bound of one token request.
func (c *Client) Timeout() time.Duration { return c.cfg.Timeout }

// AuthorizationURL is the provider authorization request for state and the
// S256 PKCE challenge.
func (c *Client) AuthorizationURL(state, challenge string) string {
	u, _ := url.Parse(c.cfg.AuthorizeURL)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURI)
	if len(c.cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if c.cfg.Flavor == FlavorAtlassian {
		q.Set("audience", atlassianAudience)
		q.Set("prompt", "consent")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Exchange trades an authorization code for tokens, with the PKCE
// verifier and the exact redirect URI.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (TokenResponse, error) {
	if code == "" || len(code) > maxTokenBytes || verifier == "" {
		return TokenResponse{}, ErrProviderRejected
	}
	return c.token(ctx, [][2]string{
		{"grant_type", "authorization_code"},
		{"client_id", c.cfg.ClientID},
		{"client_secret", c.cfg.ClientSecret},
		{"code", code},
		{"redirect_uri", c.cfg.RedirectURI},
		{"code_verifier", verifier},
	})
}

// Refresh trades a refresh token for new tokens.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenResponse, error) {
	if refreshToken == "" {
		return TokenResponse{}, ErrInvalidGrant
	}
	return c.token(ctx, [][2]string{
		{"grant_type", "refresh_token"},
		{"client_id", c.cfg.ClientID},
		{"client_secret", c.cfg.ClientSecret},
		{"refresh_token", refreshToken},
	})
}

func (c *Client) token(ctx context.Context, fields [][2]string) (TokenResponse, error) {
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	if c.cfg.Flavor == FlavorAtlassian {
		object := make(map[string]string, len(fields))
		for _, f := range fields {
			object[f[0]] = f[1]
		}
		body, _ = json.Marshal(object)
		contentType = "application/json"
	} else {
		form := url.Values{}
		for _, f := range fields {
			form.Set(f[0], f[1])
		}
		body = []byte(form.Encode())
	}
	defer clear(body)
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, bytes.NewReader(body))
	if err != nil {
		return TokenResponse{}, ErrProviderUnavailable
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The error names the URL only; it is still not returned.
		return TokenResponse{}, ErrProviderUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	defer clear(raw)
	if err != nil || len(raw) > maxTokenResponseBytes {
		return TokenResponse{}, ErrProviderUnavailable
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
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return TokenResponse{}, ErrProviderUnavailable
	case resp.StatusCode != http.StatusOK:
		if decodeErr == nil && parsed.Error == "invalid_grant" {
			return TokenResponse{}, ErrInvalidGrant
		}
		return TokenResponse{}, ErrProviderRejected
	case decodeErr != nil:
		return TokenResponse{}, ErrProviderRejected
	}
	if parsed.AccessToken == "" || len(parsed.AccessToken) > maxTokenBytes || len(parsed.RefreshToken) > maxTokenBytes ||
		(parsed.TokenType != "" && !strings.EqualFold(parsed.TokenType, "bearer")) {
		return TokenResponse{}, ErrProviderRejected
	}
	out := TokenResponse{AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken}
	if seconds, ok := expiresIn(parsed.ExpiresIn); ok {
		out.ExpiresAt = c.now().Add(time.Duration(seconds) * time.Second).UTC().Truncate(time.Microsecond)
	}
	for _, scope := range strings.Fields(parsed.Scope) {
		if len(out.Scopes) == 64 {
			break
		}
		if scopePattern.MatchString(scope) && !slices.Contains(out.Scopes, scope) {
			out.Scopes = append(out.Scopes, scope)
		}
	}
	return out, nil
}

// expiresIn reads expires_in as a number or a numeric string; a value
// outside one second to one year is no stated lifetime.
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
