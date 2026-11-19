package microsoftlogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
)

const (
	maxTokenResponse = 256 << 10
	exchangeTimeout  = 15 * time.Second
	// A token endpoint that answers no lifetime is read as the common hour.
	defaultAccessLifetime = time.Hour
	maxAccessLifetime     = 24 * time.Hour
)

// exchangeError is a refusal of the code exchange. It holds the Entra error code and
// the correlation id, and nothing of the response body.
type exchangeError struct {
	code          string
	status        int
	correlationID string
}

func (e *exchangeError) Error() string {
	return fmt.Sprintf("the token endpoint refused the code (%s, HTTP %d)", e.code, e.status)
}

var (
	errorCodePattern   = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)
	correlationPattern = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)
)

// tokens is what the code exchange yields that this flow keeps: an access token and
// when it expires. The refresh token, if Entra sent one, is discarded unread.
type tokens struct {
	access    string
	expiresAt time.Time
	idToken   string
}

type tokenBody struct {
	AccessToken   string `json:"access_token"`
	IDToken       string `json:"id_token"`
	ExpiresIn     any    `json:"expires_in"`
	Error         string `json:"error"`
	CorrelationID string `json:"correlation_id"`
}

// exchange redeems the code at the authority's own token endpoint, computed from
// the validated authority and never read from a discovery document. The client
// secret goes there and nowhere else.
func (s *Service) exchange(ctx context.Context, c client, code, verifier string, now time.Time) (tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.clientID},
		"code":          {code},
		"redirect_uri":  {s.redirectURI},
		"code_verifier": {verifier},
		"scope":         {strings.Join(c.scopes, " ")},
	}
	if !c.secret.Empty() {
		form.Set("client_secret", c.secret.Reveal())
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, microsoftauth.TokenEndpoint(c.authority),
		strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := s.do(request)
	if err != nil {
		// Never wrap err: a transport error can carry the request URL.
		return tokens{}, errors.New("the token endpoint could not be reached")
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse))
	if err != nil {
		return tokens{}, errors.New("the token endpoint answer could not be read")
	}
	var body tokenBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return tokens{}, &exchangeError{code: "invalid_response", status: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK || body.Error != "" || body.AccessToken == "" {
		code := "invalid_response"
		if errorCodePattern.MatchString(body.Error) {
			code = body.Error
		}
		correlation := ""
		if correlationPattern.MatchString(body.CorrelationID) {
			correlation = body.CorrelationID
		}
		return tokens{}, &exchangeError{code: code, status: response.StatusCode, correlationID: correlation}
	}
	return tokens{access: body.AccessToken, idToken: body.IDToken, expiresAt: now.Add(lifetime(body.ExpiresIn))}, nil
}

// lifetime reads expires_in, which Entra sends as a number or a string.
func lifetime(value any) time.Duration {
	var seconds float64
	switch typed := value.(type) {
	case float64:
		seconds = typed
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(typed, "%f", &parsed); err == nil {
			seconds = parsed
		}
	}
	if !(seconds > 0) { // also false for NaN
		return defaultAccessLifetime
	}
	// Clamp before converting: a float beyond the Duration range is undefined.
	return time.Duration(min(seconds, maxAccessLifetime.Seconds())) * time.Second
}

// identity is who signed in, as Entra reported it.
type identity struct {
	tid, oid string
	name     string
}

type idClaims struct {
	TID               string `json:"tid"`
	OID               string `json:"oid"`
	Aud               any    `json:"aud"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// readIdentity reads the claims of the id token the token endpoint returned over
// TLS. Per OIDC Core 3.1.3.7 the signature of such a token need not be checked; the
// audience must still be this client, so a token for another app is not accepted.
func readIdentity(idToken, clientID string) (identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return identity{}, errors.New("no id token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, errors.New("the id token is not readable")
	}
	var claims idClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return identity{}, errors.New("the id token is not readable")
	}
	if !audienceIs(claims.Aud, clientID) || !guidPattern.MatchString(claims.TID) || !guidPattern.MatchString(claims.OID) {
		return identity{}, errors.New("the id token does not name this client and a person")
	}
	name := claims.Name
	if strings.TrimSpace(name) == "" {
		name = claims.PreferredUsername
	}
	return identity{tid: strings.ToLower(claims.TID), oid: strings.ToLower(claims.OID), name: cleanName(name)}, nil
}

func audienceIs(aud any, clientID string) bool {
	switch typed := aud.(type) {
	case string:
		return strings.EqualFold(typed, clientID)
	case []any:
		for _, item := range typed {
			if value, ok := item.(string); ok && strings.EqualFold(value, clientID) {
				return true
			}
		}
	}
	return false
}

// cleanName keeps a display name printable and bounded: it is shown in a credential
// form, and it came from a directory the person's administrator controls.
func cleanName(name string) string {
	var out []rune
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			continue
		}
		out = append(out, r)
		if len(out) >= 128 {
			break
		}
	}
	return string(out)
}

// tenantMatches reports whether the signed-in person belongs to the tenant a
// single-tenant authority names. A multi-tenant alias or a domain name has nothing
// to compare, and passes.
func tenantMatches(authority, tid string) bool {
	parsed, err := microsoftapps.ParseAuthority(authority)
	if err != nil || parsed.MultiTenant() || !guidPattern.MatchString(parsed.Tenant) {
		return true
	}
	return strings.EqualFold(parsed.Tenant, tid)
}
