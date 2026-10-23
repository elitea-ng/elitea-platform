package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/admission"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/registrar"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

// providerRegistrationInterval is how often a facade re-probes its
// provider's descriptor and health for the admission plane.
const providerRegistrationInterval = time.Minute

// startProviderRegistrar makes a composed facade's provider known to the
// admission plane (ADR-0023 decision 6): its descriptor is registered under
// the public project at boot and its health projection kept current. It
// returns nil, with the reason logged, when there is nothing to register
// with — no database, no public project, or a database the admission
// migration (0107) has not reached — so a deployment without the plane
// keeps serving the provider exactly as before. provider is the facade's own
// client to the provider (proxy.Proxy.Client): the registrar dials nothing.
//
// IT RETURNS THE REGISTRAR, not a bool, because the registrar is the only
// thing that knows WHAT THE PROVIDER IS CALLED. The provider id is the name
// in the provider's own descriptor ("wikis", not "deepwiki"), read over the
// wire after composition has finished, so the admission gate cannot be given
// a literal here — it has to ask, per request, through ProviderID().
func startProviderRegistrar(
	ctx context.Context,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	publicProjectID int32,
	name string,
	cfg facade.Config,
	envName string,
	provider subappv1.ProviderServiceClient,
) *registrar.Registrar {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("provider", name)
	switch {
	case pool == nil:
		log.Info("provider registration skipped: no database")
		return nil
	case publicProjectID <= 0:
		log.Info("provider registration skipped: no public project (ELITEA_AI_PROJECT_ID)")
		return nil
	case !providerhub.Present(ctx, pool):
		log.Info("provider registration skipped: the admission plane is not migrated (0107)")
		return nil
	}
	reg, err := registrar.New(cfg, envName, registrar.PoolStore{Pool: pool}, registrar.Options{
		ProjectID: int64(publicProjectID),
		Actor:     "facade:" + name,
		Interval:  providerRegistrationInterval,
		Provider:  provider,
	}, log)
	if err != nil {
		log.Error("provider registration not started", "error", err)
		return nil
	}
	go reg.Run(ctx)
	log.Info("provider registration started", "origin", reg.Origin(), "project", publicProjectID,
		"interval", providerRegistrationInterval.String())
	return reg
}

// providerAdmissionGate builds the gate the facade's invoke route asks.
//
// A nil registrar means the plane is absent for this deployment, and the
// gate built from it allows everything — the E2E stack composes no public
// project, so this is the path that keeps its journeys green rather than a
// theoretical branch.
func providerAdmissionGate(
	logger *slog.Logger,
	pool *pgxpool.Pool,
	publicProjectID int32,
	posture facade.AdmissionPosture,
	reg *registrar.Registrar,
) facade.AdmissionHook {
	if reg == nil {
		return nil
	}
	return admission.New(admission.Config{
		Store:      admission.PoolStore{Pool: pool},
		ProjectID:  int64(publicProjectID),
		ProviderID: reg.ProviderID,
		Posture:    posture,
		Logger:     logger,
	}).Hook()
}
