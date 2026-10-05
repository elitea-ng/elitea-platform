package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicorigin"
)

// nativeAuthComposition is the native authorization server's boot-time state
// (ADR-0025 WP2). Every field is nil without a pool.
type nativeAuthComposition struct {
	registry  *nativeauth.Registry
	store     *nativeauth.Store
	validator *nativeauth.AccessValidator
}

// apiTokens is the validator for the API group as a nil INTERFACE when absent:
// a typed nil pointer boxed into apimw.TokenValidator would read as configured
// (#86).
func (n nativeAuthComposition) apiTokens() apimw.TokenValidator {
	if n.validator == nil {
		return nil
	}
	return n.validator
}

// graphTokens is the same validator for the Form graph's gateway edge.
func (n nativeAuthComposition) graphTokens() authsvc.TokenValidator {
	if n.validator == nil {
		return nil
	}
	return n.validator
}

// policy is the ONE cached native client policy (ADR-0025 WP4) the router
// shares between discovery, the token response, the 426 gate and the admin
// save that invalidates it. Built here, once, like the brand resolver. The
// registry is passed as a nil INTERFACE when absent (#86).
func (n nativeAuthComposition) policy(pool *pgxpool.Pool) *nativepolicy.Service {
	var clients nativepolicy.Clients
	if n.registry != nil {
		clients = n.registry
	}
	return nativepolicy.New(pool, clients)
}

// errNativeNeedsPublicOrigin is coordinator decision 9: once any native client
// is registered, the issuer must be the configured public origin, never one
// derived from a request.
var errNativeNeedsPublicOrigin = errors.New(
	"a native client is registered but " + publicorigin.Env + " is not set: " +
		"set it to this deployment's public origin (the issuer every native sign-in carries)")

// nativeAuthFromEnv reads the token lifetimes (ELITEA_NATIVE_*), the file layer
// (NATIVE_CLIENTS_PATH) and, when a client is registered in either layer,
// requires DEPLOYMENT_URL. Any unparsable value refuses boot and names its
// variable.
func nativeAuthFromEnv(
	ctx context.Context,
	getenv func(string) string,
	pool *pgxpool.Pool,
	publicOrigin string,
	logger *slog.Logger,
) (nativeAuthComposition, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := nativeauth.ConfigFromEnv(getenv)
	if err != nil {
		return nativeAuthComposition{}, err
	}
	fileClients, err := nativeauth.LoadClientsFile(getenv(nativeauth.NativeClientsPathEnv))
	if err != nil {
		return nativeAuthComposition{}, err
	}
	if pool == nil {
		if len(fileClients) > 0 {
			logger.Warn("NATIVE_CLIENTS_PATH is set but no database is configured; native sign-in is not served")
		}
		return nativeAuthComposition{}, nil
	}
	store := nativeauth.NewStore(pool, cfg)
	if publicOrigin == "" {
		registered := len(fileClients) > 0
		if !registered {
			hasRows, probeErr := store.HasDBClients(ctx)
			if probeErr != nil {
				// A database that cannot answer this at boot cannot serve
				// the token endpoint either; say so and keep booting, the
				// authorize route refuses with 503 while the origin is unset.
				logger.Warn("could not check for registered native clients at boot", "err", probeErr)
			}
			registered = hasRows
		}
		if registered {
			return nativeAuthComposition{}, fmt.Errorf("%w", errNativeNeedsPublicOrigin)
		}
	}
	if len(fileClients) > 0 {
		logger.Info("native clients file layer loaded", "clients", len(fileClients))
	}
	return nativeAuthComposition{
		registry:  nativeauth.NewRegistry(fileClients, pool),
		store:     store,
		validator: nativeauth.NewAccessValidator(pool),
	}, nil
}
