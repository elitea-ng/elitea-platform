package main

// Web push (ADR-0045 decision 6, #1437): the install's VAPID key in the
// global vault, the push_config and push_subscriptions routes, and the
// dispatcher that sends the outbox.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	notificationsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const (
	// webPushSubjectEnv is the VAPID `sub` claim: a mailto: address or an
	// https: URL that a push service can use to reach the operator. Empty:
	// PUBLIC_BASE_URL when it is https.
	webPushSubjectEnv = "ELITEA_WEB_PUSH_SUBJECT"
	publicBaseURLEnv  = "PUBLIC_BASE_URL"
)

// webPushParts is the composed web push feature.
type webPushParts struct {
	service    *webpush.Service
	repository *dbrepos.WebPushRepository
	client     *http.Client
	validate   notificationsapi.PushEndpointValidator
}

// composeWebPush builds the web push feature. It never fails the boot: a
// missing subject or vault leaves push disabled (push_config answers
// enabled false), and the log says why.
func composeWebPush(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, logger *slog.Logger) *webPushParts {
	if pool == nil {
		return nil
	}
	repository, err := dbrepos.NewWebPushRepository(pool)
	if err != nil {
		logger.Warn("web push is off: no repository", "err", err)
		return nil
	}
	subject, err := webpush.Subject(getenv(webPushSubjectEnv), getenv(publicBaseURLEnv))
	if err != nil {
		logger.Warn("web push is off: no contact for the push services", "err", err)
	}
	settings := platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.NotificationSettings, error) {
		return platformconfig.LoadNotificationSettings(ctx, pool)
	}, 0)
	keys := webpush.NewKeySource(v2secrets.NewHandler(pool), logger)
	service := webpush.NewService(keys, webpush.SettingsFunc(func(ctx context.Context) bool {
		value, err := settings.Get(ctx)
		if err != nil {
			return platformconfig.DefaultNotificationSettings().WebPush
		}
		return value.WebPush
	}), subject)
	// The key is made at first boot. A vault that fails now is tried again
	// on the first use after the retry wait.
	if _, ok := keys.Keys(ctx); ok {
		logger.Info("web push key ready", "subject_set", subject != "")
	}
	guard := egress.New(nil, egress.RequireHTTPS())
	transport := webPushTransport{
		proxy:   http.ProxyFromEnvironment,
		proxied: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: egress.MaxResponseHeaderBytes},
		guarded: guard.RoundTripper(),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		// A push service never redirects a send; a redirect is an answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	validate := func(ctx context.Context, endpoint string) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err != nil {
			return err
		}
		if proxyURL, err := transport.proxy(request); err == nil && proxyURL != nil {
			// The operator's egress proxy decides where Main may go.
			return nil
		}
		return guard.Validate(ctx, endpoint)
	}
	return &webPushParts{service: service, repository: repository, client: client, validate: validate}
}

// route builds the three web push routes.
func (p *webPushParts) route(authConfig apimw.AuthConfig, permissions auth.PermissionResolver) (*notificationsapi.PushRoute, error) {
	if p == nil {
		return nil, errors.New("web push is not composed")
	}
	return notificationsapi.NewPushRoute(p.service, p.repository, p.validate, authConfig, permissions)
}

// startDispatcher sends the outbox until ctx ends. Every replica runs one;
// each claims different deliveries (webpush.Store.ClaimDue).
func (p *webPushParts) startDispatcher(ctx context.Context, logger *slog.Logger) {
	if p == nil {
		return
	}
	dispatcher, err := webpush.NewDispatcher(p.repository, p.service, p.client, logger)
	if err != nil {
		logger.Warn("web push dispatcher is off", "err", err)
		return
	}
	go func() {
		if err := dispatcher.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("the web push dispatcher stopped", "err", err)
		}
	}()
}

// webPushTransport sends through the operator's egress proxy
// (HTTPS_PROXY / NO_PROXY) when it applies to the endpoint, and otherwise
// through the egress guard, which refuses private and reserved addresses: a
// subscription's endpoint comes from a browser, so Main must not reach an
// internal address with it.
type webPushTransport struct {
	proxy   func(*http.Request) (*url.URL, error)
	proxied http.RoundTripper
	guarded http.RoundTripper
}

func (t webPushTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if proxyURL, err := t.proxy(request); err == nil && proxyURL != nil {
		return t.proxied.RoundTrip(request)
	}
	return t.guarded.RoundTrip(request)
}
