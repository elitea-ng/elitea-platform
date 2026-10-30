package main

// Remote agents (ADR-0044 decision 4, #1501): the delegation assertion mint
// the runtime control service serves to the worker, and the inbound checks
// of the MCP endpoint.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	v2mcp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
)

// remoteAgentSettings reads the remote_agents section on each call (the
// issuer allowlist caches it itself).
func remoteAgentSettings(pool *pgxpool.Pool) remoteagent.Settings {
	return func(ctx context.Context) (platformconfig.RemoteAgentSettings, error) {
		return platformconfig.LoadRemoteAgentSettings(ctx, pool)
	}
}

// remoteAgentSettingsTTL is how long a replica's mint keeps the section: a
// worker asks before each remote MCP request it has no assertion for.
const remoteAgentSettingsTTL = 5 * time.Second

// cachedRemoteAgentSettings is remoteAgentSettings read at most once per
// remoteAgentSettingsTTL per replica, like the other platform-config
// readers on hot paths.
func cachedRemoteAgentSettings(pool *pgxpool.Pool) remoteagent.Settings {
	cache := platformconfig.NewCachedSection(remoteAgentSettings(pool), remoteAgentSettingsTTL)
	return cache.Get
}

func remoteAgentAudit(pool *pgxpool.Pool) (*repos.RemoteAgentAudit, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, fmt.Errorf("construct remote agent audit appender: %w", err)
	}
	return repos.NewRemoteAgentAudit(pool, appender)
}

// remoteAgentMint builds the mint the runtime control service serves.
// runtimePool reads the run; pool reads the settings and takes the audit.
// A nil signer (no agent assertion key ring) still builds the service: it
// answers DISABLED to a run that is not governed (one run read, nothing
// else) and refuses a governed run, so that run refuses its remote call
// instead of making it unsigned.
func remoteAgentMint(runtimePool, pool *pgxpool.Pool, signer *eliteanative.DelegationSigner) (control.DelegationAssertions, error) {
	runs, err := repos.NewRemoteAgentRuns(runtimePool)
	if err != nil {
		return nil, err
	}
	nested, err := repos.NewCurrentNestedApplicationVersionRepository(runtimePool)
	if err != nil {
		return nil, fmt.Errorf("construct remote agent nested reader: %w", err)
	}
	audit, err := remoteAgentAudit(pool)
	if err != nil {
		return nil, err
	}
	var sign remoteagent.Signer
	if signer != nil {
		sign = signer
	}
	return remoteagent.NewMintService(runs, nested, sign, cachedRemoteAgentSettings(pool), audit)
}

// remoteAgentJWKSTimeout bounds one fetch of a trusted issuer's JWKS.
const remoteAgentJWKSTimeout = 5 * time.Second

// remoteAgentInboundAtBoot builds the inbound checks of the MCP endpoint:
// the issuer allowlist (the remote_agents section plus the
// ELITEA_REMOTE_AGENT_TRUSTED_ISSUERS bootstrap), the one-time record and
// the governed agent invocation policy. A malformed bootstrap refuses boot.
func remoteAgentInboundAtBoot(
	admission profile.Admission,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	publicOrigin string,
	logger *slog.Logger,
) (*v2mcp.RemoteAgentInbound, error) {
	if pool == nil || publicOrigin == "" {
		// No database or no public origin: nothing can be verified, and the
		// MCP routes stay as they are.
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	raw, _ := lookup(remoteagent.EnvTrustedIssuers)
	bootstrap, err := remoteagent.ParseEnvTrustedIssuers(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", remoteagent.EnvTrustedIssuers, err)
	}
	replays, err := repos.NewRemoteAgentReplays(pool)
	if err != nil {
		return nil, err
	}
	audit, err := remoteAgentAudit(pool)
	if err != nil {
		return nil, err
	}
	allowlist := remoteagent.NewIssuerAllowlist(remoteAgentSettings(pool), bootstrap, func(jwksURL string) (coreoidc.KeySet, error) {
		return identityoidc.NewRemoteKeySet(identityoidc.RemoteKeySetConfig{
			JWKSURL: jwksURL, SupportedSigningAlgorithms: []string{string(eliteanative.ES256), string(eliteanative.RS256)},
			RequestTimeout: remoteAgentJWKSTimeout,
		}, nil)
	})
	verifier, err := remoteagent.NewVerifier(allowlist, replays, nil)
	if err != nil {
		return nil, err
	}
	inbound, err := v2mcp.NewRemoteAgentInbound(v2mcp.RemoteAgentInboundConfig{
		PublicOrigin: publicOrigin, Verifier: verifier, Audit: audit,
		Governed: admission.Governed(), Logger: logger,
	})
	if err != nil {
		return nil, errors.Join(errors.New("remote agent inbound"), err)
	}
	return inbound, nil
}
