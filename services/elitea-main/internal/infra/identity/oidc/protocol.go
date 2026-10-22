// Package oidc implements the OpenID Connect authorization-code boundary used
// by browser authentication. It rejects the current implicit/direct-id-token
// compatibility mode. Discovery reload, resolved configuration composition,
// and federated logout remain explicit production-mount gates.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

const (
	ProviderName           = "oidc"
	randomValueBytes       = 32
	maxAuthorizationCode   = 4096
	maxRawIDTokenBytes     = 128 << 10
	maxConfigurationString = 4096
	maxScopes              = 16
	maxScopeBytes          = 256
	maxSigningAlgorithms   = 8
	maxEndpointParameters  = 16
	maxExpirationOverride  = 30 * 24 * time.Hour
	maxFutureIssuedAtSkew  = 5 * time.Minute
)

var (
	ErrInvalidConfiguration = errors.New("invalid OIDC protocol configuration")
	ErrAssertionRejected    = errors.New("OIDC assertion rejected")
	// ErrProviderUnavailable is implemented by code-exchange and key-set
	// adapters when their remote dependency cannot be reached or returns an
	// unusable service response. It must not classify OAuth/OIDC rejection.
	ErrProviderUnavailable = errors.New("OIDC provider unavailable")
)

var allowedSigningAlgorithms = map[string]struct{}{
	"RS256": {}, "RS384": {}, "RS512": {},
	"PS256": {}, "PS384": {}, "PS512": {},
	"ES256": {}, "ES384": {}, "ES512": {},
	"EdDSA": {},
}

type Config struct {
	Issuer                     string
	AuthorizationEndpoint      string
	LoginMode                  browserapp.OIDCAuthorizationTransport
	ClientID                   string
	RedirectURI                string
	Scopes                     []string
	SupportedSigningAlgorithms []string
	RequireEmailVerified       bool
	ExpirationOverride         time.Duration
}

type ExchangeRequest struct {
	AuthorizationCode string
	PKCEVerifier      string
}

type ExchangeResult struct {
	RawIDToken string
}

// CodeExchanger owns the token endpoint, client credential, TLS, timeout, and
// response-size policy. It returns ErrProviderUnavailable only for dependency
// failures; rejected or malformed authorization grants return another error.
type CodeExchanger interface {
	Exchange(context.Context, ExchangeRequest) (ExchangeResult, error)
}

type Protocol struct {
	oauthConfig            oauth2.Config
	authorizationTransport browserapp.OIDCAuthorizationTransport
	issuer                 string
	signingAlgorithms      []string
	requireEmailVerified   bool
	expirationOverride     time.Duration
	exchanger              CodeExchanger
	keySet                 coreoidc.KeySet
	random                 io.Reader
	now                    func() time.Time
	randomMu               sync.Mutex
}

func NewProtocol(config Config, exchanger CodeExchanger, keySet coreoidc.KeySet) (*Protocol, error) {
	return newProtocol(config, exchanger, keySet, rand.Reader, time.Now)
}

func newProtocol(
	config Config,
	exchanger CodeExchanger,
	keySet coreoidc.KeySet,
	randomReader io.Reader,
	now func() time.Time,
) (*Protocol, error) {
	transport, ok := normalizedAuthorizationTransport(config.LoginMode)
	if exchanger == nil || keySet == nil || randomReader == nil || now == nil ||
		!ok || !validIssuer(config.Issuer) || !validAuthorizationEndpoint(config.AuthorizationEndpoint) ||
		!validHTTPSURL(config.RedirectURI) || !validConfigurationText(config.ClientID) ||
		config.ExpirationOverride < 0 || config.ExpirationOverride > maxExpirationOverride ||
		(config.ExpirationOverride > 0 && config.ExpirationOverride < time.Second) {
		return nil, ErrInvalidConfiguration
	}
	scopes, ok := normalizedScopes(config.Scopes)
	if !ok {
		return nil, ErrInvalidConfiguration
	}
	algorithms, ok := normalizedSigningAlgorithms(config.SupportedSigningAlgorithms)
	if !ok {
		return nil, ErrInvalidConfiguration
	}
	return &Protocol{
		oauthConfig: oauth2.Config{
			ClientID:    config.ClientID,
			RedirectURL: config.RedirectURI,
			Scopes:      scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL: config.AuthorizationEndpoint,
			},
		},
		authorizationTransport: transport,
		issuer:                 config.Issuer,
		signingAlgorithms:      algorithms,
		requireEmailVerified:   config.RequireEmailVerified,
		expirationOverride:     config.ExpirationOverride,
		exchanger:              exchanger,
		keySet:                 keySet,
		random:                 randomReader,
		now:                    now,
	}, nil
}

func (p *Protocol) NewAuthorization(ctx context.Context) (browserapp.OIDCAuthorization, error) {
	if err := ctx.Err(); err != nil {
		return browserapp.OIDCAuthorization{}, err
	}
	nonce, err := p.randomValue()
	if err != nil {
		return browserapp.OIDCAuthorization{}, fmt.Errorf("generate OIDC nonce: %w", err)
	}
	verifier, err := p.randomValue()
	if err != nil {
		return browserapp.OIDCAuthorization{}, fmt.Errorf("generate OIDC PKCE verifier: %w", err)
	}
	return browserapp.OIDCAuthorization{
		Correlation:         browserflow.ProtocolCorrelation{Nonce: nonce},
		ProviderState:       browserflow.ProviderState{PKCEVerifier: verifier},
		PKCEChallengeMethod: browserapp.OIDCPKCEChallengeS256,
	}, nil
}

func (p *Protocol) AuthorizationRequest(
	state string,
	authorization browserapp.OIDCAuthorization,
) (browserapp.OIDCAuthorizationRequest, error) {
	if browserflow.ValidateTransactionID(state) != nil ||
		!validAuthorization(authorization) {
		return browserapp.OIDCAuthorizationRequest{}, ErrAssertionRejected
	}
	challengeBytes := sha256.Sum256([]byte(authorization.ProviderState.PKCEVerifier))
	return browserapp.OIDCAuthorizationRequest{
		Transport:           p.authorizationTransport,
		Endpoint:            p.oauthConfig.Endpoint.AuthURL,
		ResponseType:        "code",
		ClientID:            p.oauthConfig.ClientID,
		RedirectURI:         p.oauthConfig.RedirectURL,
		Scope:               strings.Join(p.oauthConfig.Scopes, " "),
		State:               state,
		Nonce:               authorization.Correlation.Nonce,
		CodeChallenge:       base64.RawURLEncoding.EncodeToString(challengeBytes[:]),
		CodeChallengeMethod: browserapp.OIDCPKCEChallengeS256,
	}, nil
}

func (p *Protocol) NewVerifier(code string) browserapp.AssertionVerifier {
	return &assertionVerifier{protocol: p, code: code}
}

func (p *Protocol) randomValue() (string, error) {
	randomBytes := make([]byte, randomValueBytes)
	p.randomMu.Lock()
	_, err := io.ReadFull(p.random, randomBytes)
	p.randomMu.Unlock()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

type assertionVerifier struct {
	protocol *Protocol
	code     string
}

func (v *assertionVerifier) Verify(
	ctx context.Context,
	verification browserflow.VerificationContext,
) (browserflow.VerifiedAssertion, error) {
	if err := ctx.Err(); err != nil {
		return browserflow.VerifiedAssertion{}, err
	}
	if verification.Provider != ProviderName || !validAuthorization(browserapp.OIDCAuthorization{
		Correlation:         verification.Correlation,
		ProviderState:       verification.ProviderState,
		PKCEChallengeMethod: browserapp.OIDCPKCEChallengeS256,
	}) || !validCode(v.code) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	exchanged, err := v.protocol.exchanger.Exchange(ctx, ExchangeRequest{
		AuthorizationCode: v.code,
		PKCEVerifier:      verification.ProviderState.PKCEVerifier,
	})
	if err != nil {
		return browserflow.VerifiedAssertion{}, classifyVerificationError(ctx, err)
	}
	if len(exchanged.RawIDToken) == 0 || len(exchanged.RawIDToken) > maxRawIDTokenBytes ||
		!utf8.ValidString(exchanged.RawIDToken) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}

	trackedKeys := &availabilityKeySet{delegate: v.protocol.keySet}
	verifier := coreoidc.NewVerifier(v.protocol.issuer, trackedKeys, &coreoidc.Config{
		ClientID:             v.protocol.oauthConfig.ClientID,
		SupportedSigningAlgs: append([]string(nil), v.protocol.signingAlgorithms...),
		Now:                  v.protocol.now,
	})
	verifiedToken, err := verifier.Verify(ctx, exchanged.RawIDToken)
	if err != nil {
		if trackedKeys.unavailable {
			return browserflow.VerifiedAssertion{}, verifierUnavailable(ctx)
		}
		return browserflow.VerifiedAssertion{}, classifyRejectedError(ctx, err)
	}
	if !constantTimeEqual(verifiedToken.Nonce, verification.Correlation.Nonce) ||
		!validProviderReference(verifiedToken.Subject) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	verificationTime := v.protocol.now().UTC()
	if verifiedToken.IssuedAt.IsZero() ||
		verifiedToken.IssuedAt.After(verificationTime.Add(maxFutureIssuedAtSkew)) ||
		verifiedToken.IssuedAt.After(verifiedToken.Expiry) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}

	var claims identityClaims
	if err := verifiedToken.Claims(&claims); err != nil {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	if v.protocol.requireEmailVerified && (claims.EmailVerified == nil || !*claims.EmailVerified) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	if (len(verifiedToken.Audience) > 1 && claims.AuthorizedParty == "") ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != v.protocol.oauthConfig.ClientID) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	providerReference := claims.PreferredUsername
	if providerReference == "" {
		providerReference = verifiedToken.Subject
	}
	if !validProviderReference(providerReference) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	var rawClaims json.RawMessage
	if err := verifiedToken.Claims(&rawClaims); err != nil {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	// Full bounded verified claims are deliberately retained for current
	// mapper, picture, and social consumers. Access/refresh tokens and the raw
	// ID token are never retained; claim minimization is a later contract change.
	providerAttributes, err := json.Marshal(providerAttributeEnvelope{
		NameID:       providerReference,
		Attributes:   rawClaims,
		SessionIndex: "",
	})
	if err != nil || !validProviderAttributes(providerAttributes) {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	expiresAt := verifiedToken.Expiry.UTC()
	if v.protocol.expirationOverride > 0 {
		// The legacy auth plugin can extend beyond signed exp. This correction
		// allows only a shorter local session and never weakens the signed
		// lifetime.
		override := verificationTime.Add(v.protocol.expirationOverride)
		if override.Before(expiresAt) {
			expiresAt = override
		}
	}
	assertion := browserflow.VerifiedAssertion{
		Provider:            ProviderName,
		ProviderReference:   providerReference,
		Email:               claims.Email,
		GivenName:           claims.GivenName,
		FamilyName:          claims.FamilyName,
		Name:                claims.Name,
		ProviderAttributes:  providerAttributes,
		Expiration:          &expiresAt,
		ProtocolCorrelation: verification.Correlation,
	}
	if assertion.Validate() != nil {
		return browserflow.VerifiedAssertion{}, ErrAssertionRejected
	}
	return assertion, nil
}

type identityClaims struct {
	PreferredUsername string `json:"preferred_username"`
	AuthorizedParty   string `json:"azp"`
	Email             string `json:"email"`
	EmailVerified     *bool  `json:"email_verified"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
	Name              string `json:"name"`
}

type providerAttributeEnvelope struct {
	NameID       string          `json:"nameid"`
	Attributes   json.RawMessage `json:"attributes"`
	SessionIndex string          `json:"sessionindex"`
}

type availabilityKeySet struct {
	delegate    coreoidc.KeySet
	unavailable bool
}

func (k *availabilityKeySet) VerifySignature(ctx context.Context, rawIDToken string) ([]byte, error) {
	payload, err := k.delegate.VerifySignature(ctx, rawIDToken)
	if err != nil && isProviderUnavailable(err) {
		k.unavailable = true
	}
	return payload, err
}

func validAuthorization(authorization browserapp.OIDCAuthorization) bool {
	return authorization.PKCEChallengeMethod == browserapp.OIDCPKCEChallengeS256 &&
		authorization.Correlation.Nonce != "" && authorization.Correlation.RequestID == "" &&
		authorization.Correlation.Validate() == nil && authorization.ProviderState.PKCEVerifier != "" &&
		authorization.ProviderState.Validate() == nil
}

func validCode(code string) bool {
	return len(code) > 0 && len(code) <= maxAuthorizationCode && utf8.ValidString(code) &&
		!strings.ContainsFunc(code, unicode.IsControl)
}

func validProviderReference(value string) bool {
	return len(value) > 0 && len(value) <= browserflow.MaxProviderReferenceBytes &&
		utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func validProviderAttributes(raw json.RawMessage) bool {
	provider := ProviderName
	state := sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Provider:           &provider,
		ProviderAttributes: raw,
	}
	return state.Validate() == nil
}

func normalizedScopes(configured []string) ([]string, bool) {
	if len(configured) == 0 {
		return []string{"openid", "profile", "email"}, true
	}
	if len(configured) > maxScopes {
		return nil, false
	}
	result := append([]string(nil), configured...)
	seen := make(map[string]struct{}, len(result))
	for _, scope := range result {
		if len(scope) == 0 || len(scope) > maxScopeBytes || !utf8.ValidString(scope) ||
			strings.ContainsFunc(scope, unicode.IsSpace) || strings.ContainsFunc(scope, unicode.IsControl) {
			return nil, false
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, false
		}
		seen[scope] = struct{}{}
	}
	_, hasOpenID := seen["openid"]
	return result, hasOpenID
}

func normalizedSigningAlgorithms(configured []string) ([]string, bool) {
	if len(configured) == 0 {
		return []string{"RS256"}, true
	}
	if len(configured) > maxSigningAlgorithms {
		return nil, false
	}
	result := append([]string(nil), configured...)
	seen := make(map[string]struct{}, len(result))
	for _, algorithm := range result {
		if _, allowed := allowedSigningAlgorithms[algorithm]; !allowed {
			return nil, false
		}
		if _, duplicate := seen[algorithm]; duplicate {
			return nil, false
		}
		seen[algorithm] = struct{}{}
	}
	return result, true
}

func validIssuer(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && validHTTPSURL(raw) && parsed.RawQuery == ""
}

func normalizedAuthorizationTransport(
	configured browserapp.OIDCAuthorizationTransport,
) (browserapp.OIDCAuthorizationTransport, bool) {
	switch configured {
	case "", browserapp.OIDCAuthorizationPOST:
		return browserapp.OIDCAuthorizationPOST, true
	case browserapp.OIDCAuthorizationGET:
		return browserapp.OIDCAuthorizationGET, true
	default:
		return "", false
	}
}

func validAuthorizationEndpoint(raw string) bool {
	if !validHTTPSURL(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !headerSafeAuthorizationHost(parsed.Host) {
		return false
	}
	parameters, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return false
	}
	count := 0
	for key, values := range parameters {
		if reservedAuthorizationParameter(key) || forbiddenAuthorizationEndpointParameter(key) ||
			!validConfigurationText(key) || len(values) != 1 {
			return false
		}
		count += len(values)
		for _, value := range values {
			if len(value) > maxConfigurationString || !utf8.ValidString(value) ||
				strings.ContainsFunc(value, unicode.IsControl) {
				return false
			}
		}
	}
	return count <= maxEndpointParameters
}

func forbiddenAuthorizationEndpointParameter(name string) bool {
	switch strings.ToLower(name) {
	case "client_secret", "code_verifier", "access_token", "refresh_token", "id_token":
		return true
	default:
		return false
	}
}

func headerSafeAuthorizationHost(host string) bool {
	if host == "" {
		return false
	}
	for index := 0; index < len(host); index++ {
		if host[index] <= 0x20 || host[index] >= 0x7f || strings.ContainsRune(";,'\"", rune(host[index])) {
			return false
		}
	}
	return true
}

func reservedAuthorizationParameter(name string) bool {
	switch name {
	case "response_type", "client_id", "redirect_uri", "scope", "state", "nonce",
		"code_challenge", "code_challenge_method":
		return true
	default:
		return false
	}
}

func validHTTPSURL(raw string) bool {
	if len(raw) == 0 || len(raw) > maxConfigurationString || !utf8.ValidString(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Opaque == "" && parsed.Fragment == ""
}

func validConfigurationText(value string) bool {
	return len(value) > 0 && len(value) <= maxConfigurationString && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func constantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func classifyVerificationError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if isProviderUnavailable(err) {
		return verifierUnavailable(ctx)
	}
	return ErrAssertionRejected
}

func classifyRejectedError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrAssertionRejected
}

func verifierUnavailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %w", browserapp.ErrAssertionVerifierUnavailable, ErrProviderUnavailable)
}

func isProviderUnavailable(err error) bool {
	if errors.Is(err, ErrProviderUnavailable) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

var _ browserapp.OIDCProtocol = (*Protocol)(nil)
