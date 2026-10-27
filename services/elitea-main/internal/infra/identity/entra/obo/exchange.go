package obo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entraoauth"
)

const (
	grantTypeJWTBearer      = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	clientAssertionTypeJWT  = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	maxTokenResponseBytes   = 1 << 20
	maxAssertionBytes       = 16 << 10
	maxScopes               = 16
	maxScopeBytes           = 256
	maxClaimsBytes          = entraoauth.MaxClaimsBytes
	defaultExchangeTimeout  = 10 * time.Second
	minIssuedTokenLifetime  = 30 * time.Second
	clientCapabilityCP1     = "cp1"
	clientRequestIDHeader   = "client-request-id"
	maxAllowedTenants       = 64
	maxAcceptedAudiences    = 8
	maxConfigurationTextLen = 512
)

// tenantIDPattern is a lower-case GUID. "common", "organizations" and
// "consumers" are not tenants and never match.
var tenantIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidTenantID reports whether tid is a lower-case GUID tenant id.
func ValidTenantID(tid string) bool { return tenantIDPattern.MatchString(tid) }

// endpoint is the token endpoint half both exchangers share.
type endpoint struct {
	authority string
	tenants   []string
	client    *http.Client
	now       func() time.Time
}

func newEndpoint(authority string, tenants []string, client *http.Client, now func() time.Time) (endpoint, error) {
	parsed, err := url.Parse(authority)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return endpoint{}, ErrInvalidConfiguration
	}
	if len(tenants) == 0 || len(tenants) > maxAllowedTenants {
		return endpoint{}, ErrInvalidConfiguration
	}
	for _, tid := range tenants {
		if !ValidTenantID(tid) {
			return endpoint{}, ErrInvalidConfiguration
		}
	}
	if client == nil {
		client = &http.Client{Timeout: defaultExchangeTimeout}
	}
	// A redirect from a token endpoint is never followed: it would resend
	// the assertion to wherever it points.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = defaultExchangeTimeout
	}
	if now == nil {
		now = time.Now
	}
	return endpoint{
		authority: "https://" + parsed.Host,
		tenants:   slices.Clone(tenants),
		client:    &bounded,
		now:       now,
	}, nil
}

func (e endpoint) tokenURL(tid string) string {
	return e.authority + "/" + tid + "/oauth2/v2.0/token"
}

// checkAssertion is the local half of the assertion contract: the fields
// custody recorded, and the token payload agreeing with them. audiences are
// the values this exchanger accepts as `aud`.
func (e endpoint) checkAssertion(a Assertion, audiences []string) error {
	if a.Token == "" || len(a.Token) > maxAssertionBytes || strings.ContainsFunc(a.Token, unicode.IsControl) ||
		!ValidTenantID(a.TenantID) || !ValidTenantID(a.ObjectID) || a.Audience == "" {
		return ErrInvalidRequest
	}
	if !slices.Contains(e.tenants, a.TenantID) {
		return ErrInvalidRequest
	}
	if !a.ExpiresAt.After(e.now()) {
		return ErrInteractionRequired
	}
	if !slices.Contains(audiences, a.Audience) {
		return ErrAudienceMismatch
	}
	// Defence in depth: custody validated the signature at sign-in; here the
	// payload must still name the same audience and user.
	payload, ok := unverifiedPayload(a.Token)
	if !ok {
		return ErrInvalidRequest
	}
	if !payload.hasAudience(a.Audience) {
		return ErrAudienceMismatch
	}
	if payload.TenantID != a.TenantID || payload.ObjectID != a.ObjectID {
		return ErrInvalidRequest
	}
	return nil
}

func checkScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > maxScopes {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope == "" || len(scope) > maxScopeBytes || strings.ContainsFunc(scope, unicode.IsSpace) ||
			strings.ContainsFunc(scope, unicode.IsControl) {
			return ErrInvalidRequest
		}
		if _, dup := seen[scope]; dup {
			return ErrInvalidRequest
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// claimsParameter returns the `claims` form value: the challenge being
// answered, merged with the cp1 client capability so Entra issues
// CAE-capable tokens.
func claimsParameter(challenge string) (string, error) {
	document := map[string]any{}
	if challenge != "" {
		if len(challenge) > maxClaimsBytes {
			return "", ErrInvalidRequest
		}
		raw := []byte(challenge)
		if !json.Valid(raw) {
			decoded, err := base64.StdEncoding.DecodeString(challenge)
			if err != nil {
				decoded, err = base64.RawURLEncoding.DecodeString(challenge)
			}
			if err != nil || !json.Valid(decoded) {
				return "", ErrInvalidRequest
			}
			raw = decoded
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			return "", ErrInvalidRequest
		}
	}
	accessToken, _ := document["access_token"].(map[string]any)
	if accessToken == nil {
		accessToken = map[string]any{}
	}
	accessToken["xms_cc"] = map[string]any{"values": []string{clientCapabilityCP1}}
	document["access_token"] = accessToken
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", ErrInvalidRequest
	}
	return string(encoded), nil
}

// post sends one token request and maps the answer.
func (e endpoint) post(ctx context.Context, tid string, form url.Values, correlationID string) (AccessToken, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL(tid), strings.NewReader(form.Encode()))
	if err != nil {
		return AccessToken{}, ErrInvalidRequest
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(clientRequestIDHeader, correlationID)
	response, err := e.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return AccessToken{}, ctxErr
		}
		return AccessToken{}, ErrUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes+1))
	_ = response.Body.Close()
	if readErr != nil || len(body) > maxTokenResponseBytes {
		return AccessToken{}, ErrUnavailable
	}
	switch response.StatusCode {
	case http.StatusOK:
		if !entraoauth.JSONContentType(response.Header) {
			return AccessToken{}, ErrUnavailable
		}
		return e.issuedToken(body)
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		if !entraoauth.JSONContentType(response.Header) {
			return AccessToken{}, ErrUnavailable
		}
		return AccessToken{}, mapOAuthError(body)
	default:
		return AccessToken{}, ErrUnavailable
	}
}

func (e endpoint) issuedToken(body []byte) (AccessToken, error) {
	var issued struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Scope       string          `json:"scope"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&issued); err != nil {
		return AccessToken{}, ErrUnavailable
	}
	seconds, ok := entraoauth.ExpiresInSeconds(issued.ExpiresIn)
	if issued.AccessToken == "" || len(issued.AccessToken) > maxAssertionBytes ||
		strings.ContainsFunc(issued.AccessToken, unicode.IsControl) ||
		!strings.EqualFold(issued.TokenType, "Bearer") || !ok {
		return AccessToken{}, ErrUnavailable
	}
	lifetime := time.Duration(seconds) * time.Second
	if lifetime < minIssuedTokenLifetime {
		return AccessToken{}, ErrUnavailable
	}
	return AccessToken{
		Token:     issued.AccessToken,
		ExpiresAt: e.now().Add(lifetime).UTC(),
		Scopes:    strings.Fields(issued.Scope),
	}, nil
}

// mapOAuthError maps the shared classification to this package's errors.
// An application-disabled refusal is a plain rejection here.
func mapOAuthError(body []byte) error {
	c := entraoauth.Classify(body)
	switch c.Kind {
	case entraoauth.KindAgentConsent:
		return ErrAgentConsentNotGranted
	case entraoauth.KindClaimsChallenge:
		return &ErrClaimsChallenge{Claims: c.Claims}
	case entraoauth.KindAudienceMismatch:
		return ErrAudienceMismatch
	case entraoauth.KindInteractionRequired:
		return ErrInteractionRequired
	case entraoauth.KindUnavailable:
		return ErrUnavailable
	}
	return ErrRejected
}

// tokenPayload is the part of an unverified JWT payload the local checks read.
type tokenPayload struct {
	Audience json.RawMessage `json:"aud"`
	TenantID string          `json:"tid"`
	ObjectID string          `json:"oid"`
}

func (p tokenPayload) hasAudience(audience string) bool {
	var single string
	if err := json.Unmarshal(p.Audience, &single); err == nil {
		return single == audience
	}
	var list []string
	if err := json.Unmarshal(p.Audience, &list); err == nil {
		return len(list) == 1 && list[0] == audience
	}
	return false
}

func unverifiedPayload(token string) (tokenPayload, bool) {
	var payload tokenPayload
	ok := entraoauth.UnverifiedPayload(token, &payload)
	return payload, ok
}

func validAudiences(audiences []string) bool {
	if len(audiences) == 0 || len(audiences) > maxAcceptedAudiences {
		return false
	}
	for _, audience := range audiences {
		if !validConfigurationText(audience) {
			return false
		}
	}
	return true
}

func validConfigurationText(value string) bool {
	return value != "" && len(value) <= maxConfigurationTextLen && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl) && !strings.ContainsFunc(value, unicode.IsSpace)
}
