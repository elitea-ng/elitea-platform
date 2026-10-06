// Package registrar makes a facade's provider known to the admission plane
// at boot (ADR-0023 decision 6) and keeps its health projection current.
//
// The provider holds no platform credential (ADR-0022 decision 6), so it is
// the FACADE — which already reaches the provider over mTLS and already
// holds the database — that registers on its behalf: it reads the
// provider's own descriptor, records origin + manifest + an inactive
// revision under the public project, then probes /health on an interval
// and writes what it saw. A provider that is down at boot is retried on the
// same interval; a provider that answers a different descriptor later is
// re-registered (one more manifest row, one more revision — the plane is
// append-only by design).
package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

// Store is what the registrar writes to: the admission plane.
type Store interface {
	Register(ctx context.Context, in providerhub.Registration) (providerhub.Admitted, error)
	RecordHealth(ctx context.Context, projectID int64, providerID string, healthy bool, detail string) error
}

// Options shape one registrar.
type Options struct {
	// ProjectID is the project the registration is filed under — the public
	// project, so every project's catalogue can see it (the register-once
	// shape global LLM providers use).
	ProjectID int64
	// Actor is recorded as registered_by / admitted_by.
	Actor string
	// Interval between probes; the first runs at Start.
	Interval time.Duration
	// Timeout for one probe request.
	Timeout time.Duration
	// Client overrides the mTLS client built from the facade config (tests).
	Client *http.Client
	// RequireTLS is the hop's rule; tests over plain httptest turn it off.
	RequireTLS *bool
}

// Registrar is one facade's registrar.
type Registrar struct {
	target     *url.URL
	client     *http.Client
	secret     []byte
	store      Store
	opts       Options
	logger     *slog.Logger
	providerID string
	digest     string
}

const maxDescriptorBytes = 1 << 20

// New builds a registrar for the provider behind cfg. envName names the
// setting in error messages, as the proxy does.
func New(cfg facade.Config, envName string, store Store, opts Options, logger *slog.Logger) (*Registrar, error) {
	if store == nil {
		return nil, errors.New("registrar: a store is required")
	}
	if opts.ProjectID <= 0 {
		return nil, errors.New("registrar: a positive project id is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	requireTLS := true
	if opts.RequireTLS != nil {
		requireTLS = *opts.RequireTLS
	}
	target, err := hop.ParseTarget(cfg.BaseURL, hop.TargetOptions{EnvName: envName, RequireTLS: requireTLS})
	if err != nil {
		return nil, err
	}
	client := opts.Client
	if client == nil {
		serverName := cfg.ServerName
		if serverName == "" {
			serverName = target.Hostname()
		}
		transport, err := llmproxy.NewMTLSTransport(cfg.ClientCertFile, cfg.ClientKeyFile, cfg.CAFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("registrar: build provider mTLS transport: %w", err)
		}
		client = &http.Client{Transport: transport}
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Actor == "" {
		opts.Actor = "facade"
	}
	return &Registrar{target: target, client: client, secret: []byte(cfg.IdentitySecret), store: store, opts: opts, logger: logger}, nil
}

// ProviderID is the provider's own name, once its descriptor has been read.
func (r *Registrar) ProviderID() string { return r.providerID }

// Origin is the provider's service location as registered: the target's
// scheme, host and port, no path.
func (r *Registrar) Origin() string {
	return r.target.Scheme + "://" + r.target.Host
}

// Once reads the descriptor, registers it when it is new or changed, and
// records one health observation. Errors are returned for the caller to
// log; the next tick tries again.
func (r *Registrar) Once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	if err := r.register(ctx); err != nil {
		return err
	}
	healthy, detail := r.probe(ctx)
	if err := r.store.RecordHealth(ctx, r.opts.ProjectID, r.providerID, healthy, detail); err != nil {
		return err
	}
	return nil
}

// registrarRetryFirst is the first pause after a failed Once. Failures back
// off by doubling up to Interval, so a provider that starts after
// elitea-main (the usual order in a fresh install) registers within seconds
// rather than a full Interval later, and an absent one is not hammered.
const registrarRetryFirst = 2 * time.Second

// Run performs Once now, then again every Interval while it succeeds and
// sooner — 2s, doubling, capped at Interval — while it fails.
func (r *Registrar) Run(ctx context.Context) {
	retry := time.Duration(0)
	for {
		next := r.opts.Interval
		if err := r.Once(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logger.Warn("provider registration probe failed", "origin", r.Origin(), "error", err)
			retry = nextRegistrarRetry(retry, r.opts.Interval)
			next = retry
		} else {
			retry = 0
		}
		timer := time.NewTimer(next)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// nextRegistrarRetry doubles the previous retry pause (starting at
// registrarRetryFirst) and caps it at interval.
func nextRegistrarRetry(previous, interval time.Duration) time.Duration {
	next := registrarRetryFirst
	if previous > 0 {
		next = previous * 2
	}
	return min(next, interval)
}

func (r *Registrar) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.target.ResolveReference(&url.URL{Path: path}).String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	// The provider's identity gate admits only the probes unsigned; the
	// descriptor is read as the facade itself, under the registration's
	// project, with the same signer the proxied calls use.
	if len(r.secret) > 0 {
		llmproxy.SignIdentityHeaders(request.Header, r.secret, strconv.FormatInt(r.opts.ProjectID, 10), "0", "", "")
	}
	return r.client.Do(request)
}

// register reads /descriptor and files it when its bytes differ from the
// last registration this process made.
func (r *Registrar) register(ctx context.Context) error {
	response, err := r.get(ctx, "/descriptor")
	if err != nil {
		return fmt.Errorf("read the provider descriptor: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("read the provider descriptor: HTTP %d", response.StatusCode)
	}
	manifest, err := io.ReadAll(io.LimitReader(response.Body, maxDescriptorBytes+1))
	if err != nil {
		return fmt.Errorf("read the provider descriptor: %w", err)
	}
	if len(manifest) > maxDescriptorBytes {
		return fmt.Errorf("the provider descriptor exceeds the %d byte limit", maxDescriptorBytes)
	}
	var named struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(manifest, &named); err != nil || strings.TrimSpace(named.Name) == "" {
		return errors.New("the provider descriptor carries no name; nothing to register it as")
	}
	digest := providerhub.Digest(manifest)
	if r.providerID == named.Name && r.digest == digest {
		return nil
	}
	admitted, err := r.store.Register(ctx, providerhub.Registration{
		ProjectID:  r.opts.ProjectID,
		ProviderID: named.Name,
		Origin:     r.Origin(),
		Manifest:   manifest,
		Actor:      r.opts.Actor,
	})
	if err != nil {
		return fmt.Errorf("register the provider descriptor: %w", err)
	}
	r.providerID, r.digest = named.Name, digest
	r.logger.Info("provider descriptor registered", "provider_id", named.Name, "origin", r.Origin(),
		"project", r.opts.ProjectID, "revision", admitted.RevisionID, "status", admitted.Status)
	return nil
}

// probe reads /health: healthy when it answers 200 with status UP; the
// detail keeps what was seen either way.
func (r *Registrar) probe(ctx context.Context) (bool, string) {
	started := time.Now()
	response, err := r.get(ctx, "/health")
	if err != nil {
		return false, "health probe failed: " + err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	elapsed := time.Since(started).Round(time.Millisecond)
	var health struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &health)
	if response.StatusCode != http.StatusOK || !strings.EqualFold(health.Status, "UP") {
		return false, fmt.Sprintf("health answered HTTP %d status=%q in %s", response.StatusCode, health.Status, elapsed)
	}
	return true, fmt.Sprintf("health UP in %s", elapsed)
}

// PoolStore is the Store over the admission plane's tables.
type PoolStore struct{ Pool *pgxpool.Pool }

// Register files a registration.
func (s PoolStore) Register(ctx context.Context, in providerhub.Registration) (providerhub.Admitted, error) {
	return providerhub.Register(ctx, s.Pool, in)
}

// RecordHealth writes one observation.
func (s PoolStore) RecordHealth(ctx context.Context, projectID int64, providerID string, healthy bool, detail string) error {
	return providerhub.RecordHealth(ctx, s.Pool, projectID, providerID, healthy, detail)
}
