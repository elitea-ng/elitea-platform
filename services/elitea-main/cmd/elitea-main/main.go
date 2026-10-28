package main

import (
	"context"
	"errors"
	"fmt"
	browserapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/browserauth"
	v2branding "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/brandpackage"
	appmailer "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/emailsettings"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimclient"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminui"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/gateway"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	v2analytics "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	applicationskillsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2canvaspresence "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	configurationapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2convs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2deepwiki "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	desktopopsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	v2evaluation "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	v2events "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/events"
	v2folders "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	indexingapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indextypesapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	v2inventory "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	v2mcp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	v2memories "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/memories"
	notificationsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	v2pipelinetriggers "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	predictapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
	projectinfoapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	v2projects "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/projects"
	promptcontextreadsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	v2retention "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/retention"
	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	socialapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/social"
	v2support "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/supportassistant"
	v2tags "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/tags"
	toolkitrun "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	v2toolkits "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/webedge"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/webhook"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	identityapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	notificationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/notifications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	socialapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	discovery "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/wikichat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/events"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
	infradb "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/natsbus"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/material"
	providerproxy "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/proxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		endpoint, err := healthcheckURL(os.LookupEnv, os.Args[2:]...)
		if err != nil {
			os.Exit(1)
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	// A one-shot operator command in the same image (legacy_pgvector_orphans.go):
	// it runs and exits, and never starts the service.
	if len(os.Args) > 1 && os.Args[1] == legacyOrphansCommand {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := runLegacyPgvectorOrphans(ctx, os.Args[2:], os.LookupEnv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	// The security audit chain verifier (#1114): it runs and exits.
	if len(os.Args) > 2 && os.Args[1] == "security-audit" && os.Args[2] == "verify" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := securityaudit.RunVerify(ctx, os.Args[3:], os.LookupEnv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	// The credential reference migration helper (#1124,
	// credential_governance_migrate.go): it runs and exits.
	if len(os.Args) > 1 && os.Args[1] == credentialGovernanceCommand {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := runCredentialGovernance(ctx, os.Args[2:], os.LookupEnv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	// The credential policy operator command (credential_governance.go).
	if len(os.Args) > 1 && os.Args[1] == credentialPolicyCommand {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		code := runCredentialPolicy(ctx, os.Args[2:], os.LookupEnv, os.Stdout, os.Stderr, func(p *pgxpool.Pool) func(context.Context) ([]profile.Prerequisite, error) {
			return credentialGovernancePrerequisites(os.LookupEnv, p)
		})
		stop()
		os.Exit(code)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil {
		logger.Error("elitea-main stopped with an error", "err", err)
		os.Exit(1)
	}
}

// developmentFlagsFromEnv validates the two development-only environment
// flags and reports whether the legacy-schema bootstrap was requested. It
// reads the environment through the injected getenv and has NO side effects,
// so its invariants are testable without executing startup — calling run() to
// exercise them would open database pools, contact the object store, and bind
// the public HTTP port.
//
// AUTH_DEV_MODE was an unauthenticated-admin bypass (ADR-0017). It is gone, so
// the variable is inert — but an operator who still sets it to "true" believes
// authentication is disabled and may have deployed accordingly, so refuse to
// start rather than ignoring it silently. A lingering "false" is harmless and
// stays tolerated.
//
// The legacy-schema bootstrap is a developer-machine convenience that runs the
// unversioned schema against an empty local database. It needs a "this is a
// developer machine" marker, not an authentication bypass, so it no longer
// cross-checks AUTH_DEV_MODE. It is self-gating and fail-closed: absent an
// explicit opt-in, nothing runs.
func developmentFlagsFromEnv(getenv func(string) string) (bootstrapLegacySchema bool, err error) {
	if getenv("AUTH_DEV_MODE") == "true" {
		return false, errors.New("AUTH_DEV_MODE=true is no longer supported: the development authentication bypass was removed (ADR-0017). Authenticate via OIDC or an API token and remove this variable")
	}

	bootstrapLegacySchema = getenv("ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA") == "true"
	if bootstrapLegacySchema {
		// Production shared/tenant histories are owned by elitea-migrate. A
		// deployment that has configured any real authentication mode is not a
		// developer machine, and must never bootstrap the legacy schema.
		//
		// The refusal NAMES THE SUPPORTED PATH (#556). An operator installing
		// onto an empty database used to read this message as "the only thing
		// that can build my schema will not run", and then applied
		// 001_initial.sql by hand against a production database. That is no
		// longer necessary: elitea-migrate embeds the same schema and applies
		// it itself when the database does not carry it, as the migrating role,
		// which is also what the histories need in order to ALTER those objects.
		for _, configured := range []string{"APPLICATION_SECRET_KEY", "OIDC_ISSUER_URL", "ELITEA_AUTH_CONFIG_FILE"} {
			if getenv(configured) != "" {
				return false, fmt.Errorf("ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA is a local-development flag and must not be set when %s is configured. "+
					"To build a schema on a deployment, run elitea-migrate: it applies the legacy-era schema to an empty database itself, "+
					"then the shared and tenant histories. Remove this variable", configured)
			}
		}
	}
	return bootstrapLegacySchema, nil
}

func run(ctx context.Context, logger *slog.Logger) (runErr error) {
	publicAddress, err := configuredHTTPAddress(os.LookupEnv)
	if err != nil {
		return err
	}
	bootstrapLegacySchema, err := developmentFlagsFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	// The ONE public project id. See resolvePublicProject for why an
	// environment that names two of them stops the process here rather than
	// producing a deployment whose surfaces silently disagree.
	publicProjectID, err := resolvePublicProject(logger, os.LookupEnv)
	if err != nil {
		return err
	}
	logger.Info("public project resolved", "public_project_id", publicProjectID)

	// The project vault's master key (#412).
	//
	// This runs BEFORE the database pool and before every handler, because the
	// fault it catches is silent everywhere else. The secrets handler holds the
	// one key source the vault has (#411/#399), and it used to ignore a
	// malformed value: it kept no key, then minted and wrote UNWRAPPED vaults
	// while the operator believed the keys were wrapped. Provisioning
	// succeeded, every later read succeeded, and nothing logged.
	//
	// So the check belongs here and not in the constructor. Two of the four
	// NewHandler callers build a handler PER REQUEST, so a constructor error
	// would arrive after provisioning had already written vaults, and one of
	// the others builds it inside the chi route tree, which cannot report an
	// error at all.
	//
	// The key bytes are discarded on purpose. Every handler reads the variable
	// again, so passing them on would create a SECOND key source, and one key
	// source is the rule #411/#399 established. This call decides one thing
	// only: whether the process may continue. An ABSENT key stops the process
	// too, unless the development opt-out is set (master_key_gate.go).
	masterKeyWarning, err := requireVaultMasterKey(os.Getenv)
	if err != nil {
		return err
	}
	if masterKeyWarning != "" {
		// The development opt-out. Loud on purpose: the operator cannot tell
		// this shape apart from a key that failed to arrive.
		logger.Warn(masterKeyWarning,
			"variable", v2secrets.MasterKeyEnvVar, "opt_out", v2secrets.AllowUnwrappedEnvVar)
	}

	// APPLICATION_SECRET_KEY signs personal access tokens. internal/api/router.go
	// passes it to v2auth.WithTokenSigningKey, and tokens.go answers 503
	// `{"error":"token service is not configured"}` to every /api/v2/auth/token
	// route while it is empty.
	//
	// Nothing else reported that. The service started. The Settings > Personal
	// Tokens screen rendered "No tokens yet — create your first API token".
	// Pressing Generate answered "The system did not create the token. Try
	// again." — advice that can never succeed. The whole OpenAI-compatible /llm
	// path goes with it, because a caller there authenticates with a personal
	// token. Observed on a live deployment.
	//
	// It stays a warning rather than a hard stop, for the reason the vault key
	// above gives: a deployment that never issues personal tokens is a real
	// shape and must still start. It must not be silent, though.
	//
	// One exception: a deployment with ELITEA_AUTH_CONFIG_FILE signs and
	// validates a personal access token with credentials.pat_signing_key_file
	// instead. See patSigner below. The warning below can therefore be noise on
	// that shape, and the route still works there.
	if os.Getenv("APPLICATION_SECRET_KEY") == "" && os.Getenv("ELITEA_AUTH_CONFIG_FILE") == "" {
		logger.Warn("no application secret key: personal access tokens are DISABLED, "+
			"every /api/v2/auth/token route answers 503, and the OpenAI-compatible "+
			"/llm path has no credential a caller can present",
			"variable", "APPLICATION_SECRET_KEY")
	}

	// Observability (issue #250): exports elitea-main's own request spans to
	// the same OTLP collector internal/api/v2/tracing proxies UI/worker
	// traces to, so the ingest pipeline is self-verifying — every request
	// this process serves produces a span an operator can see land in the
	// collector, with no separate "did tracing actually work" check needed.
	obsProvider, err := observability.New(ctx, observability.ConfigFromEnv("elitea-main", ""))
	if err != nil {
		return fmt.Errorf("initialize observability: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obsProvider.Shutdown(shutdownCtx); runErr == nil && err != nil {
			runErr = fmt.Errorf("shut down observability: %w", err)
		}
	}()

	// Read before anything starts: a bad value refuses the boot instead of
	// surfacing at the first SIGTERM.
	shutdownTimeout, err := shutdownTimeoutFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}

	// Database
	dbDSN := envOr("DATABASE_URL", "postgres://localhost:5432/elitea?sslmode=disable")
	pool, err := openDatabasePool(ctx, dbDSN, os.LookupEnv)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The credential governance profile (ADR-0039, #1111). Resolved before any
	// admission path exists, so a refused governed boot admits nothing.
	credentialGovernanceStore, err := dbrepos.NewCredentialGovernanceStore(pool)
	if err != nil {
		return err
	}
	credentialAdmission, credentialRevision, err := credentialGovernanceAtBoot(
		ctx, os.LookupEnv, credentialGovernanceStore,
		credentialGovernancePrerequisites(os.LookupEnv, pool),
	)
	if err != nil {
		return err
	}
	logger.Info("credential governance profile resolved",
		"profile", string(credentialAdmission.Profile()),
		"policy_revision", credentialRevision.ID,
		"continuation_guard", credentialAdmission.ContinuationGuard())
	auditExport, closeAuditExport, err := auditExportAtBoot(ctx, os.LookupEnv, credentialAdmission, pool, logger)
	if err != nil {
		return err
	}
	if closeAuditExport != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := closeAuditExport(shutdownCtx); runErr == nil && err != nil {
				runErr = fmt.Errorf("shut down audit export: %w", err)
			}
		}()
	}
	if err := startSecurityAuditRelay(ctx, credentialAdmission, credentialRevision, pool, logger, securityAuditExporter(auditExport)); err != nil {
		return err
	}
	// agentIdentity.Broker opens the run-scoped agent session at a governed
	// run's first credential redemption (#1118).
	securityInvalidations, err := securityInvalidationsAtBoot(credentialAdmission, logger)
	if err != nil {
		return err
	}
	credentialProbeHandler, err := credentialProbeAdmin(credentialAdmission, pool)
	if err != nil {
		return err
	}
	// The governed secret stores, built once (on first use, flag on only)
	// and shared by the reference admin API and the credential broker.
	governedStores := onceSecretStores(os.LookupEnv, pool)
	credentialReferencesHandler, err := credentialReferencesAdmin(credentialAdmission, credentialRevision, governedStores, pool, securityInvalidations)
	if err != nil {
		return err
	}

	if os.Getenv(v2secrets.MasterKeyEnvVar) != "" {
		if err := refuseUnwrappedVaultKeys(ctx, pool); err != nil {
			return err
		}
	}

	// The `centry.audit_events` writer.
	//
	// It is built HERE, once, and given to BOTH the router (which mounts the
	// audit middleware over the whole /api/v2 group) and the pipeline-trigger
	// handler (whose inbound route sits ABOVE that group and so writes its own
	// row). Two recorders would mean two background writers and two queues over
	// one table; one recorder is also what makes an inbound refusal and an
	// ordinary 403 land in the same trail with the same drop policy.
	auditRecorder := audit.NewPostgresRecorder(pool, logger)
	defer func() {
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFlush()
		if flushErr := auditRecorder.Flush(flushCtx); flushErr != nil {
			logger.Warn("could not flush the audit trail on shutdown", "err", flushErr)
		}
	}()

	// The kill switch (ADR-0041 decision 1, #1493): one admission gate per
	// replica, shared by the runtime admission stores and the suspend and
	// resume routes, and its scheduled self-test.
	executionSuspensions, err := newKillSwitch(pool, logger)
	if err != nil {
		return err
	}
	if err := startKillSwitchSelfTest(ctx, pool, executionSuspensions, auditRecorder, logger); err != nil {
		return err
	}

	// The domain-events Publisher — #876's second half. Every producer that
	// wires it (pipeline run admission, agent version publish/unpublish,
	// conversation create, artifact upload, moderation decision) reaches it
	// through the SAME instance, built once here, exactly like auditRecorder
	// above and for the same reason: two Publishers over one webhook registry
	// would mean two independent dispatch paths that could disagree about
	// what already fired.
	//
	// Its Bus is events.NoopBus{} — ALWAYS, including when the live-update
	// NATS bus below is configured. Domain events are a WEBHOOK contract
	// (the per-project, owner-configured, signed sinks), not a project-feed
	// one: their payloads carry data the project SSE stream's gate
	// (models.project_context.view, which viewers hold) does not cover — a
	// conversation.created carries a private conversation's name and
	// creator — and no web client listens for them. See
	// newDomainEventsPublisher, whose signature takes no Bus so this cannot
	// drift back by accident, and the SSE handler's own type allowlist
	// (internal/api/v2/events, forwardedEventTypes) as the second layer.
	//
	// Its one Sink is the webhook Dispatcher, wired only when the webhooks
	// table's repository composes (webhooksRepository never returns nil, but
	// the guard mirrors the one WebhookRepo's own composition documents: a
	// pool-less deployment gets a Dispatcher with a nil Repository, and
	// HandleDomainEvent's own nil guard turns that into a no-op rather than a
	// panic).
	// The SSRF guard every webhook destination is checked against — at
	// create/update time (Handler.validateDestination, fail-closed on a nil
	// guard) and again at every dial (Dispatcher's guarded Transport,
	// defence in depth against a row admitted some other way or a hostname
	// that resolves differently later). ELITEA_WEBHOOK_EGRESS_ALLOWLIST is
	// unset by default, which is a valid, safe "refuse all private-network
	// destinations" allowlist — see webhook.DestinationAllowlistEnv's own
	// doc comment. A malformed entry fails startup, the same posture
	// egresslib.Parse already enforces for GATEWAY_EGRESS_ALLOWLIST: a typo
	// that silently dropped a rule would either open the guard wider than
	// intended or wedge a legitimate private destination an operator meant
	// to permit.
	webhookAllowlist, err := webhook.ParseDestinationAllowlist(splitEnvList(os.Getenv(webhook.DestinationAllowlistEnv)))
	if err != nil {
		return fmt.Errorf("parse %s: %w", webhook.DestinationAllowlistEnv, err)
	}
	webhookDestinationGuard := webhook.NewDestinationGuard(webhookAllowlist)

	// The MCP OAuth and DCR proxies POST to a URL from the request body, and
	// a project viewer may call them (#6885). The same DNS-aware, dial-time
	// guard applies. ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST is unset by default,
	// which refuses every private, loopback and link-local identity provider.
	// An operator with a private IdP names it there. A malformed entry fails
	// startup, as for the webhook allowlist above.
	mcpOAuthAllowlist, err := webhook.ParseDestinationAllowlist(
		splitEnvList(os.Getenv("ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST")))
	if err != nil {
		return fmt.Errorf("parse ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST: %w", err)
	}
	mcpAuthorizationEgressGuard := webhook.NewDestinationGuard(mcpOAuthAllowlist)

	webhookDeliveries := webhookDeliveriesRepository(pool)
	webhookDispatcher := webhook.NewDispatcher(webhooksRepository(pool), webhookDeliveries, webhook.WithGuard(webhookDestinationGuard))

	// The live-update plane: NATS core pub/sub (internal/infra/natsbus). One
	// connection carries the project SSE stream (EventSource) and canvas
	// presence's roster (KV) and publish. Domain events are NOT on it — see
	// domainEvents above.
	// Unset ELITEA_EVENTS_NATS_URL leaves the plane absent; a configured but
	// unreachable server stops startup — see newEventsNATSConn for both.
	//
	// eventSource stays a nil INTERFACE when there is no bus: assigning a nil
	// *natsbus.EventBus to RouterConfig.EventSource would box a typed nil,
	// read as non-nil by router.go's gate, and mount the SSE route over a nil
	// bus (#86).
	eventsNATS, err := newEventsNATSConn(os.LookupEnv, logger)
	if err != nil {
		return fmt.Errorf("compose live-update plane: %w", err)
	}
	var (
		eventSource v2events.EventSource
		liveNATSBus *natsbus.EventBus
	)
	if eventsNATS != nil {
		liveNATSBus = natsbus.NewFromConn(eventsNATS, "elitea-main", natsbus.WithBufferedPublish())
		// Flush then drain, both bounded: buffered publishes reach the
		// server before exit (natsbus.EventBus.Close).
		defer liveNATSBus.Close()
		eventSource = liveNATSBus
		logger.Info("live-update plane enabled (nats transport)", "server", eventsNATS.ConnectedUrlRedacted())
	} else {
		logger.Warn("live-update plane disabled: " + eventsNATSURLEnv + " is unset, so " +
			"/api/v2/events/prompt_lib/{projectID} is not registered and canvas presence is per-replica")
	}
	domainEvents := newDomainEventsPublisher(webhookDispatcher)

	// public.pipeline_runs' repository (migrations/shared/0124) — the write
	// half (pipelinetriggers.WithRunTracker, below) and the read half
	// (execution.WithAfterSettleHooks / output.WithFailureObserver, in
	// runtimeRoot's own composition further down) of
	// pipeline.run.succeeded/failed. One instance, for the same "two halves
	// must agree" reason webhookDispatcher and domainEvents are each built
	// once here rather than per call site.
	pipelineRunsRepo := pipelineRunsRepository(pool)

	// Object store. Production remains fail-closed: when the capability is
	// enabled, startup requires a working S3/Azure/GCS backend. The mixed
	// migration deployment explicitly disables these still-incomplete Go
	// routes and keeps the current Centry artifacts capability authoritative;
	// this avoids adding a second storage service or inventing a filesystem
	// compatibility backend.
	var objectStore storage.ObjectStore
	// The workspace store's physical buckets (ADR-0034 decision 3): the
	// deployment's own, and the optional dedicated one.
	var workspaceInputs workspaceHandoffInputs
	if os.Getenv("ELITEA_ARTIFACTS_ENABLED") != "false" {
		storageCfg, err := storage.ConfigFromEnv(os.LookupEnv)
		if err != nil {
			return fmt.Errorf("load storage configuration: %w", err)
		}
		workspaceInputs, err = workspaceStorageFromEnv(ctx, storageCfg, os.LookupEnv)
		if err != nil {
			return err
		}
		objectStore, err = newObjectStore(ctx, storageCfg)
		if err != nil {
			return fmt.Errorf("create object store: %w", err)
		}
		if err := objectStoreReadinessProbe(ctx, objectStore); err != nil {
			return fmt.Errorf("object store readiness probe: %w", err)
		}
		if err := configureObjectStoreRetentionLifecycle(ctx, objectStore); err != nil {
			return fmt.Errorf("configure object store retention lifecycle: %w", err)
		}
	} else {
		logger.Info("Go artifacts capability disabled; current Centry artifacts routes remain authoritative")
	}

	// The unversioned legacy bootstrap exists only for an empty local developer
	// database. Production shared/tenant histories are owned by elitea-migrate.
	if bootstrapLegacySchema {
		if err := infradb.RunMigrations(ctx, pool); err != nil {
			return fmt.Errorf("bootstrap local legacy database schema: %w", err)
		}
	}

	backfillProjectSecretsHeaderValues(ctx, pool, logger)

	var productionAuth *api.ProductionAuthRoutes
	var currentProjectList *v2projects.CurrentProjectListRoute
	var currentSocialAuthors *socialapi.CurrentAuthorsRoute
	var currentSocialAvatar *socialapi.CurrentAvatarRoute
	var currentNotifications *notificationsapi.CurrentNotificationAPIRoute
	var currentNotificationStore notificationapp.Store
	var currentNotificationEvents *notificationsapi.CurrentNotificationEventsRoute
	var formGraph *authcomposition.FormGraph
	var principalValidator apimw.PrincipalValidator
	// formSessions reads the Form plane's session cookie for every route's
	// apimw.Auth. It replaced the forwarded-identity verifier the X-Auth-*
	// edge projection needed (#1359), and is assigned exactly where that was:
	// only inside the authEnabled block below.
	var formSessions apimw.FormSessionAuthenticator
	// firstLoginPolicy travels OUT of the authEnabled block below, because the
	// plane that consumes it is composed further down. `initial_global_admins`
	// used to reach only the browserauth plane, and internal/api/
	// production_router.go does not mount that plane when single sign-on is
	// configured, so an SSO-only deployment had no way to name its first
	// administrator except an INSERT into auth_core__user_role.
	var firstLoginPolicy v2auth.FirstLoginPolicy
	authConfigPath, authEnabled, err := configuredAuthConfigPath(os.LookupEnv)
	if err != nil {
		return err
	}
	// The local username/password sign-in. OFF unless explicitly enabled:
	// a deployment signs people in through OIDC or SAML (and provisions them
	// through SCIM). The authentication document is still read either way —
	// PAT validation and the runtime depend on it.
	formSignInEnabled, err := formSignInEnabledFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	if formSignInEnabled && !authEnabled {
		logger.Warn("ELITEA_FORM_LOGIN_ENABLED=true has no effect without ELITEA_AUTH_CONFIG_FILE: " +
			"Form sign-in needs the authentication document that names its users file")
	}
	// The brand pack resolver is built ONCE, here, because two composition
	// roots consume it: the login page inside the Form graph and the
	// bootstrap/admin routes inside the router (ADR-0024).
	brandPack, err := brandPackConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load brand pack settings: %w", err)
	}
	brandingResolver := v2branding.NewResolver(v2branding.ResolverConfig{
		PackPath: brandPack.Path,
		Pool:     pool,
	})
	// ADR-0025 WP1: the discovery document and the brand pack JSON are
	// anonymous documents with absolute URLs. Both settings are read once.
	deploymentKind, err := deploymentKindFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load deployment kind: %w", err)
	}
	publicOrigin, err := publicOriginFromEnv(os.LookupEnv, logger)
	if err != nil {
		return fmt.Errorf("load public origin: %w", err)
	}
	// ADR-0025 WP2: the native authorization server. Built whenever a pool
	// exists, so its routes are mounted and answer 404 until a client is
	// registered; the access-token validator joins the API group, the /auth
	// edge endpoint and the Form graph's gateway edge below.
	native, err := nativeAuthFromEnv(ctx, os.Getenv, pool, publicOrigin, logger)
	if err != nil {
		return fmt.Errorf("load native authorization: %w", err)
	}

	// Outbound e-mail (ADR-0024 WP7, gap G7). The environment is the BOOTSTRAP
	// DEFAULT; the admin E-mail page's rows lay over it, and the resolver
	// merges the two on every send. So a deployment that ships SMTP_HOST in
	// its chart keeps working with no rows at all, and an operator who has
	// never touched the chart can still turn e-mail on.
	//
	// There is no transport built here any more, and that absence is the
	// change: a transport built at boot is a transport that needs a restart to
	// correct, which is why every invitation on a mis-set deployment reported
	// `invitation_delivered: false` until a redeploy.
	mailSettings, err := mailerConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load outbound e-mail settings: %w", err)
	}
	// The SCIM access token lifetime (shared migration 0135). A value that is
	// not a duration, or is outside 5m..24h, stops the boot and names the
	// variable, rather than running with a lifetime nobody chose.
	scimAccessTokenTTL, err := scimclient.AccessTokenTTLFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("load SCIM settings: %w", err)
	}
	// ONE store and ONE resolver, shared by the composer that sends and the
	// admin surface that writes (wired through api.Config.EmailSettings).
	// Building two would let the page that reports a relay configured and the
	// composer that dials it disagree.
	emailResolver := emailsettings.NewResolver(
		emailsettings.NewStore(pool, v2secrets.NewHandler(pool)),
		mailSettings.Settings,
		mailSettings.Password,
	)
	logger.Info("outbound e-mail settings loaded",
		"environment_complete", mailSettings.Complete(),
		"environment_host", mailSettings.Settings.Host,
		"suppressed", mailSettings.Suppressed)
	mailComposer, err := appmailer.New(appmailer.Config{
		Brand:         brandingResolver,
		PublicBaseURL: mailSettings.Settings.PublicBaseURL,
		Suppressed:    mailSettings.Suppressed,
		Resolver:      emailResolver,
	})
	if err != nil {
		return fmt.Errorf("compose outbound e-mail: %w", err)
	}

	// The Form document's trusted proxy CIDRs, kept for the SCIM token
	// endpoint's caller-address resolver, which every plane builds below.
	var authDocumentTrustedProxyCIDRs []string
	if authEnabled {
		if err := pool.Ping(ctx); err != nil {
			return fmt.Errorf("verify authentication PostgreSQL dependency: %w", err)
		}
		authConfig, loadErr := authcomposition.Load(authConfigPath)
		if loadErr != nil {
			return fmt.Errorf("load production Form authentication: %w", loadErr)
		}
		authDocumentTrustedProxyCIDRs = append([]string(nil), authConfig.TrustedProxyCIDRs...)
		// The document is the PRIMARY source, so the list has one meaning on
		// whichever browser plane ends up mounted.
		firstLoginPolicy = v2auth.FirstLoginPolicy{
			InitialGlobalAdmins: authConfig.Identity.InitialGlobalAdmins,
		}
		formGraph, err = authcomposition.NewFormGraph(ctx, authConfig, authcomposition.FormGraphDependencies{
			PostgreSQL:        pool,
			Brand:             brandingResolver,
			FormSignInEnabled: formSignInEnabled,
		})
		if err != nil {
			return fmt.Errorf("compose production Form authentication: %w", err)
		}
		if formGraph.FormSignInEnabled() {
			logFormUserConfiguration(logger, formGraph.FormUsers())
			productionAuth, err = api.NewProductionAuthRoutes(formGraph.BrowserRoutes())
			if err != nil {
				return fmt.Errorf("mount production Form authentication: %w", err)
			}
		} else {
			logFormSignInDisabled(logger, formGraph.FormUsers())
		}
		principalValidator = authsvc.NewPrincipalValidator(pool)
		formSessions = formGraph.FormSessions()
		logger.Info("production Form authentication enabled", "form_sign_in", formGraph.FormSignInEnabled())
	}

	// The caller-address resolver every plane keys its limiters on: the SCIM
	// token endpoint, the native endpoints and the single-sign-on attempt
	// limiter below. Built here, before the single-sign-on block, because
	// that block attaches it.
	scimClientAddresses, err := scimClientAddressesFromConfig(os.Getenv, authDocumentTrustedProxyCIDRs, logger)
	if err != nil {
		return fmt.Errorf("load SCIM caller-address settings: %w", err)
	}

	// Wire OIDC browser-session authentication when OIDC_ISSUER_URL is set.
	// SessionHandler and OIDCHandler are independent of the FormGraph path and
	// can coexist with it (both populate RouterConfig.Auth) — but only because
	// internal/api/production_router.go now resolves the /auth prefix to
	// ONE owner. Composing both used to panic chi at startup; see the comment
	// there before assuming any second browser-auth plane can simply be added.
	var oidcSessionHandler *v2auth.SessionHandler
	var oidcOIDCHandler *v2auth.OIDCHandler
	// sessionManager is declared out here because apiGroupAuthConfig below
	// needs it and the block that builds it is conditional. Nil is the
	// "no server-side sessions" value every consumer accepts.
	var sessionManager *browsersession.Manager
	oidcCfg, err := v2auth.OIDCConfigFromEnv()
	if err != nil {
		return fmt.Errorf("load OIDC configuration: %w", err)
	}
	// A deployment can now federate through an AUTHORED provider instead of the
	// environment (`elitea_auth.identity_providers`, shared migration 0095), so
	// the environment is no longer the only thing that can turn OIDC on.
	//
	// Whether the routes are MOUNTED stays a boot decision, and it has to:
	// internal/api/production_router.go allows exactly one browser-auth plane to
	// own /auth, so which plane owns it cannot change under a running
	// process. What the mounted routes DO is resolved per request, so editing,
	// replacing or disabling a provider needs no restart — only introducing the
	// first one on a deployment that had none does.
	identityProviderStore := identityproviders.NewStore(pool)
	var storedSAMLProvider, storedOIDCProvider bool
	storedSAMLProvider, err = v2auth.HasEnabledSAMLProvider(ctx, identityProviderStore)
	if err == nil {
		storedOIDCProvider, err = v2auth.HasEnabledOIDCProvider(ctx, identityProviderStore)
	}
	switch {
	case err == nil:
	case identityproviders.IsSchemaMissing(err):
		// The migration has not been applied to this database yet. Warn and
		// continue WITHOUT the browser-auth plane, which is what this
		// deployment did before the table existed at all.
		//
		// Failing the boot here would turn a schema-ordering hiccup — a pod
		// that starts before the migration job finishes — into a total outage,
		// and it would do so on deployments that use form authentication and
		// never federate a login.
		logger.Warn("identity provider table is absent; starting without single sign-on",
			"err", err)
	default:
		// Any OTHER read failure IS fatal. A deployment that can reach its
		// database and still cannot read this table must not start silently
		// unfederated, with every login answering "single sign-on is not
		// available" and no statement of why.
		return fmt.Errorf("read the enabled identity provider: %w", err)
	}
	var oidcSAMLHandler *v2auth.SAMLHandler
	if oidcCfg != nil || storedOIDCProvider || storedSAMLProvider {
		appSecretKey := os.Getenv("APPLICATION_SECRET_KEY")
		if appSecretKey == "" {
			return errors.New("APPLICATION_SECRET_KEY is required when single sign-on is configured")
		}
		// A single-sign-on-only deployment has no authentication configuration
		// document at all, and it is exactly the shape that cannot make an
		// administrator any other way. Fall back to the environment there, and
		// only there, so the document stays the single source when it exists.
		firstLoginPolicy = singleSignOnFirstLoginPolicy(firstLoginPolicy, v2auth.InitialGlobalAdminsFromEnv)
		// The server-side browser session (migrations/shared/0117).
		//
		// ONE manager for every plane. The session handler, the OIDC plane, the
		// SAML plane and apimw.Auth all reach the same rows, so a second
		// manager built anywhere would give one of them a different policy and
		// a different clock. The policy is read here because a bad duration
		// must stop the boot, not be defaulted away hours later.
		//
		// A deployment with no pool composes no manager and keeps the legacy
		// signed cookie. Every consumer treats nil that way on purpose; see
		// internal/auth/browsersession.
		sessionPolicy, policyErr := browsersession.PolicyFromEnv()
		if policyErr != nil {
			return fmt.Errorf("read the browser session policy: %w", policyErr)
		}
		if pool != nil {
			store, storeErr := browsersession.NewPostgresStore(pool)
			if storeErr != nil {
				return fmt.Errorf("compose the browser session store: %w", storeErr)
			}
			browserSessions, managerErr := browsersession.NewManager(store, sessionPolicy)
			if managerErr != nil {
				return fmt.Errorf("compose the browser session manager: %w", managerErr)
			}
			sessionManager = browserSessions
			logger.Info("server-side browser sessions are enabled",
				"idle_timeout", sessionPolicy.IdleTimeout,
				"absolute_lifetime", sessionPolicy.AbsoluteLifetime,
				"reject_legacy_cookies", sessionPolicy.RejectLegacyCookies)
		}
		oidcSessionHandler = v2auth.NewSessionHandler(pool, appSecretKey).
			WithSessionManager(sessionManager)
		oidcOIDCHandler, err = v2auth.NewOIDCHandler(ctx, oidcCfg, pool, appSecretKey)
		if err != nil {
			return fmt.Errorf("initialize OIDC handler: %w", err)
		}
		oidcOIDCHandler = oidcOIDCHandler.WithSessionManager(sessionManager)
		vault := v2secrets.NewHandler(pool)
		oidcOIDCHandler = oidcOIDCHandler.
			WithProviderStore(identityProviderStore, vault).
			WithFirstLoginPolicy(firstLoginPolicy)
		switch {
		case storedOIDCProvider:
			logger.Info("OIDC authentication enabled from an authored identity provider")
		case oidcCfg != nil:
			logger.Info("OIDC authentication enabled", "issuer", oidcCfg.IssuerURL)
		}

		// The SAML handler is built whenever the browser plane is mounted at
		// all, not only when a SAML provider exists today. Its routes resolve
		// their provider per request and answer 503 while none is enabled, so
		// authoring the first SAML provider on a deployment that already
		// federates OIDC needs no restart. The reverse — a deployment with NO
		// browser plane at boot — still does, because which plane owns
		// /auth is fixed there (internal/api/production_router.go).
		oidcSAMLHandler = v2auth.NewSAMLHandler(
			pool, appSecretKey, identityProviderStore, vault,
			os.Getenv("COOKIE_SECURE") != "false",
		).WithFirstLoginPolicy(firstLoginPolicy).WithSessionManager(sessionManager)
		if storedSAMLProvider {
			logger.Info("SAML authentication enabled from an authored identity provider")
		}

		// The begin/callback attempt limiter for both planes (#1078).
		if err := composeSingleSignOnAttempts(
			pool, appSecretKey, scimClientAddresses, oidcOIDCHandler, oidcSAMLHandler, logger,
		); err != nil {
			return err
		}
	}

	// Governed Entra custody and exchangers (#1117). Nil with the flag off
	// or without an Entra provider; then neither plane changes.
	entraGoverned, err := entraGovernanceAtBoot(ctx, credentialAdmission, pool, identityProviderStore,
		func() ([]byte, error) { return v2secrets.MasterKeyFromEnv(os.Getenv) }, securityInvalidations, logger)
	if err != nil {
		return err
	}
	if entraGoverned != nil {
		oidcOIDCHandler = oidcOIDCHandler.WithEntraGovernance(entraGoverned.Delegations)
		oidcSessionHandler = oidcSessionHandler.WithEntraGovernance(entraGoverned.Delegations)
	}

	// The Elitea-native agent assertion issuer (#1153): its provider for the
	// registry and its public JWKS and issuer metadata. Empty unless governed
	// with a relying party configured.
	eliteaNative, err := eliteaNativeAgentIdentityAtBoot(ctx, credentialAdmission, credentialRevision,
		os.LookupEnv, pool, publicProjectID, logger)
	if err != nil {
		return err
	}
	// The agent identity registry, admin APIs and session broker. Built
	// after the Entra custody: the Entra Agent ID provider (#1152) reads
	// the delegations for Agent OBO.
	agentIdentity, err := agentIdentityAdmin(credentialAdmission, credentialRevision, pool, securityInvalidations,
		agentIdentityProviderSources{
			Native: eliteaNative.Provider,
			Entra:  entraAgentIdentitySource(credentialAdmission, credentialRevision, os.LookupEnv, entraGoverned, logger),
		})
	if err != nil {
		return err
	}
	if agentIdentity.Reconciler != nil {
		go agentIdentity.Reconciler.Run(ctx)
	}
	// Mandate-bound schedules, triggers and index schedules (#1122). Empty
	// with the flag off.
	governedTriggers, err := governedTriggersAtBoot(credentialAdmission, credentialRevision, pool)
	if err != nil {
		return err
	}

	// The operation decider and the credential broker, its only caller
	// (#1116, #1118). Built after the Entra custody so the decider's
	// delegation gate reads it where it is configured. Nil with the flag off.
	credentialCatalog := credentialOperationCatalog()
	credentialDecider, credentialGrantsAdminHandler, err := credentialGrantGovernance(
		credentialAdmission, pool, credentialGovernanceStore, entraDelegations(entraGoverned), credentialCatalog, securityInvalidations)
	if err != nil {
		return err
	}
	// Provider delegated OAuth (#1121): consent routes and the token source
	// the broker reads for Jira, Confluence and Aha! oauth_delegated uses.
	// Nil with the flag off or with no provider OAuth application.
	vaultMasterKey := func() ([]byte, error) { return v2secrets.MasterKeyFromEnv(os.Getenv) }
	// Governed MCP (#1123): the inbound resource server and remote MCP
	// OAuth, which joins provider OAuth custody below. Empty with the flag
	// off.
	governedMCPComposed, err := governedMCPAtBoot(credentialAdmission, os.LookupEnv, pool, vaultMasterKey, publicOrigin,
		mcpAuthorizationEgressGuard, logger)
	if err != nil {
		return err
	}
	providerOAuthGoverned, err := providerOAuthWithMCPAtBoot(credentialAdmission, os.LookupEnv, pool,
		vaultMasterKey, governedMCPComposed, logger)
	if err != nil {
		return err
	}
	credentialBroker, err := credentialBrokerAtBoot(credentialAdmission, governedStores, pool, credentialDecider,
		credentialGovernanceStore, agentIdentity.Broker, entraGoverned, providerOAuthGoverned.Tokens, credentialCatalog, credentialRevision,
		securityInvalidations, governedMCPComposed.authorizations(), logger)
	if err != nil {
		return err
	}
	// A nil *Broker boxed into the interface would look composed.
	var runtimeCredentialBroker storage.CredentialRedeemer
	if credentialBroker != nil {
		runtimeCredentialBroker = credentialBroker
	}
	// A governed run's client token is scoped to the model gateway and its
	// project's artifacts (#1461): the issuer for the content server, the
	// validator for those routes. Empty with the flag off.
	runTokens, err := runTokensAtBoot(credentialAdmission, formGraph, pool)
	if err != nil {
		return err
	}

	// A deployment where nobody can sign in through a browser is legal (an
	// API-only install), but it is never what a first install meant.
	if oidcSessionHandler == nil && (formGraph == nil || !formGraph.FormSignInEnabled()) {
		logger.Warn("no browser sign-in is enabled: configure OIDC (OIDC_ISSUER_URL) or an identity provider, " +
			"or set ELITEA_FORM_LOGIN_ENABLED=true to use the Form users file")
	}

	// People whom an earlier release signed in as `<login>@centry.user`. A
	// warning only; see warnReservedDomainSignInAccounts.
	warnReservedDomainSignInAccounts(ctx, logger, pool)

	// ONE call site, placed AFTER both assignments above.
	//
	// `initial_global_admins` reaches this variable from two sources — the
	// authentication configuration document, and the environment fallback that
	// only a single-sign-on-only deployment uses — and the value is final here
	// whichever one supplied it. The line the SSO block used to print reported
	// a bare count, and it printed nothing at all for a deployment that
	// configures the list in a document and mounts no SSO plane.
	logInitialGlobalAdminShapes(logger, firstLoginPolicy.InitialGlobalAdmins)

	// The credential set the whole /api/v2 group authenticates with — and now
	// the credential set EVERY per-route composition in this file uses.
	//
	// It is built here, immediately after both browser-authentication planes
	// are final, because every field it reads is final at this point:
	// formGraph, principalValidator and formSessions are assigned
	// only inside the `authEnabled` block above, and oidcSessionHandler only
	// inside the single-sign-on block above. Nothing between there and here
	// assigns any of them.
	//
	// Under production Form authentication the set is the graph's token
	// validator, the Form session reader and the browser session secret; under OIDC-only it is the session cookie plus the LocalValidator
	// for the tokens APPLICATION_SECRET_KEY signs — which is also how a
	// provider's callback bearer, minted by the same key, is read back on its
	// upload. See apiGroupAuthConfig for both shapes and why each carries a
	// principal validator.
	//
	// It is ALSO the input to productionAuthenticationComposed, the predicate
	// the capability gates below ask.
	//
	// Every route that a browser reaches now takes this value. It used to be
	// one composition among many: sixteen of the twenty-one AuthConfig
	// literals in this file left SessionSecret empty, so apimw.Auth's cookie
	// branch was inert on them and a browser holding a VALID session got
	// `401 missing authorization header`. The SPA reads that 401 as a lost
	// session and opens a fresh OIDC authorize window, so the symptom was a
	// login loop on a healthy backend. Measured on GET
	// /api/v2/elitea_core/project_info/prompt_lib/{id}, GET
	// /api/v2/social/authors/{id} and GET /api/v2/social/avatar/{id}.
	//
	// Reusing one value — rather than adding the missing field to each literal
	// — is what makes the drift impossible to repeat: there is one composition
	// left to keep in sync with itself. TestNoPrivateAuthConfigLiteralsInMain
	// fails the build if a second one appears. See chatConfigAuthConfig, which
	// took this shape first (#301).
	var sessionTokens apimw.TokenValidator
	if secretKey := os.Getenv("APPLICATION_SECRET_KEY"); secretKey != "" && pool != nil {
		sessionTokens = authsvc.NewLocalValidator(pool, secretKey)
	}
	apiGroupAuth := withNativeTokens(apiGroupAuthConfig(
		formGraph,
		principalValidator,
		formSessions,
		authsvc.NewPrincipalValidator(pool),
		sessionTokens,
		os.Getenv("APPLICATION_SECRET_KEY"),
		oidcSessionHandler != nil,
		sessionManager,
	), native.apiTokens())

	// The browser-only routes: the project switcher, the notification list and
	// the notification event stream.
	//
	// Each one used to be composed TWICE — once inside the `authEnabled`
	// block, from formGraph and friends, and once again here for the OIDC-only
	// shape — and the two drifted. The OIDC-only composition left Validator
	// empty, so a personal access token that every other /api/v2 route accepts
	// answered 401 on exactly these three. That is the defect #301 fixed on
	// chat_config, in a third place.
	//
	// One composition, one gate. productionAuthenticationComposed is true in
	// exactly the union of the two former gates — `formGraph != nil ||
	// oidcSessionHandler != nil` — because apiGroupAuthConfig returns the zero
	// AuthConfig when neither plane exists. So no route gains or loses a
	// deployment. What each one gains is the credential the other composition
	// already had: a DELIBERATE widening on an OIDC-only deployment, on the
	// same three properties apiGroupAuthConfig states — the token row is read
	// live, its owner must be active, and a session cookie carries no `uuid`
	// claim, so no cookie can be replayed as a bearer token.
	if productionAuthenticationComposed(apiGroupAuth) {
		currentProjectList, err = v2projects.NewCurrentProjectListRoute(
			sqlcgen.New(pool),
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current project-list route: %w", err)
		}
		notificationRepository, repositoryErr := dbrepos.NewCurrentNotificationRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf("compose current notification repository: %w", repositoryErr)
		}
		currentNotifications, err = notificationsapi.NewCurrentNotificationAPIRoute(
			notificationRepository,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current notification API route: %w", err)
		}
		currentNotificationStore = notificationRepository
		notificationEventsRepository, repositoryErr :=
			dbrepos.NewCurrentNotificationEventRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf("compose current notification events repository: %w", repositoryErr)
		}
		currentNotificationEvents, err = notificationsapi.NewCurrentNotificationEventsRoute(
			notificationEventsRepository,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current notification events route: %w", err)
		}
		logger.Info("browser project-list and notification routes enabled")
	}

	// The Social author and avatar routes.
	//
	// Both answered `401 missing authorization header` to a browser holding a
	// valid session: their AuthConfig carried the forwarded-identity verifier
	// and no SessionSecret, and the browser edge strips every
	// inbound X-Auth-* header and runs no edgeAuth, so forwarded identity
	// never arrives and cannot be made to. The avatar route is what renders
	// the user menu, so the failure was visible on every page.
	//
	// The gate stays `formGraph != nil`. It does NOT move to
	// productionAuthenticationComposed. internal/api/router.go mounts a
	// compatibility `/social` handler in the same group, so composing these
	// routes on a deployment that does not have them today would shadow it.
	// That is a route-availability change, not an authentication fix, and it
	// belongs to its own change. An OIDC-only deployment therefore still
	// answers 404 on these two paths.
	if formGraph != nil {
		socialAuthorsRepository, repositoryErr := dbrepos.NewCurrentSocialAuthorsRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf("compose current Social authors repository: %w", repositoryErr)
		}
		socialAuthorsService, serviceErr := socialapp.NewCurrentAuthorsService(socialAuthorsRepository)
		if serviceErr != nil {
			return fmt.Errorf("compose current Social authors service: %w", serviceErr)
		}
		currentSocialAuthors, err = socialapi.NewCurrentAuthorsRoute(
			socialAuthorsService,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current Social authors route: %w", err)
		}
		socialAvatarRepository, repositoryErr := dbrepos.NewCurrentSocialAvatarRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf("compose current Social avatar repository: %w", repositoryErr)
		}
		currentSocialAvatar, err = socialapi.NewCurrentAvatarRoute(
			socialAvatarRepository,
			objectStore,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current Social avatar route: %w", err)
		}
		logger.Info("current Social author and avatar routes enabled")
	}

	currentProjectInfoSettings, err := currentProjectInfoConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load current project-info settings: %w", err)
	}
	if currentProjectInfoSettings.Enabled &&
		(formGraph == nil || principalValidator == nil || formSessions == nil) {
		return errors.New("ELITEA_PROJECT_INFO_ENABLED requires production authentication")
	}
	var currentProjectInfo *projectinfoapi.CurrentProjectInfoRoute
	if currentProjectInfoSettings.Enabled {
		currentProjectInfoRepository, repositoryErr :=
			projectinfoapi.NewCurrentProjectInfoRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf("compose current project-info repository: %w", repositoryErr)
		}
		currentProjectInfo, err = projectinfoapi.NewCurrentProjectInfoRoute(
			currentProjectInfoRepository,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current project-info route: %w", err)
		}
		logger.Info("current project-info route enabled")
	}

	currentIndexTypesSettings, err := currentIndexTypesConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load current index-types settings: %w", err)
	}
	// The gate is the credential plane, NOT the FormGraph — the same correction
	// gap G2 made for ELITEA_CONFIGURATIONS_ENABLED below.
	//
	// This used to read `formGraph == nil || principalValidator == nil ||
	// forwardedIdentityVerifier == nil`. All three are assigned only inside the
	// `authEnabled` block, so ELITEA_AUTH_CONFIG_FILE was the ONLY way to
	// satisfy it and an install with corporate single sign-on could not turn
	// the capability on at all. That mattered more once the prototype fallback
	// was deleted (#394): an OIDC install would answer 404 on a path the
	// published contract declares.
	//
	// productionAuthenticationComposed asks what the route actually needs: one
	// reader of the caller's credential plus a PrincipalValidator. A deployment
	// with NO authentication still fails it, because apiGroupAuthConfig hands
	// back the zero AuthConfig there.
	if currentIndexTypesSettings.Enabled && !productionAuthenticationComposed(apiGroupAuth) {
		return errors.New("ELITEA_INDEX_TYPES_ENABLED requires an authenticated deployment")
	}
	var currentIndexTypes *indextypesapi.CurrentIndexTypesRoute
	var attachmentExtensions []string
	if currentIndexTypesSettings.Enabled {
		currentIndexTypesSnapshot, snapshotErr :=
			runtimecomposition.LoadPinnedCurrentIndexTypesSnapshot()
		if snapshotErr != nil {
			return fmt.Errorf("load pinned current index-types snapshot: %w", snapshotErr)
		}
		attachmentExtensions = currentIndexTypesSnapshot.AttachmentExtensions()
		currentIndexTypes, err = indextypesapi.NewCurrentIndexTypesRoute(
			currentIndexTypesSnapshot,
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose current index-types route: %w", err)
		}
		logger.Info(
			"current index-types route enabled",
			"sdk_revision",
			currentIndexTypesSnapshot.SDKRevision(),
			"entry_count",
			currentIndexTypesSnapshot.EntryCount(),
		)
	}

	currentApplicationSkillsSettings, err :=
		currentApplicationSkillsConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load current application-skills settings: %w", err)
	}
	// Same correction, same reason, as the index-types gate above (#395). The
	// attached-skills route is the ONLY handler for its path now that the
	// prototype fallback is deleted, so a Form-only gate would leave an OIDC
	// install with a 404 where the published contract declares a list.
	if currentApplicationSkillsSettings.Enabled && !productionAuthenticationComposed(apiGroupAuth) {
		return errors.New("ELITEA_APPLICATION_SKILLS_ENABLED requires an authenticated deployment")
	}
	var currentApplicationSkills *applicationskillsapi.CurrentApplicationSkillsRoute
	if currentApplicationSkillsSettings.Enabled {
		currentApplicationSkillsRepository, repositoryErr :=
			applicationskillsapi.NewCurrentApplicationSkillsRepository(pool)
		if repositoryErr != nil {
			return fmt.Errorf(
				"compose current application-skills repository: %w",
				repositoryErr,
			)
		}
		currentApplicationSkills, err =
			applicationskillsapi.NewCurrentApplicationSkillsRoute(
				currentApplicationSkillsRepository,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
		if err != nil {
			return fmt.Errorf("compose current application-skills route: %w", err)
		}
		logger.Info("current application-skills route enabled")
	}

	currentConfigurationsConfig, err := currentConfigurationsConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load current Configurations settings: %w", err)
	}
	// The gate is the credential plane, NOT the FormGraph (gap G2).
	//
	// This used to read `formGraph == nil || principalValidator == nil ||
	// forwardedIdentityVerifier == nil`, and all three are assigned only
	// inside the `authEnabled` block. ELITEA_AUTH_CONFIG_FILE was therefore
	// the only way to satisfy it, so an install with real corporate single
	// sign-on — the shape deploy/helm/elitea/values-auth-minimal.yaml
	// describes — was refused the whole Configurations plane: no credential
	// create, no model catalogue, no project vector store. Every LLM setup
	// step fell back to deploy/scripts/seed-llm-api.py or raw SQL.
	//
	// productionAuthenticationComposed asks what the routes actually need: a
	// reader of the caller's credential plus a PrincipalValidator. A
	// deployment with NO authentication still fails it, because
	// apiGroupAuthConfig hands back the zero AuthConfig there.
	if currentConfigurationsConfig.Enabled && !productionAuthenticationComposed(apiGroupAuth) {
		return errors.New("ELITEA_CONFIGURATIONS_ENABLED requires an authenticated deployment")
	}
	// The optional runtime configuration is read here, ahead of the runtime
	// itself, because one Configurations-side decision depends on it: under
	// the Rust indexing runtime no project vector store is composed (below).
	// Its validation against the Configurations flags stays where the runtime
	// is composed.
	runtimeConfig, err := runtimecomposition.ConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load optional runtime configuration: %w", err)
	}
	var currentConfigurationsRoot *runtimecomposition.CurrentConfigurationsRuntime
	// The toolkit settings resolver the Inventory facade expands a source with.
	// Nil where the Configurations runtime is not composed — a deployment with
	// no vault — and the facade then mounts without source expansion rather
	// than failing to boot.
	var inventorySourceSettings *configurationapp.CurrentToolkitSettingsResolver
	// The platform default model (#6826). Declared as the interface the router
	// takes and assigned only from a non-nil service, so no typed nil reaches
	// the router's nil checks.
	var platformModelDefaults api.PlatformModelDefaults
	// The save-time credential gate for the toolkit write path (#613). It is
	// composed from the same Configurations graph agent-version freezing uses,
	// so it exists only where that graph does.
	//
	// DISCLOSED, and now SMALLER: an install that composes no Configurations
	// runtime still saves toolkit credential references unresolved. That used
	// to mean every Kubernetes install except values-standalone.yaml, because
	// deploy/helm/elitea/values.yaml stated ELITEA_CONFIGURATIONS_ENABLED
	// "false" and no OIDC-only install could state otherwise. The chart now
	// derives the flag from the deployment (see
	// elitea-main.configurationsEnabled in the chart's _helpers.tpl), so an
	// install with any authentication and a public project id composes the
	// graph. What is left is the shape with NO authentication.
	//
	// Composing a second graph from a second vault key source to close the
	// remainder would reproduce #399's one-key-source defect on the model
	// catalogue the resolver reads, which is a worse failure than the one it
	// would fix.
	var toolkitSettingsValidator v2toolkits.ToolkitSettingsValidator
	var delegatedAuthToolkitSettings *toolkitexecutionapp.CurrentDelegatedAuthToolkitSettings
	var currentConfigurationRead http.Handler
	var currentConfigurationAvailable http.Handler
	var currentConfigurationTypes http.Handler
	var currentConfigurationMutation http.Handler
	var currentModelCatalog http.Handler
	var currentModelDefault http.Handler
	var internalConfigurationTools *configurationapi.CurrentConfigurationToolHandler
	var configProviderAdmission configurationapi.ProviderAdmission
	// The resolve+unseal capability the STORED connection checks need
	// (internal/api/v2/configurations/stored_check.go). It composes here, with
	// the admission decision, because both read the same expander and the same
	// project vault this Configurations runtime owns, and a second vault with
	// a second key source is #399's defect.
	var configStoredResolver configurationapi.StoredConfigurationResolver
	var currentPromptContextReads *promptcontextreadsapi.CurrentRoutes
	if currentConfigurationsConfig.Enabled {
		currentConfigurationsRoot, err = runtimecomposition.NewCurrentConfigurationsRuntime(
			pool,
			currentConfigurationsConfig.PublicProjectID,
			currentConfigurationsConfig.VaultMasterKeyFile,
			currentConfigurationsConfig.VaultMasterKey,
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations services: %w", err)
		}
		defer currentConfigurationsRoot.Destroy()
		toolkitSettingsResolver, err := currentConfigurationsRoot.NewToolkitSettingsValidator(pool)
		if err != nil {
			return fmt.Errorf("compose toolkit settings validation: %w", err)
		}
		// Assigned through the concrete value, never a typed nil: the handler's
		// only fallback is a nil-interface check.
		toolkitSettingsValidator = toolkitSettingsResolver
		// The provenance twin (#1286): the same graph without the admin-vault
		// fallback, so the OAuth proxy can tell a global secret from a
		// project one before it binds an editor-written token endpoint.
		projectOwnedToolkitSettings, err := currentConfigurationsRoot.NewProjectOwnedToolkitSettingsResolver(pool)
		if err != nil {
			return fmt.Errorf("compose project-owned toolkit settings: %w", err)
		}
		delegatedAuthToolkitSettings, err = runtimecomposition.NewCurrentDelegatedAuthToolkitSettings(
			pool,
			toolkitSettingsResolver,
			projectOwnedToolkitSettings,
		)
		if err != nil {
			return fmt.Errorf("compose delegated authorization toolkit settings: %w", err)
		}
		// The SAME graph the Inventory facade claims a source toolkit's
		// credentials with. One composition, so what a source's own index runs
		// read is what the provider receives; a second would be a second answer
		// to "which credentials can this caller see".
		inventorySourceSettings = toolkitSettingsResolver
		// The SAME credential set the /api/v2 group authenticates with.
		//
		// These routes are registered on the ROOT mux by
		// mountReviewedProductionRoutes, so they carry their own AuthConfig
		// and inherit nothing from the group. This block used to build one
		// inline from formGraph, which is nil on an OIDC-only deployment.
		// Reusing apiGroupAuth keeps the two planes symmetric and, on
		// OIDC-only, keeps the personal access token working: the reviewed
		// read routes shadow the compatibility handler at the same patterns,
		// so an AuthConfig without a token validator would turn an SDK call
		// that answered 200 into a 401.
		//
		// The Form shape is byte-identical to the AuthConfig this block built
		// before: the graph as token validator, the production principal
		// validator, the forwarded-identity verifier, and the browser session
		// secret.
		//
		// The session secret is the browser's only credential (#292), same
		// reasoning as the agent-start route above. These are the
		// configuration reads the UI makes on every chat page — the model
		// catalogue among them — and without a session they answered 401 to
		// the product's own model picker, which then rendered empty. A user
		// could not choose a model, so the turn was rejected for not naming
		// one: a chat that cannot run, with every configuration row present
		// and correct.
		//
		// Reads only widen to a session; each route still resolves
		// permissions through currentPermissions below.
		currentAuth := apiGroupAuth
		currentPermissions := legacyrbac.NewPostgresResolver(pool)
		internalConfigurationTools = configurationapi.NewCurrentConfigurationToolHandler(
			currentConfigurationsRoot.Types(),
			currentConfigurationsRoot.ModelCatalog(),
			currentConfigurationsRoot.VaultWriter(),
			currentConfigurationsConfig.PublicProjectID,
		)
		currentConfigurationAvailable, err = configurationapi.NewCurrentAvailableRoute(
			currentConfigurationsRoot.AvailableCatalog(),
			currentAuth,
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations available route: %w", err)
		}
		currentConfigurationRead, err = configurationapi.NewCurrentConfigurationReadRoute(
			currentConfigurationsRoot.Reader(),
			currentConfigurationsConfig.PublicProjectID,
			currentAuth,
			currentPermissions,
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations read routes: %w", err)
		}
		currentConfigurationTypes, err = configurationapi.NewCurrentConfigurationTypesRoute(
			currentConfigurationsRoot.Types(),
			currentAuth,
			currentPermissions,
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations types route: %w", err)
		}
		currentModelCatalog, err = configurationapi.NewCurrentModelCatalogRoute(
			currentConfigurationsRoot.ModelCatalog(),
			currentConfigurationsConfig.PublicProjectID,
			currentAuth,
			currentPermissions,
			// Gateway catalogue prices on llm items, behind the budget routes'
			// can_see_amounts rule (#1227).
			configurationapi.WithModelPrices(dbrepos.NewModelPriceRepository(pool)),
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations model route: %w", err)
		}
		currentModelDefault, err = configurationapi.NewCurrentModelDefaultRoute(
			currentConfigurationsRoot.VaultWriter(),
			currentAuth,
			currentPermissions,
		)
		if err != nil {
			return fmt.Errorf("compose current Configurations model-default route: %w", err)
		}
		// The secrets handler is the one vault creator (#399). It creates the
		// public project's vault on a fresh install, where migrations made
		// the project without the provisioner.
		modelDefaults, modelDefaultsErr := currentConfigurationsRoot.NewPlatformModelDefaults(
			pool,
			v2secrets.NewHandler(pool),
		)
		if modelDefaultsErr != nil {
			return fmt.Errorf("compose the platform default model: %w", modelDefaultsErr)
		}
		if modelDefaults != nil {
			platformModelDefaults = modelDefaults
		}
		// The status_ok decision for the compatibility write routes (#457).
		//
		// Those routes are the write path every deployed stack serves: the
		// reviewed mutation route below needs
		// ELITEA_CONFIGURATIONS_MUTATION_ENABLED, and no deployment file sets
		// it. Their INSERT never named status_ok, so a saved credential kept
		// the column default of false, and the gateway admits only
		// status_ok = true. The decision composes here rather than with the
		// lifecycle because it needs no runtime plane and no mutation flag:
		// it reads the vault and the expander this Configurations runtime
		// already owns.
		providerAdmission, admissionErr := runtimecomposition.NewCurrentProviderAdmission(
			currentConfigurationsRoot,
			currentConfigurationsConfig.AllowProjectOwnLLMs,
		)
		if admissionErr != nil {
			return fmt.Errorf("compose current Configurations provider admission: %w", admissionErr)
		}
		// Assigned only when non-nil, for the reason spelled out at
		// configConnectionChecker below: boxing a nil pointer into the
		// interface would make the handler's nil test false and call a method
		// on a nil receiver instead of leaving the column alone.
		if providerAdmission != nil {
			configProviderAdmission = providerAdmission
		}
		storedResolver, storedResolverErr := runtimecomposition.NewCurrentStoredConfigurationResolver(
			currentConfigurationsRoot,
		)
		if storedResolverErr != nil {
			return fmt.Errorf("compose current stored configuration resolution: %w", storedResolverErr)
		}
		// Assigned only when non-nil, for the same reason as the two
		// dependencies around it: a nil POINTER boxed into this interface
		// makes the handler's nil test false, so the stored check would call a
		// method on a nil receiver instead of reporting itself unavailable.
		if storedResolver != nil {
			configStoredResolver = storedResolver
		}
		// No /llm data plane is composed here. This block used to build the
		// LiteLLM facade (an authenticated reverse proxy plus an administration
		// client holding the proxy's master key) whenever ELITEA_LITELLM_BASE_URL
		// was set. The Bifrost gateway replaced it: it resolves each project's
		// provider credentials and model definitions from
		// p_{projectID}.configuration itself, so Main has nothing to proxy on
		// its behalf beyond the mTLS gateway proxy composed on LLM_GATEWAY_URL
		// below, which is the sole /llm backend.
		// ELITEA_PROMPT_CONTEXT_READS_ENABLED used to gate a second composition
		// of promptcontextreadsapi.NewCurrentRoutes here, on the same reader
		// pair. It is deleted with its branch (#367).
		//
		// The branch was unreachable and redundant at the same time. It needed
		// this flag AND ELITEA_CONFIGURATIONS_ENABLED AND production auth, and
		// no deployment sets the flag — while the ungated composition below
		// builds the identical pair whenever FormGraph or an OIDC session
		// handler exists, which every deployment that could have satisfied the
		// flag also has. So the flag could only ever pre-empt a block that was
		// going to run anyway, and its absence changed nothing.
		//
		// Deleting it is not a behaviour change: with the flag unset, control
		// already reached the block below. What it removes is a second, subtly
		// different composition of the same routes — this one passed
		// currentAuth/currentPermissions, the one below passes
		// chatConfigAuthConfig(...) and a fresh legacyrbac resolver — that no
		// test and no deployment ever exercised.
		logger.Info("current Configurations services enabled", "public_project_id", currentConfigurationsConfig.PublicProjectID)
	}

	// The chat-config read has to be composable WITHOUT the Configurations
	// chain (#194). Its only registration used to sit inside the deleted
	// `ChatService` gate on the prototype eliteacore handler, and the current
	// implementation was then reachable only under
	// ELITEA_PROMPT_CONTEXT_READS_ENABLED, which no deployment set. So
	// `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` answered 404
	// everywhere for as long as that gate existed — while
	// `features/artifacts`' chatConfigApi has been querying it on every
	// artifacts page load and silently falling back to a 150 MB default.
	//
	// This block is now the ONLY composition of the pair. #367 deleted the
	// flagged one above, which needed a flag nothing set to pre-empt a block
	// that runs regardless.
	//
	// The reader needs nothing from the Configurations chain — only a
	// *pgxpool.Pool and the optional vault master key — so it is composed here,
	// exactly like the OIDC-only project-list and notification-event routes
	// above, and it stays composed here whether or not that chain is on.
	//
	// The claim this comment used to make, that ELITEA_CONFIGURATIONS_ENABLED
	// "is set in no deployment", was stale:
	// deploy/docker-compose.standalone-full.yml sets it. The claim mattered,
	// because it was the reason given for composing here rather than in the
	// chain; the reason above does not depend on it and holds either way.
	//
	// CurrentRoutes is an atomic pair, so the project-context read is
	// constructed alongside it. Which PATHS become reachable is decided at the
	// router: mountReviewedProductionRoutes registers the chat_config path
	// only, because that is #194's half and the project-context path is already
	// served by the prototype eliteacore handler — see production_router.go's
	// comment on why registering both would change that path's default.
	if currentPromptContextReads == nil && (formGraph != nil || oidcSessionHandler != nil) {
		// Reuse the Configurations runtime's loader when it exists, so a
		// deployment that DOES set ELITEA_VAULT_MASTER_KEY_FILE keeps reading
		// master-key-wrapped project keys. Without that chain the master-key
		// FILE cannot be expressed at all today (currentConfigurationsConfig
		// rejects it unless ELITEA_CONFIGURATIONS_ENABLED=true) — but
		// SECRETS_MASTER_KEY can, and usually is, so the loader built below
		// takes it. Only a deployment that sets neither gets the unwrapped
		// shape, which is what centry writes with no master key at all.
		chatConfigVaults := currentConfigurationsRoot.VaultLoader()
		if chatConfigVaults == nil {
			// SECRETS_MASTER_KEY, not nil. A nil key builds the UNWRAPPED
			// opener, and the secrets handler writes every project vault key
			// WRAPPED with that variable whenever it is set — so a nil here
			// reproduces, on this branch, the exact ErrInvalidProjectKey the
			// Configurations loader was just fixed for: an intact vault row
			// that will not open, and a chat configuration read that fails for
			// every project which has one.
			//
			// This branch is not hypothetical. deploy/docker-compose.yml
			// MANDATES SECRETS_MASTER_KEY and sets no Configurations flag, so
			// it is exactly the shape that lands here.
			//
			// The key is re-read rather than taken from `masterKey` above:
			// that one is the raw 32 bytes for the secrets handler, and this
			// loader validates the 44-character encoded form.
			chatConfigMasterKey, keyErr := encodedVaultMasterKeyFromEnv(os.LookupEnv)
			if keyErr != nil {
				return fmt.Errorf("compose ungated chat configuration vault loader: %w", keyErr)
			}
			ungated, vaultErr := storage.NewPostgresSecretVaultLoader(pool, chatConfigMasterKey)
			if vaultErr != nil {
				return fmt.Errorf("compose ungated chat configuration vault loader: %w", vaultErr)
			}
			defer ungated.Destroy()
			chatConfigVaults = ungated
		}
		chatConfigReader, readerErr :=
			promptcontextreadsapi.NewCurrentChatConfigVaultReader(chatConfigVaults)
		if readerErr != nil {
			return fmt.Errorf("compose ungated chat configuration reader: %w", readerErr)
		}
		projectContextReader, readerErr :=
			promptcontextreadsapi.NewCurrentProjectContextRepository(pool)
		if readerErr != nil {
			return fmt.Errorf("compose ungated project-context reader: %w", readerErr)
		}
		// apiGroupAuth, not a fresh composition from formGraph/
		// principalValidator/formSessions: those are the SAME
		// inputs apiGroupAuth was already built from above, and the gate on
		// this block (formGraph != nil || oidcSessionHandler != nil) makes
		// the two compositions identical in both branches. See
		// chatConfigAuthConfig for why reusing the value, rather than
		// recomposing it, is what keeps the OIDC-only branch from silently
		// losing a validator again (#301, and the token validator this fixes).
		currentPromptContextReads, err = promptcontextreadsapi.NewCurrentRoutes(
			chatConfigReader,
			projectContextReader,
			chatConfigAuthConfig(apiGroupAuth),
			legacyrbac.NewPostgresResolver(pool),
		)
		if err != nil {
			return fmt.Errorf("compose ungated chat configuration route: %w", err)
		}
		logger.Info("chat-config route enabled (ungated)")
	}

	// runtimeConfig was read above, before the Configurations runtime, so the
	// project vector-store decision could use it.
	if runtimeConfig.Enabled && (principalValidator == nil || formSessions == nil) {
		return errors.New("ELITEA_RUNTIME_ENABLED requires production authentication")
	}
	if err := validateRuntimeComposition(currentConfigurationsConfig, runtimeConfig); err != nil {
		return err
	}
	// The ONE native policy reader, built here because the /llm edge's
	// execution verifier and the workspace handoff need it too: it stops
	// attributing an uncommitted local turn the moment `local_work.allowed`
	// is turned off, and a save of local_work_cloud_handoff reaches the next
	// send.
	nativePolicy := native.policy(pool)
	// The desktop ⇄ cloud handoff (ADR-0034), composed BEFORE the runtime
	// plane: the cloud executor's adapters (workspaceExecutor) are runtime
	// dependencies. Its turn canceller is the runtime's own stop use case,
	// bound once the runtime is composed (workspaceTurnCanceller.bind).
	workspaceTurnCanceller := &deferredAgentCanceller{}
	var workspaceBus events.Bus
	if liveNATSBus != nil {
		workspaceBus = liveNATSBus
	}
	// Conversation activity frames (client contract 1.12) on the same
	// live-update plane: the runtime plane publishes the turn transitions,
	// the read route the read markers. nil without the plane.
	conversationActivity := newConversationActivity(workspaceBus, pool, logger)
	// Assigned only from a composed runtime, never a typed nil.
	var workspaceForgeExpander forgeExpander
	if currentConfigurationsRoot != nil {
		if expander := currentConfigurationsRoot.Expansion(); expander != nil {
			workspaceForgeExpander = expander
		}
	}
	workspaceHandoff, err := composeWorkspaceHandoff(ctx, pool, objectStore, workspaceInputs, nativePolicy,
		workspaceTurnCanceller, auditRecorder, apiGroupAuth, newHandoffLiveEvents(workspaceBus), workspaceForgeExpander, logger)
	if err != nil {
		return fmt.Errorf("compose workspace handoff routes: %w", err)
	}
	workspaceExecutor, err := composeWorkspaceExecutor(pool, workspaceHandoff, workspaceInputs.Resources, logger)
	if err != nil {
		return fmt.Errorf("compose the cloud executor adapters: %w", err)
	}
	var runtimeRoot *runtimecomposition.Runtime
	var productionRuntime *api.ProductionRuntimeRoutes

	// The DeepWiki facade (ADR-0022 P2). Composed OUTSIDE the runtime-plane
	// block: it proxies to an independently deployed service and needs nothing
	// the runtime provides, so tying it to ELITEA_RUNTIME_ENABLED would make a
	// deployment turn on the agent runtime to get a wiki.
	//
	// An enabled-but-unreachable configuration is a startup failure, not a
	// degraded mount. The alternative is four routes that answer 503 to every
	// caller while the operator's flag says the feature is on.
	// The SECOND provider facade. Composed the same way as the first and in
	// far fewer lines, because providerhost/{facade,proxy,spi} carry what the
	// two have in common — which is what ADR-0012's budget measured.
	// The admission posture, read BEFORE either facade and unconditionally.
	// An unrecognised spelling is a boot failure whether or not a provider is
	// enabled today: an operator who mistyped it must learn now, and not at
	// the moment they turn a provider on.
	admissionPosture, err := facade.AdmissionPostureFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("read provider admission posture: %w", err)
	}

	var inventoryRoute *v2inventory.Route
	inventoryConfig, err := facade.ConfigFromEnv(v2inventory.EnvNames, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("read Inventory facade configuration: %w", err)
	}
	if inventoryConfig.Enabled {
		// One hop to the provider, shared by the registrar and the route:
		// one connection and one certificate reloader, not two.
		inventoryHop, hopErr := providerproxy.New(inventoryConfig, v2inventory.EnvNames.Target, logger)
		if hopErr != nil {
			return fmt.Errorf("build the Inventory facade: %w", hopErr)
		}
		// The registrar starts FIRST: it is what learns the provider's own
		// name, and the gate the route carries reads that name per request.
		inventoryConfig.Admission = providerAdmissionGate(logger, pool,
			currentConfigurationsConfig.PublicProjectID, admissionPosture,
			startProviderRegistrar(ctx, logger, pool, currentConfigurationsConfig.PublicProjectID,
				"inventory", inventoryConfig, v2inventory.EnvNames.Target, inventoryHop.Client()))
		// The source expander (ADR-0023 H4c I2). Built only where the
		// Configurations graph that can claim a credential exists (the engine
		// calls back over the platform gRPC listener, at no configured origin,
		// since #1359 U5). Where it is absent the facade mounts WITHOUT expansion — the eight tools that
		// read the graph still work — rather than failing to boot, which is
		// what a stack with no vault (the E2E one) needs. What is NOT
		// tolerated is a half-wired expander: NewSources refuses that.
		var inventorySources *v2inventory.Sources
		inventorySourcesConfig, sourcesErr := v2inventory.SourcesFromEnv(os.LookupEnv)
		if sourcesErr != nil {
			return fmt.Errorf("read Inventory source configuration: %w", sourcesErr)
		}
		if inventorySourceSettings == nil {
			logger.Warn("Inventory mounts without source expansion",
				"settings_resolver", false)
		} else {
			inventoryToolkits, toolkitsErr := dbrepos.NewCurrentToolkitsRepository(pool)
			if toolkitsErr != nil {
				return fmt.Errorf("compose Inventory toolkit reader: %w", toolkitsErr)
			}
			// The signer must be the one THIS deployment's validator reads
			// back, and the rule for that is "the form graph when there is
			// one" — the same rule the DeepWiki minter below applies.
			var inventorySigner v2auth.TokenSigner
			if formGraph != nil {
				inventorySigner = formGraph
			}
			inventoryTokens, minterErr := v2auth.NewCallbackTokenMinter(
				pool, inventorySigner, []byte(os.Getenv("APPLICATION_SECRET_KEY")))
			if minterErr != nil {
				return fmt.Errorf("compose Inventory callback token minter: %w", minterErr)
			}
			inventoryGrants := dbrepos.NewCallbackTokenGrants(pool)
			// Every Inventory bearer carries a recorded invocation grant
			// (shared/0159) naming the provider, for elitea-vector's token
			// introspection (ADR-0031); investigate's source-tool grant
			// upgrades it in place.
			inventoryMinter, grantingErr := material.GrantingMinter(
				material.NewCallbackMinter(inventoryTokens), inventoryGrants, "inventory")
			if grantingErr != nil {
				return fmt.Errorf("compose Inventory callback token minter: %w", grantingErr)
			}
			inventorySources, err = v2inventory.NewSources(
				inventoryToolkits, inventorySourceSettings,
				inventoryMinter,
				// What investigate's bearer was minted for: the record the
				// test_tool source-tool gate reads (shared/0159).
				inventoryGrants, inventorySourcesConfig)
			if err != nil {
				return fmt.Errorf("compose Inventory source expander: %w", err)
			}
		}
		inventoryRoute, err = v2inventory.NewRoute(
			inventoryConfig,
			// The API group's credential set, in either authentication mode.
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
			inventorySources,
			logger,
			providerproxy.WithHop(inventoryHop),
		)
		if err != nil {
			return fmt.Errorf("build the Inventory facade: %w", err)
		}
	}

	var deepwikiRoute *v2deepwiki.Route
	deepwikiConfig, err := v2deepwiki.ConfigFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("read DeepWiki facade configuration: %w", err)
	}
	if deepwikiConfig.Enabled {
		// The credential resolver and the callback minter. Both are built
		// here rather than inside the route because both are database-backed
		// and the facade's own tests must not need a database to exercise a
		// body rewrite.
		//
		// The vault loader comes from the Configurations runtime when there is
		// one, and from ELITEA_VAULT_MASTER_KEY_FILE / SECRETS_MASTER_KEY when
		// there is not — the same fallback the chat-config reader makes above,
		// and for the same reason: a loader built from a key source the
		// deployment does not use opens no vault at all, and reports it as a
		// configuration that will not decrypt rather than as a misconfigured
		// key.
		deepwikiVaults := currentConfigurationsRoot.VaultLoader()
		if deepwikiVaults == nil {
			deepwikiMasterKey, keyErr := encodedVaultMasterKeyFromEnv(os.LookupEnv)
			if keyErr != nil {
				return fmt.Errorf("compose DeepWiki vault loader: %w", keyErr)
			}
			ungated, vaultErr := storage.NewPostgresSecretVaultLoader(pool, deepwikiMasterKey)
			if vaultErr != nil {
				return fmt.Errorf("compose DeepWiki vault loader: %w", vaultErr)
			}
			defer ungated.Destroy()
			deepwikiVaults = ungated
		}
		deepwikiUnsecreter, unsecreterErr := storage.NewCurrentVaultUnsecreter(deepwikiVaults)
		if unsecreterErr != nil {
			return fmt.Errorf("compose DeepWiki unsecreter: %w", unsecreterErr)
		}
		deepwikiConfigurations, configurationsErr := dbrepos.NewCurrentConfigurationsRepository(pool)
		if configurationsErr != nil {
			return fmt.Errorf("compose DeepWiki configuration reader: %w", configurationsErr)
		}
		deepwikiCredentials, credentialsErr := v2deepwiki.NewCredentialResolver(
			deepwikiConfigurations, deepwikiUnsecreter, deepwikiConfig.GitEgress)
		if credentialsErr != nil {
			return fmt.Errorf("compose DeepWiki credential resolver: %w", credentialsErr)
		}
		// patSigner is declared further down, next to the router config it
		// feeds. Recomputed here from the same rule rather than moved: the
		// signer must be the one THIS deployment's validator reads back, and
		// the rule for that is "the form graph when there is one".
		var deepwikiSigner v2auth.TokenSigner
		if formGraph != nil {
			deepwikiSigner = formGraph
		}
		deepwikiTokens, minterErr := v2auth.NewCallbackTokenMinter(
			pool, deepwikiSigner, []byte(os.Getenv("APPLICATION_SECRET_KEY")))
		if minterErr != nil {
			return fmt.Errorf("compose DeepWiki callback token minter: %w", minterErr)
		}
		// Every DeepWiki bearer carries a recorded grant (shared/0159) naming
		// the provider: elitea-vector's token introspection (ADR-0031) admits
		// the engine's vector writes and reads only with one.
		deepwikiMinter, minterErr := material.GrantingMinter(
			v2deepwiki.NewAuthCallbackMinter(deepwikiTokens),
			dbrepos.NewCallbackTokenGrants(pool), "deepwiki")
		if minterErr != nil {
			return fmt.Errorf("compose DeepWiki callback token minter: %w", minterErr)
		}

		// Registered before the route is built, for the reason the Inventory
		// facade gives above: the gate needs the provider's own name, and
		// only the registrar ever learns it.
		// One hop to the provider, shared by the registrar and the route, as
		// for Inventory above.
		deepwikiHop, hopErr := providerproxy.New(deepwikiConfig.HopConfig(), v2deepwiki.TargetEnv, logger)
		if hopErr != nil {
			return fmt.Errorf("compose DeepWiki facade: %w", hopErr)
		}
		deepwikiConfig.Admission = providerAdmissionGate(logger, pool,
			currentConfigurationsConfig.PublicProjectID, admissionPosture,
			startProviderRegistrar(ctx, logger, pool, currentConfigurationsConfig.PublicProjectID,
				"deepwiki", deepwikiConfig.HopConfig(), v2deepwiki.TargetEnv, deepwikiHop.Client()))

		// The application-toolkit reader, for the `wikis_query` rewrite: a
		// query toolkit names a Wikis toolkit, and its OWN code toolkit is
		// what gets expanded (internal/api/v2/deepwiki/wikis.go). A reader
		// that will not build is not fatal — the facade then serves `Wikis`
		// and `wiki_query` and refuses `wikis_query` with a message saying
		// why, which is better than refusing to boot a DeepWiki deployment
		// that uses neither.
		deepwikiToolkits, toolkitsErr := dbrepos.NewCurrentToolkitsRepository(pool)
		if toolkitsErr != nil {
			logger.Warn("DeepWiki mounts without wikis_query expansion",
				"error", toolkitsErr)
			deepwikiToolkits = nil
		}
		// Assigned through a nil-INTERFACE, never a typed nil: the rewriter's
		// only fallback is a nil-interface check, and a typed nil would pass
		// it and then panic on the first wikis_query invoke.
		var deepwikiToolkitReader v2deepwiki.ToolkitReader
		if deepwikiToolkits != nil {
			deepwikiToolkitReader = deepwikiToolkits
		}

		// The wiki chat's transcript store. A deployment that cannot build it
		// still mounts DeepWiki: the chat works exactly as it did before the
		// history existed, and the drawer's listing simply comes back empty.
		// Recording is a feature of the chat, not a precondition for it.
		deepwikiHistoryRepo, historyErr := dbrepos.NewDeepWikiHistoryRepo(pool)
		if historyErr != nil {
			logger.Warn("DeepWiki mounts without server-side chat history",
				"error", historyErr)
		}
		// Through a nil INTERFACE, never a typed nil — the same rule the
		// toolkit reader above follows, and for the same reason: NewHistory
		// decides on `store == nil`, and a typed nil would pass that check
		// and then call methods on a nil pointer.
		var deepwikiHistoryStore wikichat.Store
		if deepwikiHistoryRepo != nil {
			deepwikiHistoryStore = deepwikiHistoryRepo
		}

		deepwikiRoute, err = v2deepwiki.NewRoute(
			deepwikiConfig,
			// The API group's credential set, in either authentication mode.
			apiGroupAuth,
			legacyrbac.NewPostgresResolver(pool),
			deepwikiCredentials,
			deepwikiToolkitReader,
			deepwikiMinter,
			slog.Default(),
			v2deepwiki.WithHistory(
				v2deepwiki.NewHistory(deepwikiHistoryStore, slog.Default())),
			v2deepwiki.WithHop(deepwikiHop),
		)
		if err != nil {
			return fmt.Errorf("compose DeepWiki facade: %w", err)
		}
	}
	// The application engines' platform services (#1243, #1244): the gRPC
	// clients that delete a project's (and a toolkit's) data in DeepWiki and
	// Inventory, read beside the two facades whose mTLS material they present.
	// They are shared by the project provisioner's cleanup journal (one step
	// each) and the toolkit cleanup journal (one row per Inventory toolkit),
	// through RouterConfig.ApplicationData.
	applicationData, closePlatformOperations, err := composePlatformOperations(
		logger, deepwikiConfig, inventoryConfig, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("compose application platform operations: %w", err)
	}
	defer closePlatformOperations()
	// elitea-vector's gRPC client (ADR-0031 decision 4): the two platform
	// deletions in the vector store — a toolkit index's namespace (the index
	// delete route and the registry's tombstone sweeper, through the runtime
	// composition's IndexVectorDeleter) and a deleted project's every
	// namespace (one cleanup-journal step, RouterConfig.ProjectVectorData).
	// Presents its own runtime-CA client certificate, not a facade's. nil
	// without ELITEA_VECTOR_GRPC_TARGET; see vector_client_config.go.
	vectorClient, closeVectorClient, err := composeVectorClient(
		logger, os.LookupEnv, runtimeConfig.RustIndexing())
	if err != nil {
		return fmt.Errorf("compose elitea-vector client: %w", err)
	}
	defer closeVectorClient()
	// Through nil INTERFACES, never a typed nil: both consumers decide on
	// `!= nil`, and a typed-nil *Client would pass and then dereference.
	var indexVectorDeleter indexingapp.IndexVectorDeleter
	var projectVectorData projectprovisioning.ApplicationDataRemover
	// The memory toolkit's cleanup (a toolkit's namespace, a user's personal
	// memories) rides the same client; a nil INTERFACE without one.
	var memoryData api.MemoryData
	if vectorClient != nil {
		indexVectorDeleter = vectorClient
		projectVectorData = vectorClient
		memoryData = vectorClient
	}
	// The vector step is recorded on a project delete by every replica that
	// indexes into the store (Rust indexing), with or without a client; a
	// replica without index ingest dispatch only drains. No replica closes a
	// recorded step on its own configuration: only the operator's abandon
	// switch does (RouterConfig.ProjectVectorCleanupAbandoned), which is
	// refused beside a client. A replica that runs Inventory records it only
	// beside a client (projectVectorStepRecorded).
	vectorStepRecorded := projectVectorStepRecorded(
		runtimeConfig.RustIndexing(), api.HoldsInventory(applicationData), vectorClient != nil)
	vectorCleanupAbandoned, err := vectorCleanupAbandonSwitch(logger, os.LookupEnv, vectorClient != nil)
	if err != nil {
		return err
	}
	// A deleted project's legacy PgVector database (#1359): dropped through
	// the old store's admin URL, or closed under the operator's retired switch
	// (legacy_pgvector_config.go). Through a nil INTERFACE without a URL.
	legacyPgvectorDropper, legacyPgvectorRetired, err := composeLegacyPgvector(logger, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("compose legacy PgVector cleanup: %w", err)
	}
	var legacyVectorDatabases projectprovisioning.LegacyVectorDatabases
	if legacyPgvectorDropper != nil {
		legacyVectorDatabases = legacyPgvectorDropper
	}
	// A deleted Inventory toolkit's graph vectors (#1359): the toolkit cleanup
	// journal deletes the graph's namespace in elitea-vector after the engine
	// deleted its rows (api.WithInventoryGraphVectors). A nil INTERFACE without
	// a client, never a typed nil, and then the Inventory client is left as it
	// is: there is no vector leg to wait for.
	var inventoryGraphVectors api.InventoryGraphVectors
	if vectorClient != nil {
		inventoryGraphVectors = vectorClient
	}
	applicationData = api.WithInventoryGraphVectors(applicationData, inventoryGraphVectors)
	// The same endpoint serves every SDK toolkit type's settings schema and its
	// metadata — the label, categories and icon the create page groups it by.
	// This is a second pinned file rather than a larger first one: the argument
	// schemas already measure 596 KB against a 1 MiB ceiling.
	toolkitCatalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		return fmt.Errorf("load pinned current toolkit catalogue snapshot: %w", err)
	}
	// Which of those types the deployment can actually run depends on the
	// worker: the Rust worker (the only one) materializes a subset of the
	// catalogue's families. A type the worker cannot run is served hidden,
	// with the reason, so the operator can see why rather than hunting a tile
	// that is simply absent.
	workerImplementation := runtimecomposition.RustWorkerImplementation
	workerToolkitCapability, err := runtimecomposition.LoadPinnedWorkerToolkitCapability(
		workerImplementation,
	)
	if err != nil {
		return fmt.Errorf("load pinned worker toolkit capability: %w", err)
	}
	// Under the Rust indexing runtime the Rust worker serves the index tools
	// inside the I1 indexing families (ADR-0030 I1), which the pinned
	// snapshot cannot express: the same image serves them under one setting
	// and not the other. The decision is the runtime configuration's
	// RustIndexing() — the same one the composition root keys the registry,
	// the catalog freeze and the vector store on — so the catalogue can never
	// offer a tool the runtime does not serve.
	workerToolkitCapability = workerToolkitCapability.ForIndexingRuntime(runtimeConfig)
	logger.Info(
		"toolkit type catalogue composed",
		"worker_implementation", workerImplementation,
		"catalogued_types", toolkitCatalogue.EntryCount(),
	)
	// Both are loaded HERE, ahead of the runtime, rather than beside the router
	// config they also feed: the tool-run producer composed inside the runtime
	// block below asks the SAME pair whether a type is runnable, and a second
	// answer to that question is how a deployment would offer a type it then
	// refuses. Loading is pure — two embedded snapshots — so moving it earlier
	// changes nothing but the order.

	var currentIndexStart http.Handler
	var currentAgentStart http.Handler
	// The support assistant's half of the agent-execution wiring. It is
	// assigned ONLY inside the `publicRoutes.AgentStart != nil` guard below:
	// assigning a nil concrete value to an interface variable yields a
	// non-nil interface holding a nil pointer, and the router's `!= nil` check
	// would then wire a use case whose first call panics — the typed-nil trap
	// this service has already been bitten by on /healthz.
	var supportAssistantStart v2support.StartUseCase
	// The MCP server's half of the same wiring, assigned under the same guard
	// and for the same typed-nil reason. `tools/call` runs an agent through the
	// SAME use case; left nil it keeps answering the refusal it always has.
	var mcpAgentStart v2mcp.AgentStartUseCase
	var mcpToolkitExecute v2mcp.ToolkitExecuteReadUseCase
	// The tool-run halves, assigned ONLY inside the guard below and for the
	// same typed-nil reason: a nil concrete value in a non-nil interface reads
	// as "configured" downstream, and both consumers decide on `!= nil`.
	var toolkitToolRun toolkitrun.UseCase
	var toolkitDiscovery discovery.UseCase
	// The desktop's resolved definition (ADR-0029 decision 5a), assigned only
	// under the same guard so an absent agent plane leaves a NIL interface.
	var clientApplicationVersions desktopopsapi.ResolvedVersionUseCase
	// Bound by the router to the MCP server it builds: a platform MCP tool of a
	// local turn runs through the same dispatch a cloud turn's does, and the
	// resolved definition names each platform server's tools from the same
	// listing (issue 1231).
	platformMCPTools := desktopopsapi.NewPlatformToolBridge()
	// The upload-time attachment extraction warm-up (issue 1062), assigned
	// only under the same guard so an absent reader leaves a NIL interface.
	var attachmentExtractionWarmer v2convs.AttachmentExtractionWarmer
	// The remote toolkit call's authorizer: the SAME service, under the same
	// guard, so an absent agent plane leaves a NIL interface and the route
	// answers 501.
	var remoteToolAuthorizer desktopopsapi.RemoteToolAuthorizer
	// The cloud child turns of a local turn (issue 1239): the chat start use
	// case and the same version service, under the same guards and for the
	// same typed-nil reason.
	var childAgentStart desktopopsapi.ChildAgentStarter
	var childAgentAuthorizer desktopopsapi.ChildAgentAuthorizer
	var mcpToolkitRun v2mcp.ToolkitRunUseCase
	// The unattended pipeline entry points (issues 192, 193).
	//
	// ONE handler serves both the HTTP surface and the platform scheduler's
	// `pipeline.schedule.scan.v1` job, which is why it is built here rather
	// than inside the router: two handlers over one table would be two chances
	// for the tick and the settings routes to disagree on a dependency.
	var pipelineTriggers *v2pipelinetriggers.Handler
	var currentNodeRecovery http.Handler
	var currentExecutionInterrupts http.Handler
	var currentAgentCancel http.Handler
	var currentAgentSteering http.Handler
	var currentApplicationTask http.Handler
	var currentIndexCancel http.Handler
	var currentIndexMeta http.Handler
	var currentIndexMetaDelete http.Handler
	var currentIndexConfiguration http.Handler
	var currentIndexScheduleUpdate http.Handler
	var currentIndexScheduleDelete http.Handler
	if runtimeConfig.Enabled {
		databasePoolLimits, limitsErr := runtimecomposition.DatabasePoolLimitsFromEnv(os.LookupEnv)
		if limitsErr != nil {
			return fmt.Errorf("load runtime database pool limits: %w", limitsErr)
		}
		runtimePools, openErr := openRuntimeDatabasePools(ctx, dbDSN, databasePoolLimits)
		if openErr != nil {
			return openErr
		}
		defer runtimePools.Close()
		if len(runtimeConfig.VectorIntrospectionClients) > 0 {
			if err := runtimePools.openVectorIntrospection(ctx, dbDSN, databasePoolLimits, openRuntimePostgresPool); err != nil {
				return err
			}
		}
		compiledState, compiledStateErr := openCompiledSnapshotStatePool(ctx, runtimeConfig)
		if compiledStateErr != nil {
			return compiledStateErr
		}
		if compiledState.close != nil {
			defer compiledState.close()
		}
		codeConsumers, codeConsumersErr := openCodeConsumerStartup(ctx, runtimeConfig, compiledState)
		if codeConsumersErr != nil {
			return codeConsumersErr
		}
		defer codeConsumers.Close()
		var configurationLifecycleReconciler configurationapp.CurrentConfigurationLifecycleReconciler
		if currentConfigurationsConfig.MutationEnabled {
			// No LLM runtime here: the lifecycle is database-side. It once
			// pushed every credential and model into the LiteLLM proxy, which
			// meant configuration mutation could not run without that proxy.
			// The Bifrost gateway pulls the same configuration rows, so the
			// lifecycle now only resolves references and writes status_ok.
			configurationLifecycleReconciler, err = runtimecomposition.NewCurrentConfigurationLifecycleReconciler(
				runtimePools.Control,
				currentConfigurationsRoot,
				currentConfigurationsConfig.AllowProjectOwnLLMs,
			)
			if err != nil {
				return fmt.Errorf("compose current Configurations lifecycle: %w", err)
			}
		}
		workspaceEgress, egressErr := loadWorkspaceEgress(os.Getenv, logger)
		if egressErr != nil {
			return fmt.Errorf("compose the workspace egress proxy: %w", egressErr)
		}
		// formGraph is a *FormGraph that stays nil on an OIDC-only
		// deployment. Boxed straight into the three interfaces below it
		// would make each of them non-nil around a nil pointer, so the
		// composition's nil checks would pass and the first token call
		// would panic. Hand over real nils instead.
		var (
			runtimeActorTokens   storage.ActorTokenIssuer
			runtimeProjectTokens storage.ProjectTokenValidator
			runtimeSystemTokens  runtimecomposition.ProjectSystemTokenSource
		)
		if formGraph != nil {
			runtimeActorTokens, runtimeProjectTokens, runtimeSystemTokens = formGraph, formGraph, formGraph
		}
		runtimeRoot, err = runtimecomposition.New(ctx, runtimeConfig, runtimecomposition.Dependencies{
			AdmissionPool:                    runtimePools.Admission,
			CredentialProfile:                credentialAdmission,
			CredentialBroker:                 runtimeCredentialBroker,
			SecurityInvalidations:            securityInvalidations,
			ExecutionSuspensions:             executionSuspensions.gate,
			IndexScheduleMandates:            governedTriggers.IndexMandates,
			IndexScheduleMandateGate:         governedTriggers.IndexGate,
			ControlPool:                      runtimePools.Control,
			OutputPool:                       runtimePools.Output,
			ReplayPool:                       runtimePools.Replay,
			TerminalEffectsPool:              runtimePools.TerminalEffects,
			ContentPool:                      runtimePools.Content,
			VectorIntrospectionPool:          runtimePools.VectorIntrospection,
			CompiledSnapshotStatePool:        compiledState.pool,
			CodeWorkspaceCapabilities:        codeConsumers.workspaceCapabilities,
			CodeWorkspacePolicy:              codeConsumers.workspacePolicy,
			CodePlatform:                     codeConsumers.platform,
			CodeBrokerPolicies:               codeConsumers.brokerPolicies,
			CodeDebugStatePool:               codeConsumers.debugStatePool,
			CurrentConfigurations:            currentConfigurationsRoot,
			ConfigurationLifecycleReconciler: configurationLifecycleReconciler,
			ActorTokenIssuer:                 runtimeActorTokens,
			ProjectTokenValidator:            runtimeProjectTokens,
			ProjectSystemTokenSource:         runtimeSystemTokens,
			RunTokens:                        runTokens.Issuer,
			// elitea-vector's token introspection (ADR-0031) reads callback
			// grants over the main pool, where the minting facades write them.
			CallbackTokenFacts: dbrepos.NewCallbackTokenGrants(pool),
			// elitea-vector's Delete for the registry (ADR-0031 decision 4);
			// nil keeps the deferred hook and an idle sweeper.
			IndexVectorDeleter:      indexVectorDeleter,
			PermissionResolver:      legacyrbac.NewPostgresResolver(pool),
			Logger:                  logger,
			ObjectStore:             objectStore,
			ToolkitCatalogue:        toolkitCatalogue,
			WorkerToolkitCapability: workerToolkitCapability,
			// pipeline.run.succeeded/failed — see
			// runtimecomposition.Dependencies.PipelineRuns' own doc comment.
			PipelineRuns: pipelineRunsRepo,
			DomainEvents: domainEvents,
			// The cloud executor (ADR-0034): all four adapters when the
			// workspace handoff is composed, none otherwise; and how a project
			// host allowlist is enforced (decision 9, the egress proxy and its
			// run-token signing key).
			Workspace: workspaceExecutor.dependencies(workspaceEgress),
			// The egress proxy's per-turn summaries (ELITEA_EGRESS_AUDIT_CLIENTS)
			// go to the same audit trail as the API's.
			AuditRecorder: auditRecorder,
			// ...and their per-turn summaries live in the database, so every
			// Main replica merges the same rows and one writes the event.
			EgressAuditStore:     dbrepos.NewEgressRunSummaries(pool),
			ConversationActivity: conversationActivity,
		})
		if err != nil {
			return fmt.Errorf("compose optional runtime: %w", err)
		}
		defer func() {
			if err := runtimeRoot.Close(); runErr == nil && err != nil {
				runErr = fmt.Errorf("close optional runtime: %w", err)
			}
		}()
		if currentConfigurationsConfig.MutationEnabled {
			mutationService, mutationErr := currentConfigurationsRoot.NewMutationService(
				runtimeRoot.CurrentSDKConfigurationValidator(),
			)
			if mutationErr != nil {
				return fmt.Errorf("compose current Configurations mutation service: %w", mutationErr)
			}
			currentConfigurationMutation, mutationErr = configurationapi.NewCurrentConfigurationMutationRoute(
				mutationService,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if mutationErr != nil {
				return fmt.Errorf("compose current Configurations mutation routes: %w", mutationErr)
			}
		}
		publicRoutes := runtimeRoot.PublicRoutes()
		productionRuntime, err = api.NewProductionRuntimeRoutes(
			publicRoutes.Validation,
			publicRoutes.ExecutionEvents,
			principalValidator,
			// The GROUP's own credential set, handed over whole. A browser's
			// EventSource sends a cookie and nothing else (#93), and an SDK or
			// a server-side client follows the run it started with the SAME
			// personal access token it started it with (#289). Both are
			// accepted here on exactly the terms they are accepted on every
			// other /api/v2 route — including the server-side session store,
			// which the literal that used to stand here did not carry, and the
			// token validator, which NewProductionRuntimeRoutes used to drop.
			//
			// A deployment with no authentication plane hands over the zero
			// AuthConfig, whose SessionSecret is empty. That is the same
			// nothing the whole /api/v2 group admits there, rather than a
			// cookie honoured on one route and refused on every other.
			apiGroupAuth,
		)
		if err != nil {
			return fmt.Errorf("compose production runtime HTTP routes: %w", err)
		}
		// Every route below takes apiGroupAuth — the runtime routes the WEB APP
		// calls directly (#93 Surface A: the index list, an index run and its
		// cancel, and the chat stop button), and the index-schedule writes.
		// Each was once composed for forwarded identity alone, so every one of
		// them answered `401 missing authorization header` to the product's own
		// UI while working for the worker — the same shape as #291 on the chat
		// start route, and the reason Surface A's REST path could not be
		// exercised from a browser at all.
		//
		// Additive: the peer verifier and principal validator come with the
		// same value, so the worker and the auth edge authenticate
		// exactly as before, and each route still resolves permissions through
		// the RBAC resolver it is given.
		//
		// This block does NOT gate on formGraph, so a private literal reading
		// `Validator: formGraph` here boxed a nil *FormGraph into a non-nil
		// interface on every OIDC-only deployment — the #86 typed-nil trap,
		// where `!= nil` downstream reads as "configured". apiGroupAuth cannot
		// carry that: apiGroupAuthConfig picks the branch by testing the
		// pointer.
		// Standalone toolkit operations also run on the Rust agent stream.
		// Index ingestion can remain disabled for that deployment.
		toolkitDiscovery = publicRoutes.ToolkitDiscovery
		toolkitToolRun = publicRoutes.ToolkitCallTool
		if publicRoutes.ClientApplicationVersions != nil {
			publicRoutes.ClientApplicationVersions.WithPlatformTools(platformMCPTools)
			clientApplicationVersions = publicRoutes.ClientApplicationVersions
			remoteToolAuthorizer = publicRoutes.ClientApplicationVersions
			childAgentAuthorizer = publicRoutes.ClientApplicationVersions
		}
		if publicRoutes.AttachmentExtraction != nil {
			attachmentExtractionWarmer = publicRoutes.AttachmentExtraction
		}
		mcpToolkitRun = publicRoutes.ToolkitCallTool
		if publicRoutes.IndexStart != nil {
			if publicRoutes.ToolkitCallTool != nil {
				// The SAME path, the same credentials, the same permission —
				// plus the synchronous branch its `await_response` refusal used
				// to occupy (#340). This route is what a tool run actually
				// reaches wherever the runtime is composed; see
				// internal/application/toolkitcalltool/doc.go.
				currentIndexStart, err = indexingapi.NewCurrentIndexStartRouteWithToolRuns(
					publicRoutes.IndexStart,
					publicRoutes.ToolkitCallTool,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			} else {
				currentIndexStart, err = indexingapi.NewCurrentIndexStartRoute(
					publicRoutes.IndexStart,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			}
			if err != nil {
				return fmt.Errorf("compose current index-start route: %w", err)
			}
		}
		if publicRoutes.AgentStart != nil {
			supportAssistantStart = publicRoutes.AgentStart
			mcpAgentStart = publicRoutes.AgentStart
			childAgentStart = publicRoutes.AgentStart
			// The pipeline trigger and schedule handler. It runs pipelines
			// through the SAME use case as chat, MCP and the support widget —
			// see internal/api/v2/pipelinetriggers for why there is one
			// admission path and not four.
			pipelineTriggers = v2pipelinetriggers.NewPlatformHandler(
				pool,
				publicRoutes.AgentStart,
				v2secrets.NewHandler(pool),
				legacyrbac.NewPostgresResolver(pool),
				auditRecorder,
				logger,
				domainEvents,
				pipelineRunsRepo,
				v2pipelinetriggers.WithGovernedRuns(governedTriggers.Runs),
			)
			// The schedule TICK. It rides elitea-main's platform scheduler —
			// the same framework `index.schedule.scan.v1` uses — so the clock,
			// the cross-replica occurrence lease and the claim fence are the
			// framework's and are not re-implemented. See the package doc for
			// why the tick is not in services/elitea-scheduler, and what was
			// carried across from it (the maintenance gate and the
			// no-catch-up-storm rule, both ported literally).
			if scheduleErr := startPipelineScheduleRunner(
				ctx, runtimePools.Admission, pipelineTriggers,
				runtimeConfig.SchedulerInstanceID, logger,
			); scheduleErr != nil {
				return fmt.Errorf("compose pipeline schedule job: %w", scheduleErr)
			}
			// The server's hand-off of leftover steering messages: it starts
			// the next turn through the same use case as the chat composer.
			if publicRoutes.Steering != nil {
				if handOffErr := startSteeringHandOff(
					ctx, runtimePools.Admission, publicRoutes.AgentStart,
					legacyrbac.NewPostgresResolver(pool), publicRoutes.Steering, logger,
				); handOffErr != nil {
					return fmt.Errorf("compose steering hand-off: %w", handOffErr)
				}
			}
			currentAgentStart, err = agentexecutionapi.NewCurrentApplicationStartRoute(
				publicRoutes.AgentStart,
				// The browser's session cookie is this route's only credential
				// (#291). This handler serves START, REGENERATE and CONTINUE
				// (production_router.go), i.e. every write path a chat
				// conversation has, and the UI authenticates with a cookie and
				// nothing else — no bearer, no forwarded identity. Without it
				// the product's own chat cannot start a turn while every
				// server-side hop can, the same shape #93 found on the events
				// stream, which this is the other half of: the UI could read a
				// stream it was not allowed to open.
				//
				// apiGroupAuth carries that cookie in both deployment shapes,
				// so the credential no longer depends on a private literal
				// keeping one field.
				//
				// It does not widen what a caller may DO. The route still
				// resolves permissions through legacyrbac below, so membership
				// and `models.chat.messages.create` are checked exactly as
				// before; this only lets a session prove who it is.
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if err != nil {
				return fmt.Errorf("compose current agent-start route: %w", err)
			}
		}
		if publicRoutes.ToolkitExecuteRead != nil {
			mcpToolkitExecute = publicRoutes.ToolkitExecuteRead
		}
		if publicRoutes.NodeRecovery != nil {
			currentNodeRecovery, err = agentexecutionapi.NewCurrentNodeRecoveryRoute(publicRoutes.NodeRecovery, apiGroupAuth, legacyrbac.NewPostgresResolver(pool))
			if err != nil {
				return err
			}
		}
		if publicRoutes.ExecutionInterrupts != nil {
			currentExecutionInterrupts, err = agentexecutionapi.NewCurrentExecutionInterruptRoute(publicRoutes.ExecutionInterrupts, apiGroupAuth, legacyrbac.NewPostgresResolver(pool))
			if err != nil {
				return err
			}
		}
		if publicRoutes.AgentCancel != nil {
			// The cloud turn canceller of the workspace handoff (ADR-0034):
			// the same stop use case the chat's DELETE route drives.
			workspaceTurnCanceller.bind(publicRoutes.AgentCancel)
			currentAgentCancel, err = agentexecutionapi.NewCurrentAgentCancelRoute(
				publicRoutes.AgentCancel,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if err != nil {
				return fmt.Errorf("compose current agent-cancel route: %w", err)
			}
		}
		if publicRoutes.Steering != nil {
			currentAgentSteering, err = agentexecutionapi.NewCurrentAgentSteeringRoute(
				publicRoutes.Steering,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
				auditRecorder,
			)
			if err != nil {
				return fmt.Errorf("compose current agent-steering route: %w", err)
			}
		}
		// The legacy application_task path (issue 254 P2). It needs BOTH halves
		// — the poll reader and the canceller — so it is composed only when the
		// runtime supplies both. It takes the same apiGroupAuth the cancel route
		// takes, for the reason recorded on the start route above: the browser's
		// session cookie is the only credential the chat surface carries.
		if publicRoutes.AgentTaskStatus != nil && publicRoutes.AgentCancel != nil {
			currentApplicationTask, err = agentexecutionapi.NewCurrentApplicationTaskRoute(
				publicRoutes.AgentTaskStatus,
				publicRoutes.AgentCancel,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if err != nil {
				return fmt.Errorf("compose current application-task route: %w", err)
			}
		}
		if publicRoutes.IndexCancel != nil {
			currentIndexCancel, err = indexingapi.NewCurrentIndexCancelRoute(
				publicRoutes.IndexCancel,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if err != nil {
				return fmt.Errorf("compose current index-cancel route: %w", err)
			}
		}
		if publicRoutes.IndexMeta != nil {
			currentIndexMeta, err = indexingapi.NewCurrentIndexMetaRoute(
				publicRoutes.IndexMeta,
				apiGroupAuth,
				legacyrbac.NewPostgresResolver(pool),
			)
			if err != nil {
				return fmt.Errorf("compose current index-meta route: %w", err)
			}
		}
		if publicRoutes.IndexMetaDelete != nil {
			currentIndexMetaDelete, err =
				indexingapi.NewCurrentIndexMetaDeleteRoute(
					publicRoutes.IndexMetaDelete,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			if err != nil {
				return fmt.Errorf(
					"compose current index metadata delete route: %w",
					err,
				)
			}
		}
		if publicRoutes.IndexConfiguration != nil {
			currentIndexConfiguration, err =
				indexingapi.NewCurrentIndexConfigurationRoute(
					publicRoutes.IndexConfiguration,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			if err != nil {
				return fmt.Errorf(
					"compose current index configuration route: %w",
					err,
				)
			}
		}
		if publicRoutes.IndexScheduleUpdate != nil {
			currentIndexScheduleUpdate, err =
				indexingapi.NewCurrentIndexScheduleRoute(
					publicRoutes.IndexScheduleUpdate,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			if err != nil {
				return fmt.Errorf(
					"compose current index schedule update route: %w",
					err,
				)
			}
		}
		if publicRoutes.IndexScheduleDelete != nil {
			currentIndexScheduleDelete, err =
				indexingapi.NewCurrentIndexScheduleDeleteRoute(
					publicRoutes.IndexScheduleDelete,
					apiGroupAuth,
					legacyrbac.NewPostgresResolver(pool),
				)
			if err != nil {
				return fmt.Errorf(
					"compose current index schedule delete route: %w",
					err,
				)
			}
		}
		slog.Info("production runtime enabled", "control_addr", runtimeConfig.ControlAddress, "output_addr", runtimeConfig.OutputAddress, "content_addr", runtimeConfig.ContentAddress)
	}

	// CONFIGURING a pipeline's unattended entry points does not need the
	// execution runtime, and #899 is what happens when it is treated as though
	// it did: with `runtime.enabled` off the handler above was never built, so
	// `/pipeline_triggers` and `/pipeline_schedules` were not mounted at all
	// and the editor's whole Schedule/Webhook surface 404ed — the "affordance
	// the user cannot repair" the SPA's own capability module exists to
	// prevent.
	//
	// The settings half is a table and a credential; only STARTING a run needs
	// the runner. So the handler is built either way, and the one dependency
	// that is genuinely absent — AgentStartUseCase — is passed as an untyped
	// nil, which NewPlatformHandler's own present() check turns into the
	// documented honest degrade: the inbound POST and every scheduled fire
	// answer 503 and say why, while the settings routes work. The schedule
	// TICK is still only started inside the runtime block above; a schedule
	// saved on a runtime-less deployment is stored and simply never fires,
	// which is the same answer the inbound route gives.
	if pipelineTriggers == nil {
		pipelineTriggers = v2pipelinetriggers.NewPlatformHandler(
			pool,
			nil,
			v2secrets.NewHandler(pool),
			legacyrbac.NewPostgresResolver(pool),
			auditRecorder,
			logger,
			domainEvents,
			pipelineRunsRepo,
			v2pipelinetriggers.WithGovernedRuns(governedTriggers.Runs),
		)
	}

	// #1359 H8: ONE client of elitea-llm-gateway, gRPC over mutual TLS
	// (elitea.gateway.v1.GatewayService). Every consumer below — the /llm
	// surface, the configuration checks, predict_llm and the governance status —
	// shares it, so an operator configures the gateway hop once. It is nil when
	// LLM_GATEWAY_URL is empty, and each consumer reports that posture.
	var llmGateway *llmproxy.GatewayClient
	if gwURL := os.Getenv("LLM_GATEWAY_URL"); gwURL != "" {
		client, clientErr := llmproxy.NewGatewayClient(llmproxy.GatewayClientConfig{
			Target:         gwURL,
			IdentitySecret: os.Getenv("GATEWAY_IDENTITY_SECRET"),
			ClientCertFile: os.Getenv("LLM_GATEWAY_CLIENT_CERT"),
			ClientKeyFile:  os.Getenv("LLM_GATEWAY_CLIENT_KEY"),
			CAFile:         os.Getenv("LLM_GATEWAY_CA_FILE"),
		})
		if clientErr != nil {
			return fmt.Errorf("compose llm gateway client: %w", clientErr)
		}
		defer func() { _ = client.Close() }()
		llmGateway = client
	}

	// BF0.9c: the browser /llm surface over that client.
	var gatewayProxy http.Handler
	var gatewayProjectResolver apimw.PersonalProjectResolver
	if llmGateway != nil {
		gw, gwErr := llmproxy.New(llmproxy.Config{
			Gateway: llmGateway,
			Logger:  logger,
			// The analytics reads decide active users and run spend from
			// the inbound execution id, so the edge keeps it only for a live
			// execution of the caller.
			ExecutionVerifier: dbrepos.NewExecutionAttributionVerifier(pool).WithLocalWorkPolicy(nativePolicy),
			// One audit row per metered tool call (#1238): the worker cannot
			// write the trail itself.
			Audit: auditRecorder,
		})
		if gwErr != nil {
			return fmt.Errorf("compose llm gateway proxy: %w", gwErr)
		}
		gatewayProxy = gw
		gatewayProjectResolver = apimw.NewDBPersonalProjectResolver(pool)
		slog.Info("llm gateway proxy enabled", "target", os.Getenv("LLM_GATEWAY_URL"))
	}

	// #319: /configurations/check_connection(s) needs a real, minimal round
	// trip to the provider, which the gateway performs (it owns the SSRF-safe
	// egress allowlist for a tenant-authored api_base — issue #13). It uses the
	// shared gateway client above.
	var configConnectionChecker configurationapi.ConnectionChecker
	if llmGateway != nil {
		checker := configurationapi.NewGatewayConnectionChecker(llmGateway)
		// Assigned only when non-nil: boxing a nil *GatewayConnectionChecker
		// into the ConnectionChecker interface would make
		// `h.connectionChecker == nil` false (a non-nil interface holding a
		// nil pointer) and CheckConnection would call a method on a nil
		// receiver instead of reporting "not available".
		configConnectionChecker = checker
		slog.Info("configurations check-connection client enabled", "target", os.Getenv("LLM_GATEWAY_URL"))
	}

	// #194: POST /elitea_core/predict_llm/prompt_lib/{projectID} runs one
	// stateless LLM turn through the shared gateway client. A nil completer is
	// a supported state — the route is still registered and answers 503 naming
	// LLM_GATEWAY_URL — but it must be
	// left as a nil INTERFACE, not a boxed nil pointer, or the handler's
	// `completer == nil` check is false and it calls a method on a nil
	// receiver instead of reporting "not configured".
	var predictCompleter predictapi.Completer
	if llmGateway != nil {
		completer := predictapi.NewGatewayCompleter(llmGateway)
		predictCompleter = completer
		if currentConfigurationsRoot != nil {
			predictCompleter, err = predictapi.WithDefaultModel(
				completer, currentConfigurationsRoot.ModelCatalog(), currentConfigurationsConfig.PublicProjectID,
			)
			if err != nil {
				return fmt.Errorf("compose predict default model resolver: %w", err)
			}
		}
		slog.Info("predict_llm completion client enabled", "target", os.Getenv("LLM_GATEWAY_URL"))
	} else {
		slog.Warn("predict_llm completion client disabled: LLM_GATEWAY_URL is empty; " +
			"the route stays registered and answers 503")
	}

	// Agent Evaluation runs execute HERE, in this process: a bounded worker
	// pool of goroutines driven by the run rows themselves. The reasoning for
	// not using the runtime plane or the scheduler is in the package doc of
	// internal/api/v2/evaluation (orchestrator.go).
	//
	// The judge and the agent turn both take `predictCompleter`, the SAME
	// client /predict_llm and the three AI-draft routes take. There is no
	// second LLM client in this service, and adding one would mean a second
	// place that resolves credentials and a second egress path to keep
	// SSRF-safe.
	//
	// A nil completer is a supported state and does NOT unregister the routes:
	// runs are still stored and listed, every case fails with the reason
	// "no LLM plane is composed", and the run finishes `errored` carrying it.
	// An operator can then see the run, read why, and fix the deployment —
	// which a 404 would not let them do (#126).
	evalRunsRepo := evalRunsRepository(pool)
	var evalOrchestrator v2evaluation.Enqueuer
	if evalRunsRepo != nil {
		orchestrator := v2evaluation.NewOrchestrator(
			evalRunsRepo, v2evaluation.NewAIJudge(predictCompleter), predictCompleter, logger)
		// Started with the PROCESS context, so the workers and the recovery
		// sweep stop on SIGTERM. A run in flight then hands its row back to
		// `created`, and the next process resumes it at startup; a process
		// that dies without that write is re-queued once its heartbeat goes
		// stale — which is what makes a restart lose nothing.
		orchestrator.Start(ctx)
		// Deferred AFTER `defer pool.Close()`, so it runs BEFORE it: the
		// workers write their shutdown release while the pool is still open.
		defer orchestrator.Stop(evalShutdownTimeout)
		evalOrchestrator = orchestrator
	}

	// The admin LLM Proxy section reads the gateway's own enforcement status.
	// It uses the shared gateway client; the GovernanceStatus call carries no
	// identity, because the gateway verifies none on it.
	var gatewayStatus gateway.StatusReader
	if llmGateway != nil {
		statusClient := gateway.NewGatewayStatusClient(llmGateway)
		// Assigned only when non-nil, for the reason spelled out above
		// configConnectionChecker: a nil pointer boxed into this interface is
		// not nil, and the handler's "not configured" branch would never run.
		gatewayStatus = statusClient
		slog.Info("gateway status client enabled", "target", os.Getenv("LLM_GATEWAY_URL"))
	}

	// BF0.9c/d: the gateway proxy needs the same production-auth wiring as
	// every other auth-protected route above, gated on formGraph != nil —
	// assigning a nil *FormGraph directly to an interface field would produce
	// a non-nil interface holding a nil pointer, defeating the "not
	// configured" nil-checks in the auth middleware (#86).
	//
	// The same four fields also carry the WHOLE /api/v2 group, because
	// internal/api/router.go installs one apimw.AuthConfig built from them with
	// r.Use on the group that wraps r.Route("/api/v2", ...).
	//
	// authsvc.NewPrincipalValidator(pool) is built here rather than reusing the
	// `principalValidator` variable because that variable is nil in exactly the
	// OIDC-only branch: it is only assigned inside the `authEnabled` block,
	// which is also the only place formGraph is set. Without it a deactivated
	// user's unexpired session cookie reached every route in the group (#370).
	// See apiGroupAuthConfig.
	//
	// sessionTokens is the personal-access-token validator for the same branch.
	// APPLICATION_SECRET_KEY signs the tokens the /api/v2/auth/token route
	// issues, so the validator must read them back with that exact key.
	// The variable stays a nil interface when the key is absent: a boxed nil
	// pointer would read as "configured" downstream (#86).

	// The branding package (ADR-0024 decision 9) renders the login page and
	// the e-mails under the pack it carries, and embeds the offline previewer
	// the image ships beside the admin SPA (Containerfile copies
	// dist/brand-preview/ to <ADMIN_UI_STATIC_DIR>/../brand-preview/, so the
	// file is <ADMIN_UI_STATIC_DIR>/../brand-preview/index.html). No previewer
	// file means the package omits preview/app.html and its README says so.
	brandingAssetStore := v2branding.NewAssetStore(objectStore)
	previewer, previewerErr := os.ReadFile(filepath.Join(filepath.Dir(strings.TrimRight(os.Getenv("ADMIN_UI_STATIC_DIR"), "/")), "brand-preview", "index.html"))
	if previewerErr != nil {
		previewer = nil
		logger.Info("branding package: no offline previewer file; packages ship without preview/app.html")
	}
	brandingPackages := brandpackage.New(brandingAssetStore, brandpackage.Previews{
		Login: browserapi.RenderLoginPreview,
		Email: func(pack *v2branding.Pack, kind string) (string, error) {
			html, _, err := mailComposer.Preview(pack, kind)
			return html, err
		},
		App: previewer,
	}, mailSettings.Settings.PublicBaseURL)

	var adminUICfg *adminui.Config
	if dir := os.Getenv("ADMIN_UI_STATIC_DIR"); dir != "" {
		adminUICfg = &adminui.Config{
			StaticDir:     dir,
			ViteServerURL: "/api/v2",
			BasePath:      "/admin/app",
			SecretKey:     os.Getenv("APPLICATION_SECRET_KEY"),
			// The admin SPA needs the operator's REAL administration-mode
			// permissions. Without a resolver the handler injects an empty
			// list, which hides every control. It must never inject a fixed
			// admin list. The console then shows a rank-and-file user
			// controls that the server refuses with 403 on each click.
			Resolver: legacyrbac.NewPostgresResolver(pool),
			// A Form deployment authenticates the browser at /auth/login,
			// which sets `elitea_browser_auth`. Without this reader the
			// handler has no way to read that identity, injects an empty
			// permission list, and the SPA renders a sidebar with no items —
			// see adminui's ServeSPA. A nil reader (no form graph) still
			// degrades closed.
			FormSessions: formSessions,
			// The OIDC path's credential after shared migration 0117. The
			// cookie carries an opaque identifier and no claims, so without
			// this the HMAC reader below finds nothing and the sidebar is
			// empty again. A nil manager keeps the legacy reader.
			Sessions: adminUISessions(sessionManager),
			Emails:   adminUIEmails{pool: pool},
		}
	}

	// Canvas presence's cross-replica wiring: the roster in a JetStream KV
	// bucket on the live-update NATS server, published on the live-update
	// bus. Both halves or neither — see v2canvaspresence.Backend. A server
	// without JetStream, or without the bucket the nats-bootstrap Job creates
	// (main binds, it does not create — #1076), fails startup here, like an
	// unreachable one does.
	var canvasPresence v2canvaspresence.Backend
	if eventsNATS != nil {
		presenceStore, err := newCanvasPresenceStore(ctx, eventsNATS)
		if err != nil {
			return fmt.Errorf("compose canvas presence store: %w", err)
		}
		// Stops the bucket watcher. Deferred after liveNATSBus.Close, so it
		// runs first: the watcher is gone before the connection drains.
		defer presenceStore.Close()
		canvasPresence = v2canvaspresence.Backend{Store: presenceStore, Bus: liveNATSBus}
	}

	// The platform MCP replay store (issue 1231): a JetStream KV bucket on the
	// same server, which every replica claims a platform call's
	// Idempotency-Key in before running it. It is optional: without the
	// live-update NATS, or with the bucket missing or misconfigured (logged as
	// an error naming the nats-bootstrap Job), elitea-main starts without it
	// and the remote toolkit route answers 501 for platform MCP tools rather
	// than dedupe them per replica.
	platformMCPReplays := composePlatformMCPReplays(ctx, eventsNATS, logger)

	// The toolkit TYPE catalogue (GET /elitea_core/toolkits/prompt_lib/
	// {projectID}) serves each tool's argument schema from the digest-pinned SDK
	// snapshot. It is loaded unconditionally and here, in the composition root,
	// because internal/api must not import internal/runtimecomposition. A
	// snapshot that will not load is an embedded-asset defect, so it stops
	// startup rather than degrading the endpoint to schema-less tool lists.
	toolkitArgumentSchemas, err := runtimecomposition.LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		return fmt.Errorf("load pinned current toolkit schema snapshot: %w", err)
	}
	// The same endpoint serves the "$defs" its settings properties reference.
	// That block is the join of the toolkit snapshot above (which settings
	// field references a configuration) with the SDK configuration catalogue
	// (which section that configuration belongs to), so both pinned files load
	// here and stop startup the same way if either is unreadable.
	sdkConfigurations, err := runtimecomposition.LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		return fmt.Errorf("load pinned current SDK configuration catalog: %w", err)
	}
	toolkitSettingsDefinitions, err := runtimecomposition.NewCurrentToolkitSettingsDefinitionCatalog(
		toolkitArgumentSchemas,
		sdkConfigurations,
	)
	if err != nil {
		return fmt.Errorf("compose current toolkit settings definitions: %w", err)
	}
	// patSigner signs the personal access tokens /api/v2/auth/token returns.
	//
	// The form graph validates a personal access token with the bytes of
	// credentials.pat_signing_key_file. APPLICATION_SECRET_KEY is a different
	// value, so a token signed with it failed the signature check on first use
	// and the user kept a dead credential. The graph now signs it.
	//
	// The variable stays a nil interface when there is no graph: the OIDC-only
	// shape signs and validates with APPLICATION_SECRET_KEY, and boxing a nil
	// *FormGraph would read as "configured" downstream (#86).
	var patSigner v2auth.TokenSigner
	if formGraph != nil {
		patSigner = formGraph
	}

	// One native policy reader for the router (discovery, token responses,
	// the 426 gate, the admin save's invalidation) AND the local turn start,
	// so a save that turns local work off is seen by both at once.
	currentLocalTurns, err := composeLocalTurns(pool, nativePolicy, auditRecorder, apiGroupAuth, logger)
	if err != nil {
		return fmt.Errorf("compose local turn routes: %w", err)
	}
	// The handoff's sweeper; with the runtime plane composed it also ends
	// cloud turns whose finish hook never ran.
	runWorkspaceHandoffSweeper(ctx, workspaceHandoff, workspaceExecutor.lifecycle(runtimeRoot != nil), logger)
	currentResolvedVersion, err := composeResolvedVersion(pool, clientApplicationVersions, apiGroupAuth)
	if err != nil {
		return fmt.Errorf("compose resolved version route: %w", err)
	}
	currentRemoteToolkit, err := composeRemoteToolkit(pool, toolkitToolRun, remoteToolAuthorizer, workerImplementation,
		nativePolicy, auditRecorder, platformMCPTools, platformMCPReplays, apiGroupAuth, logger)
	if err != nil {
		return fmt.Errorf("compose remote toolkit route: %w", err)
	}
	currentChildTurns, err := composeChildTurns(pool, childAgentStart, childAgentAuthorizer, nativePolicy, auditRecorder, apiGroupAuth, logger)
	if err != nil {
		return fmt.Errorf("compose local child turn routes: %w", err)
	}

	// The project-deletion reconciler (#1211) drains the cleanup journal over
	// the one provisioner the router builds, so it is started from the router's
	// hook. A failure to construct it
	// is carried out of the hook and stops startup like the other composition
	// errors.
	deletionReconciler, err := projectDeletionReconcilerFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load project deletion reconciler settings: %w", err)
	}
	projectDeleteBudget, err := projectDeleteBudgetFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load project delete budget: %w", err)
	}
	var deletionReconcilerErr error
	startDeletionReconciler := func(provisioner *projectprovisioning.Provisioner) {
		deletionReconcilerErr = startProjectDeletionReconciler(ctx, deletionReconciler, provisioner, logger)
	}
	// The toolkit-deletion reconciler (#1244) drains the toolkit cleanup
	// journal the toolkit handler enqueues into, over the same journal, from
	// the router's hook; it has a flag of its own.
	toolkitReconciler, err := toolkitDeletionReconcilerFromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load toolkit deletion reconciler settings: %w", err)
	}
	var toolkitReconcilerErr error
	startToolkitReconciler := func(journal *toolkitcleanup.Journal) {
		toolkitReconcilerErr = startToolkitDeletionReconciler(ctx, toolkitReconciler, journal, logger)
	}

	// The browser edge (#1359 P2, P3): /app and /docs relayed to the web
	// container, `/` redirected to /app/. Compose sets it, and the chart sets
	// it to the elitea-web Service whenever it renders one.
	webUpstream, err := webedge.ParseUpstream(os.Getenv(webedge.UpstreamEnv))
	if err != nil {
		return fmt.Errorf("configure web edge: %w", err)
	}
	if webUpstream != nil {
		logger.Info("browser edge enabled", "web_upstream", webUpstream.String())
	}

	// Conversation retention (ADR-0042 decision 4, #1495): deletes nothing
	// until an administrator sets a maximum age for conversations.
	if err := startConversationRetention(ctx, pool, objectStore, logger); err != nil {
		return fmt.Errorf("compose conversation retention: %w", err)
	}

	r := api.NewRouter(api.RouterConfig{
		RetentionAdmin:                v2retention.NewHandler(dbrepos.NewRetentionPolicyRepo(pool)),
		AgentIdentityAdmin:            agentIdentity.Admin,
		CredentialGrantsAdmin:         credentialGrantsAdminHandler,
		CredentialProbe:               credentialProbeHandler,
		CredentialConsent:             providerOAuthGoverned.Consent,
		ExternalAgentIdentityAdmin:    agentIdentity.ExternalAdmin,
		ExecutionSuspensions:          executionSuspensions.handler,
		MandateAttachments:            governedTriggers.Attachments,
		CredentialReferencesAdmin:     credentialReferencesHandler,
		AgentAssertionWellKnown:       eliteaNative.WellKnown,
		GovernedMCPInbound:            governedMCPComposed.Inbound,
		GovernedMCPOutbound:           governedMCPComposed.Outbound,
		WebUpstream:                   webUpstream,
		OnProjectProvisioner:          startDeletionReconciler,
		OnToolkitCleanupJournal:       startToolkitReconciler,
		ProjectCleanupContext:         ctx,
		ProjectDeleteBudget:           projectDeleteBudget,
		ApplicationData:               applicationData,
		ProjectVectorData:             projectVectorData,
		MemoryData:                    memoryData,
		MemoryDataRecorded:            runtimeConfig.RustIndexing(),
		ProjectVectorDataRecorded:     vectorStepRecorded,
		ProjectVectorCleanupAbandoned: vectorCleanupAbandoned,
		LegacyVectorDatabases:         legacyVectorDatabases,
		LegacyVectorDatabasesRetired:  legacyPgvectorRetired,
		AdminUI:                       adminUICfg,
		Pool:                          pool,
		Branding:                      brandingResolver,
		DeploymentKind:                deploymentKind,
		PublicOrigin:                  publicOrigin,
		NativeClients:                 native.registry,
		NativeStore:                   native.store,
		NativePolicy:                  nativePolicy,
		NativeAccess:                  native.validator,
		NativeSecureCookies:           os.Getenv("COOKIE_SECURE") != "false",
		Mailer:                        mailComposer,
		EmailSettings:                 emailResolver,
		BrandingPackages:              brandingPackages,
		ToolkitArgumentSchemas:        toolkitArgumentSchemas,
		ToolkitCatalogue:              toolkitCatalogue,
		ToolkitWorkerCapability:       workerToolkitCapability,
		MemoryOnVectorStore:           runtimeConfig.RustIndexing(),
		ToolkitSettingsDefinitions:    toolkitSettingsDefinitions,
		ToolkitSettingsValidator:      toolkitSettingsValidator,
		ToolkitRegistry:               toolkitArgumentSchemas,
		DelegatedAuthToolkitSettings:  delegatedAuthToolkitSettings,
		WorkerImplementation:          workerImplementation,
		// Readiness is the database alone. The Form graph's sign-in state is
		// in this same database (shared migration 0153), so it adds no
		// dependency of its own; the auth Redis it once pinged is gone.
		HealthDeps: health.Deps{
			DB: &poolChecker{pool: pool},
		},
		AuthValidator:      apiGroupAuth.Validator,
		PrincipalValidator: apiGroupAuth.PrincipalValidator,
		SessionSecret:      apiGroupAuth.SessionSecret,
		PATSigner:          patSigner,
		Auth: api.AuthDeps{
			FormSessions:               apiGroupAuth.FormSessions,
			SessionStore:               apiGroupAuth.SessionStore,
			RejectLegacySessionCookies: apiGroupAuth.RejectLegacySessionCookies,
			SessionHandler:             oidcSessionHandler,
			OIDCHandler:                oidcOIDCHandler,
			SAMLHandler:                oidcSAMLHandler,
		},
		ProductionAuth:                productionAuth,
		ProductionRuntime:             productionRuntime,
		CurrentProjectInfo:            currentProjectInfo,
		CurrentIndexTypes:             currentIndexTypes,
		AttachmentExtensions:          attachmentExtensions,
		AttachmentExtractionWarmer:    attachmentExtractionWarmer,
		CurrentApplicationSkills:      currentApplicationSkills,
		CurrentPromptContextReads:     currentPromptContextReads,
		CurrentProjectList:            currentProjectList,
		CurrentSocialAuthors:          currentSocialAuthors,
		CurrentSocialAvatar:           currentSocialAvatar,
		CurrentConfigurationAvailable: currentConfigurationAvailable,
		CurrentConfigurationRead:      currentConfigurationRead,
		CurrentConfigurationTypes:     currentConfigurationTypes,
		InternalConfigurationTools:    internalConfigurationTools,
		CurrentConfigurationMutation:  currentConfigurationMutation,
		CurrentIndexStart:             currentIndexStart,
		DeepWiki:                      deepwikiRoute,
		Inventory:                     inventoryRoute,
		CurrentAgentStart:             currentAgentStart,
		// The support assistant's predict route delegates to the SAME
		// agent-execution use case CurrentAgentStart's HTTP route drives — not a
		// second pipeline. A support turn is an ordinary agent run in a hidden
		// project, and giving it its own executor is how the two would drift
		// apart on tracing, budgets and cancellation.
		SupportAssistantStart:          supportAssistantStart,
		MCPAgentStart:                  mcpAgentStart,
		MCPToolkitExecute:              mcpToolkitExecute,
		MCPToolkitRun:                  mcpToolkitRun,
		ToolkitToolRun:                 toolkitToolRun,
		ToolkitDiscovery:               toolkitDiscovery,
		PipelineTriggers:               pipelineTriggers,
		AuditRecorder:                  auditRecorder,
		SCIMAccessTokenTTL:             scimAccessTokenTTL,
		SCIMClientAddresses:            scimClientAddresses,
		CurrentAgentCancel:             currentAgentCancel,
		CurrentAgentSteering:           currentAgentSteering,
		CurrentLocalTurns:              currentLocalTurns,
		CurrentWorkspaceHandoff:        workspaceHandoff.Route,
		CloudExecutorStorageEncryption: workspaceHandoff.Encryption,
		CurrentResolvedVersion:         currentResolvedVersion,
		CurrentRemoteToolkit:           currentRemoteToolkit,
		CurrentChildTurns:              currentChildTurns,
		PlatformMCPTools:               platformMCPTools,
		CurrentNodeRecovery:            currentNodeRecovery,
		CurrentExecutionInterrupts:     currentExecutionInterrupts,
		CurrentApplicationTask:         currentApplicationTask,
		CurrentIndexCancel:             currentIndexCancel,
		CurrentIndexMeta:               currentIndexMeta,
		CurrentIndexMetaDelete:         currentIndexMetaDelete,
		CurrentIndexConfiguration:      currentIndexConfiguration,
		CurrentIndexScheduleUpdate:     currentIndexScheduleUpdate,
		CurrentIndexScheduleDelete:     currentIndexScheduleDelete,
		CurrentNotifications:           currentNotifications,
		CurrentNotificationStore:       currentNotificationStore,
		CurrentNotificationEvents:      currentNotificationEvents,
		CurrentModelCatalog:            currentModelCatalog,
		CurrentModelDefault:            currentModelDefault,
		GatewayProxy:                   gatewayProxy,
		GatewayProjectResolver:         gatewayProjectResolver,
		RunTokens:                      runTokens.Validator,
		ConfigConnectionChecker:        configConnectionChecker,
		PredictCompleter:               predictCompleter,
		GatewayStatus:                  gatewayStatus,
		ConfigProviderAdmission:        configProviderAdmission,
		ConfigStoredResolver:           configStoredResolver,
		ObjectStore:                    objectStore,
		PlatformModelDefaults:          platformModelDefaults,
		// Without AppsRepo, internal/api/router.go silently skips registering
		// every /elitea_core/application(s)/* and /elitea_core/version(s)/*
		// route, and creating an agent from the UI 404s (#115).
		AppsRepo: applicationsRepository(pool),
		// Same defect class as AppsRepo above, six more times (#126). Each of
		// these repositories already EXISTED — conversations.go alone is 951
		// lines — and had zero callers, so router.go dropped their route groups
		// and the endpoints 404'd in every deployment. Counted at the gates:
		// conversations 23 routes, skills 12, analytics 7, folders 6, tags 3.
		ConvsRepo: conversationsRepository(pool),
		// Share a conversation by link. One repository satisfies both
		// interfaces — the central link table and the per-project transcript
		// read — but they are two FIELDS because they are two tenancies and
		// two mount points (one authenticated group, one anonymous pair), and
		// a single field would make it impossible to register the anonymous
		// routes without also claiming a transcript reader.
		SharedChatStore:      sharedChatLinksRepository(pool),
		SharedChatTranscript: sharedChatTranscriptRepository(pool),
		SkillsRepo:           skillsRepository(pool),
		FoldersRepo:          foldersRepository(pool),
		MemoriesRepo:         memoriesRepository(pool),
		TagsRepo:             tagsRepository(pool),
		AnalyticsRepo:        analyticsRepository(pool),
		// The Agent Evaluation dimension library. Wired here, at the
		// composition root, in the SAME change as the routes and the RBAC
		// grant: a handler written and never composed is the shape #597
		// records — both halves correct, the wiring the defect, and 2475 green
		// unit tests unable to see it.
		EvalDimensionsRepo: evalDimensionsRepository(pool),
		// Agent Evaluation slice 2, composed in the SAME change as its routes
		// for the reason above. The orchestrator is started below, after the
		// config is built, so that a nil pool leaves all three fields nil
		// together and the thirteen routes are simply not registered.
		EvalDatasetsRepo: evalDatasetsRepository(pool),
		EvalRunsRepo:     evalRunsRepo,
		EvalOrchestrator: evalOrchestrator,
		// WebhookRepo is the sixth instance of the same defect, and it hid one
		// step deeper than the other five. Its gate mounts a subrouter —
		// `r.Mount("/webhooks/prompt_lib/{projectID}", webhook.NewHandler(...).Routes())`
		// — so a count of inline r.Get/r.Post calls inside the gated block
		// returns zero, and it looked like a field that gated nothing. The five
		// routes are declared in the handler's own Routes() method.
		WebhookRepo: webhooksRepository(pool),
		// The project SSE stream's transport: the live-update NATS bus, or a
		// nil interface when ELITEA_EVENTS_NATS_URL is unset (the route is
		// then deliberately unregistered and startup logs it). Its former
		// Redis arm is deleted; #152 is the record of that two-arm fallback
		// shipping with both arms nil.
		EventSource: eventSource,
		// Client contract 1.12: the read frame of markConversationRead.
		ConversationActivity: conversationActivity,
		// Cross-replica canvas presence: roster store + live-update bus.
		// The zero value keeps the in-process roster and publishes nothing.
		CanvasPresence: canvasPresence,
		// #876's second half — see this variable's own composition comment,
		// above, for its Bus and its one Sink, the webhook Dispatcher.
		DomainEvents:                domainEvents,
		WebhookDeliveries:           webhookDeliveries,
		WebhookDispatcher:           webhookDispatcher,
		WebhookDestinationGuard:     webhookDestinationGuard,
		MCPAuthorizationEgressGuard: mcpAuthorizationEgressGuard,
	})
	if deletionReconcilerErr != nil {
		return fmt.Errorf("start project deletion reconciler: %w", deletionReconcilerErr)
	}
	if toolkitReconcilerErr != nil {
		return fmt.Errorf("start toolkit deletion reconciler: %w", toolkitReconcilerErr)
	}

	// NOTE(#126): the Socket.IO prototype server (internal/api/socketio) is
	// gone. It was never mounted — the comment that stood here said it stayed
	// unmounted until connection authentication, project-membership checks,
	// room authorization and a per-event permission contract existed, since
	// mounting it would have exposed cross-tenant rooms and execution events.
	// Every one of its handlers proxied to indexersvc.Client, the prototype
	// Redis RPC transport this change retires, so it could not have been
	// mounted after the deletion either. The chat-dispatch migration it was a
	// placeholder for is #93; the web client's own socket.io still points at
	// the legacy runtime and is unaffected.
	// NOT wrapped in otelhttp here: internal/api/router.go already installs
	// apimw.OtelMiddleware (r.Use at router.go:396) as chi middleware, which
	// creates a span per request via otel.Tracer("elitea-main") — the same
	// global tracer provider observability.New (above) installs. otelhttp
	// would duplicate that instrumentation AND break it: otelhttp's response
	// writer wrapper has no Unwrap(), unlike apimw's own statusRecorder
	// (middleware/otel.go), so http.ResponseController.SetWriteDeadline
	// (used by the SSE writers in internal/api/v2/executions/events.go and
	// internal/api/v2/notifications/events.go, and by artifact upload/
	// download deadlines) would stop reaching the real ResponseWriter.
	srv := newHTTPServer(publicAddress, r)
	// The internal platform gRPC listener (#1359 D1), when the runtime
	// configuration opens one.
	platformSrv, err := newPlatformServer(runtimeConfig, func() ([]runtimegrpc.PlatformService, error) {
		services, err := platformServices(r, platformClientsFromEnv(os.Getenv), logger)
		if err != nil {
			return nil, err
		}
		// elitea-vector's token introspection (ADR-0031), behind
		// ELITEA_VECTOR_INTROSPECTION_CLIENTS, runtime on or off. The PAT
		// validator is the one that reads back what this deployment's
		// callback minters sign (patSigner's rule): the form graph when there
		// is one, else the APPLICATION_SECRET_KEY validator (sessionTokens),
		// which is how a deployment without ELITEA_AUTH_CONFIG_FILE (the dev
		// compose, OIDC-only) signs its DeepWiki and Inventory bearers. Both
		// assigned through a nil INTERFACE, so a deployment with neither is
		// refused at boot rather than panicking on the first call.
		introspectionValidator := introspectionTokenValidator(formGraph, sessionTokens)
		return withVectorIntrospection(services, runtimeConfig, runtimeRoot,
			runtimecomposition.PlatformVectorIntrospection{
				Validator:     introspectionValidator,
				CallbackFacts: dbrepos.NewCallbackTokenGrants(pool),
				Logger:        logger,
			})
	})
	if err != nil {
		return fmt.Errorf("configure platform listener: %w", err)
	}

	slog.Info("starting server", "addr", srv.Addr, "runtime_enabled", runtimeRoot != nil)
	servers := applicationServers{public: srv}
	if platformSrv != nil {
		slog.Info("starting platform gRPC listener", "addr", platformSrv.Addr())
		servers.platform = platformSrv
	}
	var runtimeLifecycleRoot runtimeLifecycle
	if runtimeRoot != nil {
		runtimeLifecycleRoot = runtimeRoot
	}
	if err := serveApplication(ctx, servers, runtimeLifecycleRoot, shutdownTimeout); err != nil {
		return err
	}
	slog.Info("server stopped")
	return nil
}

// applicationsRepository composes the tenant-schema applications repository
// backing RouterConfig.AppsRepo. It is a named function rather than an inline
// constructor so main_router_wiring_test.go can assert the field is present in
// the production RouterConfig literal: a nil AppsRepo makes
// internal/api/router.go drop the whole /elitea_core applications route group
// without any startup error, which is how #115 stayed invisible.
func applicationsRepository(pool *pgxpool.Pool) applications.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewApplicationsRepo(pool)
}

// The remaining tenant-schema repositories, composed the same way and for the
// same reason: a nil field makes router.go drop the route group silently, with
// no startup error and a 404 indistinguishable from a typo'd path.
//
// Named functions, not inline constructors, so main_router_wiring_test.go can
// assert each field is present in the production literal.

func conversationsRepository(pool *pgxpool.Pool) v2convs.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewConversationsRepo(pool)
}

// sharedChatLinksRepository and sharedChatTranscriptRepository return the same
// concrete repository under its two interfaces. Both return a TYPED NIL-safe
// untyped nil when the pool is absent: a typed nil in an interface field is not
// `== nil`, so router.go's gate would register the routes over a repository
// whose every method panics — the typed-nil trap the gateway's /healthz hit.
func sharedChatLinksRepository(pool *pgxpool.Pool) sharedchat.Store {
	if pool == nil {
		return nil
	}
	return dbrepos.NewSharedChatLinksRepo(pool)
}

func sharedChatTranscriptRepository(pool *pgxpool.Pool) sharedchat.TranscriptStore {
	if pool == nil {
		return nil
	}
	return dbrepos.NewSharedChatLinksRepo(pool)
}

func skillsRepository(pool *pgxpool.Pool) v2skills.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewSkillsRepo(pool)
}

func foldersRepository(pool *pgxpool.Pool) v2folders.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewFoldersRepo(pool)
}

// memoriesRepository backs the CRUD router config field (MemoriesRepo),
// over the general-purpose pool. Turn-start RECALL is a SEPARATE
// *dbrepos.MemoriesRepo instance, built against the admission pool inside
// internal/runtimecomposition/composition.go — same table, same query
// logic, different connection pool, matching how that file already builds
// its own agentGuardrails/agentVersions repositories rather than reusing
// this file's RouterConfig ones.
func memoriesRepository(pool *pgxpool.Pool) v2memories.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewMemoriesRepo(pool)
}

// splitEnvList splits a comma-separated environment variable into trimmed,
// non-empty entries — the same shape the LLM gateway's `csvOr` produces for
// GATEWAY_EGRESS_ALLOWLIST (internal/config/config.go), so an operator moving
// an allowlist value between the two variables does not also have to change
// its punctuation.
func splitEnvList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.Split(raw, ",")
	entries := make([]string, 0, len(fields))
	for _, f := range fields {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// pipelineRunsRepository backs pipeline.run.succeeded/failed's tracking
// table (migrations/shared/0124). Returns a typed nil, not the untyped
// literal, on a nil pool: every consumer wires it through present()
// (pipelinetriggers.WithRunTracker) or a plain interface field a nil pointer
// satisfies harmlessly (pipelineruns.NewSettlementHook/NewFailureObserver,
// both of which nil-check their tracker before every call), so a pool-less
// deployment gets three no-ops rather than three panics.
func pipelineRunsRepository(pool *pgxpool.Pool) *dbrepos.PipelineRunsRepo {
	if pool == nil {
		return nil
	}
	return dbrepos.NewPipelineRunsRepo(pool)
}

func webhooksRepository(pool *pgxpool.Pool) webhook.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewWebhooksRepo(pool)
}

// webhookDeliveriesRepository backs the delivery log (#876's second half) —
// same nil-pool guard as webhooksRepository above, and the same reason: a
// typed-nil *WebhooksRepo boxed into the interface would make every method
// on it panic instead of the caller's own nil check catching it first.
func webhookDeliveriesRepository(pool *pgxpool.Pool) webhook.DeliveryRepository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewWebhookDeliveriesRepo(pool)
}

func evalDimensionsRepository(pool *pgxpool.Pool) v2evaluation.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewEvalDimensionsRepo(pool)
}

func evalDatasetsRepository(pool *pgxpool.Pool) v2evaluation.DatasetRepository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewEvalDatasetsRepo(pool)
}

// evalRunsRepository returns a nil INTERFACE and not a boxed nil pointer when
// there is no pool. The router's `cfg.EvalRunsRepo != nil` gate is an interface
// comparison, and a typed nil satisfies it — the route would then register and
// call a method on a nil receiver, which is the /healthz panic recorded in the
// gateway's own composition notes.
func evalRunsRepository(pool *pgxpool.Pool) v2evaluation.RunRepository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewEvalRunsRepo(pool)
}

func tagsRepository(pool *pgxpool.Pool) v2tags.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewTagsRepo(pool)
}

func analyticsRepository(pool *pgxpool.Pool) v2analytics.Repository {
	if pool == nil {
		return nil
	}
	return dbrepos.NewAnalyticsRepo(pool)
}

const maxAuthConfigPathBytes = 4096

// evalShutdownTimeout bounds how long shutdown waits for the evaluation
// workers to write their release. Each release write has its own 5 s bound.
const evalShutdownTimeout = 10 * time.Second

func configuredAuthConfigPath(lookup func(string) (string, bool)) (string, bool, error) {
	if lookup == nil {
		return "", false, errors.New("authentication configuration environment lookup is required")
	}
	path, present := lookup("ELITEA_AUTH_CONFIG_FILE")
	if !present {
		return "", false, nil
	}
	if path == "" || len(path) > maxAuthConfigPathBytes || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\r\n\x00") {
		return "", false, errors.New("ELITEA_AUTH_CONFIG_FILE is invalid")
	}
	return path, true, nil
}

// logInitialGlobalAdminShapes reports what `identity.initial_global_admins`
// holds, BY SHAPE.
//
// The list is consumed at a first login that can happen days after this boot,
// and a wrong entry produces silence: the person signs in and simply is not an
// administrator. The count alone did not separate the two shapes, so an
// operator who wrote `alice@corp.com` when they meant `email:alice@corp.com`
// saw the same log line as one who wrote it correctly.
//
// A MALFORMED ENTRY WARNS AND DOES NOT STOP THE BOOT. This value only changes
// what a first login receives; refusing to start would turn one bad list entry
// into an outage of a running deployment.
func logInitialGlobalAdminShapes(logger *slog.Logger, admins []string) {
	if logger == nil || len(admins) == 0 {
		return
	}
	shapes := identityapp.ClassifyInitialGlobalAdmins(admins)
	logger.Info("initial global administrators configured",
		"total", len(admins),
		"provider_references", shapes.References,
		"verified_emails", shapes.Emails)
	if len(shapes.Malformed) != 0 {
		logger.Warn("initial global administrator entries match nothing and are ignored",
			"count", len(shapes.Malformed),
			"entries", shapes.Malformed,
			"accepted_shapes", "oidc:<sub>, saml:<nameid>, <bare subject>, email:<verified address>")
	}
}

type publicServerLifecycle interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

type runtimeLifecycle interface {
	Run(context.Context) error
	Shutdown(context.Context) error
}

var ErrApplicationDrainTimeout = errors.New("application drain deadline exceeded")

func serveApplication(ctx context.Context, public publicServerLifecycle, runtime runtimeLifecycle, shutdownTimeout time.Duration) error {
	if ctx == nil || public == nil || shutdownTimeout <= 0 {
		return errors.New("application lifecycle dependencies are incomplete")
	}
	type result struct {
		name string
		err  error
	}
	componentCount := 1
	results := make(chan result, 2)
	go func() { results <- result{name: "public HTTP", err: public.ListenAndServe()} }()
	if runtime != nil {
		componentCount++
		// Runtime cancellation is owned by Shutdown below so the same drain
		// deadline reaches its publisher and private listeners before pools close.
		go func() { results <- result{name: "production runtime", err: runtime.Run(context.Background())} }()
	}

	var primary *result
	select {
	case first := <-results:
		primary = &first
	case <-ctx.Done():
	}
	causes := make([]error, 0, componentCount+1)
	if primary != nil {
		if primary.err == nil {
			causes = append(causes, fmt.Errorf("%s stopped unexpectedly", primary.name))
		} else {
			causes = append(causes, fmt.Errorf("%s failed: %w", primary.name, primary.err))
		}
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer drainCancel()
	shutdownResults := make(chan result, 2)
	go func() { shutdownResults <- result{name: "public HTTP", err: public.Shutdown(drainCtx)} }()
	shutdownPending := 1
	if runtime != nil {
		shutdownPending++
		go func() { shutdownResults <- result{name: "production runtime", err: runtime.Shutdown(drainCtx)} }()
	}
	remainingComponents := componentCount
	if primary != nil {
		remainingComponents--
	}
	drainDone := drainCtx.Done()
	deadlineObserved := false
	for remainingComponents > 0 || shutdownPending > 0 {
		select {
		case drained := <-results:
			remainingComponents--
			if drained.err == nil {
				causes = append(causes, fmt.Errorf("%s stopped unexpectedly", drained.name))
				continue
			}
			if errors.Is(drained.err, http.ErrServerClosed) || errors.Is(drained.err, context.Canceled) || errors.Is(drained.err, context.DeadlineExceeded) {
				continue
			}
			causes = append(causes, fmt.Errorf("%s failed during drain: %w", drained.name, drained.err))
		case stopped := <-shutdownResults:
			shutdownPending--
			if stopped.err != nil && !errors.Is(stopped.err, context.Canceled) && !errors.Is(stopped.err, context.DeadlineExceeded) {
				causes = append(causes, fmt.Errorf("shutdown %s: %w", stopped.name, stopped.err))
			}
		case <-drainDone:
			deadlineObserved = true
			drainDone = nil
			causes = append(causes, fmt.Errorf("%w after %s", ErrApplicationDrainTimeout, shutdownTimeout))
		}
	}
	if deadlineObserved && !errors.Is(errors.Join(causes...), ErrApplicationDrainTimeout) {
		causes = append(causes, fmt.Errorf("%w after %s", ErrApplicationDrainTimeout, shutdownTimeout))
	}
	return errors.Join(causes...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// objectStoreReadinessProbe issues one Stat against a sentinel key that is
// never expected to exist. ErrNotFound therefore means the backend is
// reachable and authenticated; any other error (access denied, a transport
// failure) means it is not, and the service should not start.
func objectStoreReadinessProbe(ctx context.Context, store storage.ObjectStore) error {
	ref, err := storage.NewObjectRef("1", "elitea-system", "readiness-probe")
	if err != nil {
		return err
	}
	if _, err := store.Stat(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

// retentionLifecycleConfigurer is satisfied by s3.Backend and gcs.Backend —
// see their ConfigureRetentionLifecycle doc comments for why this is a
// separate, optional step here rather than folded into newObjectStore/New()
// itself (New() must stay a cheap, non-network-dialing constructor).
// azure.Backend does not implement it: Azure's own default ~7-day
// uncommitted-block GC already delivers the same outcome with no explicit
// call, see azure/backend.go's AbortMultipart doc comment.
type retentionLifecycleConfigurer interface {
	ConfigureRetentionLifecycle(ctx context.Context) error
}

func configureObjectStoreRetentionLifecycle(ctx context.Context, store storage.ObjectStore) error {
	configurer, ok := store.(retentionLifecycleConfigurer)
	if !ok {
		return nil
	}
	return configurer.ConfigureRetentionLifecycle(ctx)
}

type poolChecker struct {
	pool *pgxpool.Pool
}

func (p *poolChecker) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// backfillProjectSecretsHeaderValues gives every existing project an `X-SECRET`
// value, and logs how many it wrote (#408). A project with no vault gets one,
// because a project row created by anything other than projectprovisioning has
// none and would otherwise never receive a value.
//
// WHY IT RUNS HERE. The value is sealed with the project's Fernet key, which
// this process wraps with SECRETS_MASTER_KEY, so no SQL migration can write it
// — a migration could only store material the readers cannot open. This is the
// first point after the pool and the master key are both settled, and it is
// before the listeners accept a request, so no project is asked for its
// X-SECRET value while the pass is still running.
//
// IT NEVER STOPS THE SERVICE. A project the pass does not reach has no X-SECRET
// value, and the version-details route then refuses every caller for it (#408
// step 3) rather than accepting the guessable literal. That is a narrow, loud
// and repairable outage on one route, so a failed pass must not keep the whole
// service down. It is logged at error level with the counts it reached, because
// a silent pass is exactly how the operator would come to believe work happened
// that did not.
//
// IT IS CHEAP TO RE-RUN. Every project that holds a value is counted and left
// alone, so the second start reads the vaults and writes nothing.
func backfillProjectSecretsHeaderValues(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	if pool == nil {
		return
	}
	report, err := v2secrets.NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "the project X-SECRET backfill did not finish; "+
			"the version details route refuses every caller for the projects it did not reach",
			"projects", report.Projects,
			"vaults_created", report.VaultsCreated,
			"written", report.Written,
			"already_set", report.AlreadySet,
			"skipped", report.Skipped,
			"error", err)
		return
	}
	if report.SkippedLocked {
		// Another replica holds the lock and is doing the pass. This one starts
		// serving rather than queueing behind it — see the doc on
		// BackfillProjectSecretsHeaderValues for why waiting would put an
		// O(projects) sequence of Fernet operations on the readiness path of
		// every replica an autoscaler adds.
		logger.InfoContext(ctx, "another replica is running the project X-SECRET backfill; skipping it here")
		return
	}
	if report.Written == 0 && report.Skipped == 0 {
		logger.InfoContext(ctx, "every project vault already holds an X-SECRET value",
			"projects", report.Projects)
		return
	}
	logger.InfoContext(ctx, "wrote an X-SECRET value into the project vaults that had none",
		"projects", report.Projects,
		"vaults_created", report.VaultsCreated,
		"written", report.Written,
		"already_set", report.AlreadySet,
		"skipped", report.Skipped)
}

// startPipelineScheduleRunner registers `pipeline.schedule.scan.v1` on the
// platform scheduler and starts it.
//
// It builds its OWN registry and runner rather than joining the index scan's,
// and that is a deliberate separation of failure domains rather than an
// oversight. The index runner is configured `MaxParallel: 1, PageSize: 1`
// because one index scan observes every project and a second concurrent claim
// only makes the product runner release occurrences for retry. A pipeline
// schedule pass has no such property, and more importantly a tenant workload
// that is slow or wedged must not delay the platform's own indexing — the
// coupling issue 193 names as the reason not to put tenant work in the ops
// scheduler in the first place. Two runners over ONE occurrence store, keyed by
// distinct job ids, keep the lease semantics and drop the coupling.
func startPipelineScheduleRunner(
	ctx context.Context,
	pool *pgxpool.Pool,
	handler *v2pipelinetriggers.Handler,
	instanceID string,
	logger *slog.Logger,
) error {
	if pool == nil || handler == nil {
		return nil
	}
	job, err := v2pipelinetriggers.NewScheduleJob(handler)
	if err != nil {
		return fmt.Errorf("construct pipeline schedule job: %w", err)
	}
	cadence, err := schedulingapp.ParseCron(v2pipelinetriggers.ScheduleJobCadence)
	if err != nil {
		return fmt.Errorf("parse pipeline schedule cadence: %w", err)
	}
	config := schedulingapp.Config{
		// Not `instanceID` directly: the caller passes
		// runtimeConfig.SchedulerInstanceID, which is EMPTY unless
		// ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED is on, and an empty name
		// makes NewRunner below refuse — which used to stop the whole process.
		// See pipeline_schedule_instance.go.
		InstanceID:      pipelineScheduleInstanceID(instanceID),
		LeaseDuration:   2 * time.Minute,
		MaxParallel:     1,
		PageSize:        1,
		MaxPagesPerTick: 2,
	}
	registry, err := schedulingapp.NewRegistry(config.LeaseDuration, schedulingapp.Job{
		ID:       v2pipelinetriggers.ScheduleJobID,
		Revision: v2pipelinetriggers.ScheduleJobRevision,
		Mode:     schedulingapp.ModeDurableAdmission,
		Schedule: cadence,
		Timeout:  v2pipelinetriggers.ScheduleJobTimeout,
		Handler:  job,

		Description: "Starts the pipeline runs whose schedules are due.",
		Cadence:     v2pipelinetriggers.ScheduleJobCadence,
		MinInterval: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("register pipeline schedule job: %w", err)
	}
	occurrences, err := dbrepos.NewScheduleOccurrenceRepository(pool)
	if err != nil {
		return fmt.Errorf("construct pipeline schedule occurrence repository: %w", err)
	}
	runner, err := schedulingapp.NewRunner(occurrences, registry, config, logger)
	if err != nil {
		return fmt.Errorf("construct pipeline schedule runner: %w", err)
	}
	controls, err := dbrepos.NewPlatformRoutineControls(pool)
	if err != nil {
		return fmt.Errorf("construct pipeline schedule routine controls: %w", err)
	}
	runner.WithControls(controls)
	// Run returns only when ctx is cancelled, which is process shutdown. The
	// error is logged rather than dropped: a runner that stopped early would
	// otherwise take every tenant's schedules with it, silently.
	go func() {
		if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("the pipeline schedule runner stopped", "err", err)
		}
	}()
	return nil
}

// introspectionTokenValidator is the PAT validator of elitea-vector's token
// introspection on the platform listener: the form graph when there is one,
// else the APPLICATION_SECRET_KEY validator, the pair that reads back what
// the callback minters sign (patSigner's rule). A nil INTERFACE when neither
// exists, never a typed nil, so the composition refuses it at boot.
func introspectionTokenValidator(formGraph *authcomposition.FormGraph, sessionTokens apimw.TokenValidator) vectorintrospection.TokenValidator {
	if formGraph != nil {
		return formGraph
	}
	if sessionTokens != nil {
		return sessionTokens
	}
	return nil
}
