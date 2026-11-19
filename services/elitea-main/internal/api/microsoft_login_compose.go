package api

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	v2configs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2microsoftlogin "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/microsoftlogin"
	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

// microsoftLoginService composes the Microsoft sign-in (ADR-0057 decision 6): the
// start, the callback and the handoff, for Custom app and Default app.
//
// It needs the database, the vault master key (the verifier and the access token of
// a pending handoff are sealed under a key derived from it), a public origin Entra
// accepts as a redirect URI (https, or localhost) and the credential reader. Without
// any of them the service is nil: the routes answer 503, and nothing falls back to a
// sign-in that skips a rule.
func microsoftLoginService(cfg RouterConfig, configurations *v2configs.Handler, resolver *microsoftapps.Resolver) *microsoftlogin.Service {
	if cfg.Pool == nil || configurations == nil {
		return nil
	}
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil || len(key) == 0 {
		return nil
	}
	defer clear(key)
	guard := mcpAuthorizationEgressGuard(cfg)
	client := &http.Client{
		Transport: guard.Transport(),
		// Microsoft's endpoints never redirect; following one would send a client
		// secret or a token to a host the guard was never asked about.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       20 * time.Second,
	}
	config := microsoftlogin.Config{
		Pool: cfg.Pool, MasterKey: key, Credentials: configurations.MicrosoftLoginCredentials(), Origin: cfg.PublicOrigin,
		Do: func(request *http.Request) (*http.Response, error) {
			if err := guard.Validate(request.Context(), request.URL.String()); err != nil {
				return nil, err
			}
			return client.Do(request)
		},
	}
	if resolver != nil {
		// Assigned only when composed: a nil *Resolver boxed in the interface would
		// pass the nil test and fail on first use.
		config.Apps = resolver
	}
	service, err := microsoftlogin.New(config)
	if err != nil {
		slog.Warn("microsoft login: not composed", "reason", err.Error())
		return nil
	}
	return service
}

// microsoftLoginHandler is the HTTP half. A nil service gives a handler that answers
// 503 everywhere.
func microsoftLoginHandler(cfg RouterConfig, service *microsoftlogin.Service) *v2microsoftlogin.Handler {
	var flow v2microsoftlogin.Flow
	if service != nil {
		flow = service
	}
	return v2microsoftlogin.New(flow, cfg.PublicOrigin, slog.Default())
}
