package oidc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"
	"unicode"
	"unicode/utf8"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	TokenEndpointAuthBasic TokenEndpointAuthStyle = "client_secret_basic"
	TokenEndpointAuthPost  TokenEndpointAuthStyle = "client_secret_post"

	defaultTokenResponseBytes = int64(1 << 20)
	// golang.org/x/oauth2 applies its own 1 MiB read limit. Keeping the public
	// bound at or below that value makes the configured contract truthful.
	maxTokenResponseBytes   = int64(1 << 20)
	defaultRequestTimeout   = 10 * time.Second
	minRequestTimeout       = time.Second
	maxRequestTimeout       = 30 * time.Second
	maxTokenResponseMembers = 64
)

type TokenEndpointAuthStyle string

type CodeExchangerConfig struct {
	TokenEndpoint    string
	ClientID         string
	ClientSecret     string
	RedirectURI      string
	AuthStyle        TokenEndpointAuthStyle
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

type OAuth2CodeExchanger struct {
	oauthConfig oauth2.Config
	client      *http.Client
}

// NewOAuth2CodeExchanger copies all configuration and wraps the supplied
// transport with HTTPS-only, no-redirect, timeout, status, and response-size
// bounds. The returned exchanger never exposes access or refresh tokens.
func NewOAuth2CodeExchanger(
	config CodeExchangerConfig,
	transport http.RoundTripper,
) (*OAuth2CodeExchanger, error) {
	if !validHTTPSURL(config.TokenEndpoint) || !validHTTPSURL(config.RedirectURI) ||
		!validConfigurationText(config.ClientID) || !validCredential(config.ClientSecret) {
		return nil, ErrInvalidConfiguration
	}
	if config.AuthStyle == "" {
		config.AuthStyle = TokenEndpointAuthBasic
	}
	var authStyle oauth2.AuthStyle
	switch config.AuthStyle {
	case TokenEndpointAuthBasic:
		authStyle = oauth2.AuthStyleInHeader
	case TokenEndpointAuthPost:
		authStyle = oauth2.AuthStyleInParams
	default:
		return nil, ErrInvalidConfiguration
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.RequestTimeout < minRequestTimeout || config.RequestTimeout > maxRequestTimeout {
		return nil, ErrInvalidConfiguration
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultTokenResponseBytes
	}
	if config.MaxResponseBytes < 4096 || config.MaxResponseBytes > maxTokenResponseBytes {
		return nil, ErrInvalidConfiguration
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: &boundedTokenTransport{
			base:             transport,
			maxResponseBytes: config.MaxResponseBytes,
		},
		Timeout: config.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &OAuth2CodeExchanger{
		oauthConfig: oauth2.Config{
			ClientID:     config.ClientID,
			ClientSecret: config.ClientSecret,
			RedirectURL:  config.RedirectURI,
			Endpoint: oauth2.Endpoint{
				TokenURL:  config.TokenEndpoint,
				AuthStyle: authStyle,
			},
		},
		client: client,
	}, nil
}

func (e *OAuth2CodeExchanger) Exchange(
	ctx context.Context,
	request ExchangeRequest,
) (ExchangeResult, error) {
	if err := ctx.Err(); err != nil {
		return ExchangeResult{}, err
	}
	if !validCode(request.AuthorizationCode) ||
		browserPKCEVerifierInvalid(request.PKCEVerifier) {
		return ExchangeResult{}, ErrAssertionRejected
	}
	ctx = coreoidc.ClientContext(ctx, e.client)
	token, err := e.oauthConfig.Exchange(
		ctx,
		request.AuthorizationCode,
		oauth2.VerifierOption(request.PKCEVerifier),
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ExchangeResult{}, contextErr
		}
		if retrieveErrorIsExplicitRejection(err) {
			return ExchangeResult{}, ErrAssertionRejected
		}
		return ExchangeResult{}, ErrProviderUnavailable
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" || len(rawIDToken) > maxRawIDTokenBytes {
		return ExchangeResult{}, ErrProviderUnavailable
	}
	return ExchangeResult{RawIDToken: rawIDToken}, nil
}

type boundedTokenTransport struct {
	base             http.RoundTripper
	maxResponseBytes int64
}

func (t *boundedTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.Method != http.MethodPost || request.URL == nil ||
		!validHTTPSURL(request.URL.String()) {
		return nil, ErrProviderUnavailable
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, fmt.Errorf("%w: token transport", ErrProviderUnavailable)
	}
	if response == nil || response.Body == nil {
		return nil, ErrProviderUnavailable
	}
	if tokenStatusIsDependency(response.StatusCode) {
		_ = response.Body.Close()
		return nil, ErrProviderUnavailable
	}
	contentTypes := response.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		_ = response.Body.Close()
		return nil, ErrProviderUnavailable
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" {
		_ = response.Body.Close()
		return nil, ErrProviderUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, t.maxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) > t.maxResponseBytes {
		return nil, ErrProviderUnavailable
	}
	if _, ok := uniqueJSONObject(body, maxTokenResponseMembers); !ok {
		return nil, ErrProviderUnavailable
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return response, nil
}

func tokenStatusIsDependency(status int) bool {
	return status < http.StatusOK || status >= http.StatusInternalServerError ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		(status >= http.StatusMultipleChoices && status < http.StatusBadRequest)
}

func retrieveErrorIsExplicitRejection(err error) bool {
	var retrieveError *oauth2.RetrieveError
	if !errors.As(err, &retrieveError) || retrieveError.Response == nil ||
		retrieveError.Response.StatusCode != http.StatusBadRequest {
		return false
	}
	return retrieveError.ErrorCode == "invalid_grant" || retrieveError.ErrorCode == "access_denied"
}

func validCredential(value string) bool {
	return len(value) > 0 && len(value) <= maxConfigurationString && utf8.ValidString(value) &&
		!containsControl(value)
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func browserPKCEVerifierInvalid(verifier string) bool {
	return browserflow.ProviderState{PKCEVerifier: verifier}.Validate() != nil || verifier == ""
}

var _ CodeExchanger = (*OAuth2CodeExchanger)(nil)
