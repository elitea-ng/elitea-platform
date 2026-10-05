package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// NewVerifier returns a fresh PKCE code verifier (RFC 7636 §4.1): 32 random
// bytes, base64url without padding, 43 characters.
func NewVerifier() string {
	return randomToken(32)
}

// NewState returns a fresh opaque `state`.
func NewState() string {
	return randomToken(16)
}

func randomToken(size int) string {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// S256Challenge derives the S256 code challenge of a verifier.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizationRequest is the RFC 8252 request a native app opens in the
// system browser.
type AuthorizationRequest struct {
	ClientID      string
	RedirectURI   string
	State         string
	Challenge     string
	DeviceName    string
	Platform      string
	ClientVersion string
}

// AuthorizeURL renders the request against the discovery document's
// authorization endpoint.
func AuthorizeURL(endpoint string, request AuthorizationRequest) string {
	values := url.Values{
		"response_type":         {"code"},
		"client_id":             {request.ClientID},
		"redirect_uri":          {request.RedirectURI},
		"state":                 {request.State},
		"code_challenge":        {request.Challenge},
		"code_challenge_method": {"S256"},
		"device_name":           {request.DeviceName},
		"platform":              {request.Platform},
	}
	if request.ClientVersion != "" {
		values.Set("client_version", request.ClientVersion)
	}
	return endpoint + "?" + values.Encode()
}

// Callback is what arrives at the redirect URI.
type Callback struct {
	Code             string
	State            string
	Issuer           string
	Error            string
	ErrorDescription string
}

// ParseCallback reads the authorization response from the redirect URI.
func ParseCallback(callback *url.URL) Callback {
	query := callback.Query()
	return Callback{
		Code:             query.Get("code"),
		State:            query.Get("state"),
		Issuer:           query.Get("iss"),
		Error:            query.Get("error"),
		ErrorDescription: query.Get("error_description"),
	}
}

// Verify applies the checks a native app must make before it redeems a code:
// the state it sent, and the issuer (RFC 9207) the discovery document named.
func (cb Callback) Verify(state, issuer string) error {
	if cb.State != state {
		return fmt.Errorf("callback state %q does not match the request's %q", cb.State, state)
	}
	if cb.Issuer != issuer {
		return fmt.Errorf("callback iss %q does not match the discovery issuer %q", cb.Issuer, issuer)
	}
	if cb.Error != "" {
		return fmt.Errorf("authorization refused: %s (%s)", cb.Error, cb.ErrorDescription)
	}
	if cb.Code == "" {
		return errors.New("callback carries no code")
	}
	return nil
}

// Tokens is a successful token response.
type Tokens struct {
	AccessToken           string         `json:"access_token"`
	TokenType             string         `json:"token_type"`
	ExpiresIn             int64          `json:"expires_in"`
	RefreshToken          string         `json:"refresh_token"`
	RefreshTokenExpiresIn int64          `json:"refresh_token_expires_in"`
	DeviceID              string         `json:"device_id"`
	ClientPolicy          map[string]any `json:"client_policy"`
}

// TokenError is a refused token or revocation request.
type TokenError struct {
	Response *Response
	Code     string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("token endpoint refused (%s): %s", e.Code, e.Response)
}

// ExchangeCode redeems an authorization code.
func (c *Client) ExchangeCode(ctx context.Context, endpoint, clientID, redirectURI, code, verifier string) (Tokens, error) {
	return c.token(ctx, endpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
}

// Refresh rotates a refresh token.
func (c *Client) Refresh(ctx context.Context, endpoint, clientID, refreshToken string) (Tokens, error) {
	return c.token(ctx, endpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
}

func (c *Client) token(ctx context.Context, endpoint string, form url.Values) (Tokens, error) {
	public := c.WithToken("")
	response, err := public.Do(ctx, http.MethodPost, endpoint, form, nil)
	if err != nil {
		return Tokens{}, err
	}
	if response.Status != http.StatusOK {
		return Tokens{}, &TokenError{Response: response, Code: errorCode(response)}
	}
	var tokens Tokens
	if err := response.JSON(&tokens); err != nil {
		return Tokens{}, err
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.TokenType != "Bearer" {
		return Tokens{}, fmt.Errorf("token response is incomplete: %s", response)
	}
	return tokens, nil
}

// Revoke revokes a token (RFC 7009); the whole device session goes with it.
func (c *Client) Revoke(ctx context.Context, endpoint, clientID, token string) (*Response, error) {
	return c.WithToken("").Do(ctx, http.MethodPost, endpoint, url.Values{
		"token": {token}, "client_id": {clientID},
	}, nil)
}

// errorCode reads `error` when it is a string (OAuth and the flat
// device_revoked body); an object-valued `error` (the nested API envelope)
// reads as "".
func errorCode(response *Response) string {
	var body map[string]json.RawMessage
	if json.Unmarshal(response.Body, &body) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(body["error"], &code) != nil {
		return ""
	}
	return code
}

// ErrorCode is errorCode for callers outside the package.
func ErrorCode(response *Response) string { return errorCode(response) }

// IsDeviceRevoked is the ADR's discriminator: a 401 whose `error` is the
// STRING device_revoked. An object-valued `error` is an ordinary rejection.
func IsDeviceRevoked(response *Response) bool {
	return response.Status == http.StatusUnauthorized && errorCode(response) == "device_revoked"
}
