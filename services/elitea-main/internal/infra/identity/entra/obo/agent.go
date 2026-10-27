package obo

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
)

// AgentConfig configures the Entra Agent ID on-behalf-of exchanger.
type AgentConfig struct {
	Authority string
	// BlueprintAppID is the agent identity blueprint's application id.
	BlueprintAppID string
	// AssertionAudiences are the accepted assertion `aud` values. Empty
	// means api://<BlueprintAppID>/access_agent and BlueprintAppID.
	AssertionAudiences []string
	AllowedTenants     []string
	// Assertions supplies the agent client assertion (T1). Nil is allowed:
	// every exchange is then denied with ErrAgentAssertionUnavailable.
	Assertions AgentAssertionSource
	HTTPClient *http.Client
	Now        func() time.Time
}

// AgentOBO exchanges a blueprint-audience user assertion for a resource
// token acting as one agent identity.
type AgentOBO struct {
	endpoint   endpoint
	audiences  []string
	assertions AgentAssertionSource
}

// NewAgentOBO validates config.
func NewAgentOBO(config AgentConfig) (*AgentOBO, error) {
	if !validConfigurationText(config.BlueprintAppID) {
		return nil, ErrInvalidConfiguration
	}
	audiences := config.AssertionAudiences
	if len(audiences) == 0 {
		audiences = []string{"api://" + config.BlueprintAppID + "/access_agent", config.BlueprintAppID}
	}
	if !validAudiences(audiences) {
		return nil, ErrInvalidConfiguration
	}
	endpoint, err := newEndpoint(config.Authority, config.AllowedTenants, config.HTTPClient, config.Now)
	if err != nil {
		return nil, err
	}
	return &AgentOBO{endpoint: endpoint, audiences: slices.Clone(audiences), assertions: config.Assertions}, nil
}

// Audiences are the assertion audiences this exchanger accepts.
func (o *AgentOBO) Audiences() []string { return slices.Clone(o.audiences) }

// Exchange authenticates as the agent identity with T1 and redeems the user
// assertion on its behalf.
func (o *AgentOBO) Exchange(ctx context.Context, a Assertion, r ExchangeRequest) (AccessToken, error) {
	if err := ctx.Err(); err != nil {
		return AccessToken{}, err
	}
	if o == nil {
		return AccessToken{}, ErrInvalidConfiguration
	}
	if !validConfigurationText(r.AgentID) || r.ExecutionID == "" || checkScopes(r.Scopes) != nil {
		return AccessToken{}, ErrInvalidRequest
	}
	if err := o.endpoint.checkAssertion(a, o.audiences); err != nil {
		return AccessToken{}, err
	}
	if o.assertions == nil {
		return AccessToken{}, ErrAgentAssertionUnavailable
	}
	claims, err := claimsParameter(r.Claims)
	if err != nil {
		return AccessToken{}, err
	}
	agentAssertion, err := o.assertions.AgentAssertion(ctx, a.TenantID, r.AgentID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return AccessToken{}, ctxErr
		}
		return AccessToken{}, ErrAgentAssertionUnavailable
	}
	if agentAssertion == "" || len(agentAssertion) > maxAssertionBytes ||
		strings.ContainsFunc(agentAssertion, unicode.IsControl) {
		return AccessToken{}, ErrAgentAssertionUnavailable
	}
	form := url.Values{
		"grant_type":            {grantTypeJWTBearer},
		"requested_token_use":   {"on_behalf_of"},
		"client_id":             {r.AgentID},
		"client_assertion_type": {clientAssertionTypeJWT},
		"client_assertion":      {agentAssertion},
		"assertion":             {a.Token},
		"scope":                 {strings.Join(r.Scopes, " ")},
		"claims":                {claims},
	}
	return o.endpoint.post(ctx, a.TenantID, form, CorrelationID(r.ExecutionID, r.Generation))
}

var _ Exchanger = (*AgentOBO)(nil)
