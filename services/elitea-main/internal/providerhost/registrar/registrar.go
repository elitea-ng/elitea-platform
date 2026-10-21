// Package registrar makes a facade's provider known to the admission plane
// at boot (ADR-0023 decision 6) and keeps its health projection current.
//
// The provider holds no platform credential (ADR-0022 decision 6), so it is
// the FACADE — which already reaches the provider over mTLS and already
// holds the database — that registers on its behalf: it reads the
// provider's own descriptor (ProviderService.Describe, #1359), records
// origin + manifest + an inactive revision under the public project, then
// asks Health on an interval and writes what it saw. A provider that is down at boot is retried on the
// same interval; a provider that answers a different descriptor later is
// re-registered (one more manifest row, one more revision — the plane is
// append-only by design).
package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/status"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/hop"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
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
	// Provider overrides the mTLS client built from the facade config (tests).
	Provider subappv1.ProviderServiceClient
}

// Registrar is one facade's registrar.
type Registrar struct {
	target     string
	provider   subappv1.ProviderServiceClient
	secret     []byte
	store      Store
	opts       Options
	logger     *slog.Logger
	providerID string
	digest     string
}

const maxDescriptorBytes = 1 << 20

// New builds a registrar for the provider behind cfg. envName names the
// setting in error messages, as the proxy does. It shares nothing with the
// proxy but the material: one more connection to the same listener.
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
	if err := hop.ParseGRPCTarget(cfg.Target, envName); err != nil {
		return nil, err
	}
	provider := opts.Provider
	if provider == nil {
		hopProxy, err := proxy.New(cfg, envName, logger)
		if err != nil {
			return nil, fmt.Errorf("registrar: %w", err)
		}
		provider = hopProxy.Client()
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
	return &Registrar{target: cfg.Target, provider: provider, secret: []byte(cfg.IdentitySecret), store: store, opts: opts, logger: logger}, nil
}

// ProviderID is the provider's own name, once its descriptor has been read.
func (r *Registrar) ProviderID() string { return r.providerID }

// Origin is the provider's service location as registered: its gRPC
// listener, as grpcs://host:port.
func (r *Registrar) Origin() string {
	return "grpcs://" + r.target
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

// signed is ctx carrying the facade's own identity: the descriptor is read
// as the facade, under the registration's project, with the signer the
// proxied calls use. The health probe is admitted unsigned and carries it
// anyway.
func (r *Registrar) signed(ctx context.Context) context.Context {
	return proxy.SignedContext(ctx, r.secret, strconv.FormatInt(r.opts.ProjectID, 10), "0", "")
}

// register reads the descriptor and files it when its bytes differ from the
// last registration this process made.
func (r *Registrar) register(ctx context.Context) error {
	response, err := r.provider.Describe(r.signed(ctx), &subappv1.DescribeRequest{})
	if err != nil {
		return fmt.Errorf("read the provider descriptor: %w", err)
	}
	manifest := response.GetDescriptorJson()
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

// probe asks Health: healthy when it answers with status UP; the detail
// keeps what was seen either way.
func (r *Registrar) probe(ctx context.Context) (bool, string) {
	started := time.Now()
	response, err := r.provider.Health(r.signed(ctx), &subappv1.HealthRequest{})
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		return false, fmt.Sprintf("health probe failed (%s): %s", status.Code(err), status.Convert(err).Message())
	}
	if !strings.EqualFold(response.GetStatus(), "UP") {
		return false, fmt.Sprintf("health answered status=%q in %s", response.GetStatus(), elapsed)
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
