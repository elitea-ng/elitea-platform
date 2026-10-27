package obo

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ClassicConfig configures the middle-tier on-behalf-of exchanger.
type ClassicConfig struct {
	// Authority is the Entra authority, "https://login.microsoftonline.com".
	// The token endpoint is <Authority>/<tid>/oauth2/v2.0/token.
	Authority string
	// ClientID is the middle-tier application id.
	ClientID string
	// AssertionAudiences are the accepted assertion `aud` values. Empty
	// means ClientID and api://<ClientID>.
	AssertionAudiences []string
	// AllowedTenants are the tenant ids an assertion may come from.
	AllowedTenants []string
	Credential     ClientCredential
	// AllowClientSecret must be true for a ClientSecretCredential. The
	// composition root sets it from the policy revision.
	AllowClientSecret bool
	HTTPClient        *http.Client
	Now               func() time.Time
}

// ClassicOBO is the Microsoft identity platform on-behalf-of exchanger.
type ClassicOBO struct {
	endpoint   endpoint
	clientID   string
	audiences  []string
	credential ClientCredential
}

// NewClassicOBO validates config.
func NewClassicOBO(config ClassicConfig) (*ClassicOBO, error) {
	if !validConfigurationText(config.ClientID) || config.Credential == nil ||
		(config.Credential.isSecret() && !config.AllowClientSecret) {
		return nil, ErrInvalidConfiguration
	}
	audiences := config.AssertionAudiences
	if len(audiences) == 0 {
		audiences = []string{config.ClientID, "api://" + config.ClientID}
	}
	if !validAudiences(audiences) {
		return nil, ErrInvalidConfiguration
	}
	endpoint, err := newEndpoint(config.Authority, config.AllowedTenants, config.HTTPClient, config.Now)
	if err != nil {
		return nil, err
	}
	return &ClassicOBO{
		endpoint:   endpoint,
		clientID:   config.ClientID,
		audiences:  slices.Clone(audiences),
		credential: config.Credential,
	}, nil
}

// Audiences are the assertion audiences this exchanger accepts.
func (o *ClassicOBO) Audiences() []string { return slices.Clone(o.audiences) }

// Exchange runs grant_type=jwt-bearer with requested_token_use=on_behalf_of.
func (o *ClassicOBO) Exchange(ctx context.Context, a Assertion, r ExchangeRequest) (AccessToken, error) {
	if err := ctx.Err(); err != nil {
		return AccessToken{}, err
	}
	if o == nil {
		return AccessToken{}, ErrInvalidConfiguration
	}
	if r.AgentID != "" || r.ExecutionID == "" || checkScopes(r.Scopes) != nil {
		return AccessToken{}, ErrInvalidRequest
	}
	if err := o.endpoint.checkAssertion(a, o.audiences); err != nil {
		return AccessToken{}, err
	}
	claims, err := claimsParameter(r.Claims)
	if err != nil {
		return AccessToken{}, err
	}
	form := url.Values{
		"grant_type":          {grantTypeJWTBearer},
		"requested_token_use": {"on_behalf_of"},
		"client_id":           {o.clientID},
		"assertion":           {a.Token},
		"scope":               {strings.Join(r.Scopes, " ")},
		"claims":              {claims},
	}
	tokenURL := o.endpoint.tokenURL(a.TenantID)
	if err := o.credential.authenticate(ctx, o.clientID, tokenURL, o.endpoint.now(), form); err != nil {
		return AccessToken{}, err
	}
	return o.endpoint.post(ctx, a.TenantID, form, CorrelationID(r.ExecutionID, r.Generation))
}

var _ Exchanger = (*ClassicOBO)(nil)
