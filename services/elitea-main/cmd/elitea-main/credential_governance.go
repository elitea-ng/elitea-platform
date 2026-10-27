package main

// The enterprise credential governance switch (ADR-0039, #1111) and its
// operator command.
//
// Boot: ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED decides the profile
// every admission is pinned to. Off, boot runs one probe for governed pins
// left by an earlier governed boot and nothing else. On, the active revision
// must exist, hash to its digest, allow an identity mode and satisfy every
// governed prerequisite, or the process refuses to start.
//
// Operator command, in the same image:
//
//	elitea-main credential-policy put --id 2026-10-r1 --file policy.json --operator alice
//	elitea-main credential-policy show --id 2026-10-r1
//
// `put` only appends a revision; a revision is never edited. It runs the same
// activation checks boot runs (profile spec item 1.7), with the
// prerequisites this process can build from its environment.

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentidentitycore "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2credentialgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/cache"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/backends"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/pgvault"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	credentialPolicyCommand    = "credential-policy"
	credentialPolicyRunTimeout = 2 * time.Minute
	maxPolicyFileBytes         = 64 * 1024
)

// credentialGovernanceAtBoot resolves the admission profile for this process.
func credentialGovernanceAtBoot(
	ctx context.Context,
	lookup func(string) (string, bool),
	store *repos.CredentialGovernanceStore,
	prerequisites func(context.Context) ([]profile.Prerequisite, error),
) (profile.Admission, profile.PolicyRevision, error) {
	cfg, err := profile.ConfigFromEnv(lookup)
	if err != nil {
		return profile.Admission{}, profile.PolicyRevision{}, err
	}
	if store == nil {
		return profile.Admission{}, profile.PolicyRevision{}, errors.New("credential governance store is required")
	}
	// Prerequisites are built only with the flag on: off, no governed
	// dependency reads its environment or dials anything (flag-off contract
	// item 4).
	var checks []profile.Prerequisite
	if cfg.Enabled && prerequisites != nil {
		if checks, err = prerequisites(ctx); err != nil {
			return profile.Admission{}, profile.PolicyRevision{}, fmt.Errorf("%w: %w", profile.ErrBootRefused, err)
		}
	}
	return profile.Boot(ctx, cfg, store, store, checks)
}

// credentialGovernancePrerequisites builds the governed dependencies' boot
// checks. Called only with the flag on.
func credentialGovernancePrerequisites(lookup func(string) (string, bool), pool *pgxpool.Pool) func(context.Context) ([]profile.Prerequisite, error) {
	return func(ctx context.Context) ([]profile.Prerequisite, error) {
		var checks []profile.Prerequisite
		// The Vault/OpenBao readiness check (#1113). Unconfigured Vault is
		// refused only by a revision that requires it for a class. The store
		// itself is built for the credential broker (#1118), its only consumer.
		_, vaultReady, err := backends.VaultFromEnv(lookup)
		if err != nil {
			return nil, err
		}
		checks = append(checks, vaultReady)
		// Boot and `credential-policy put` check the same outbox: put on a
		// database without 0183 is refused, not deferred to the next boot.
		if pool == nil {
			// A nil *pgxpool.Pool boxed into the Querier interface would pass
			// the constructor's nil check and panic at the first query.
			return nil, errors.New("credential governance prerequisites need the platform database")
		}
		outbox, err := securityaudit.NewOutboxPrerequisite(pool)
		if err != nil {
			return nil, err
		}
		checks = append(checks, outbox)
		return checks, nil
	}
}

// startSecurityAuditRelay runs the security audit relay (#1114) for a
// governed process. Flag off: no relay goroutine starts.
func startSecurityAuditRelay(ctx context.Context, admission profile.Admission, revision profile.PolicyRevision, pool *pgxpool.Pool, logger *slog.Logger) error {
	if !admission.Governed() {
		return nil
	}
	relay, err := securityaudit.NewRelay(pool, securityaudit.RelayConfig{
		BacklogAlertThreshold: revision.Document.AuditBacklogAlertThreshold,
		Logger:                logger,
	})
	if err != nil {
		return fmt.Errorf("construct the security audit relay: %w", err)
	}
	go relay.Run(ctx)
	return nil
}

// credentialPolicyStore is what the operator command needs.
type credentialPolicyStore interface {
	profile.RevisionStore
}

func runCredentialPolicy(
	ctx context.Context,
	args []string,
	lookup func(string) (string, bool),
	stdout, stderr io.Writer,
	prerequisites func(*pgxpool.Pool) func(context.Context) ([]profile.Prerequisite, error),
) int {
	if len(args) == 0 || (args[0] != "put" && args[0] != "show") {
		_, _ = fmt.Fprintln(stderr, "usage: elitea-main credential-policy put --id ID --file FILE --operator NAME | show --id ID")
		return 2
	}
	dsn, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_URL is required")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, credentialPolicyRunTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return 1
	}
	defer pool.Close()
	store, err := repos.NewCredentialGovernanceStore(pool)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if args[0] == "show" {
		return credentialPolicyShow(ctx, args[1:], store, stdout, stderr)
	}
	var checks func(context.Context) ([]profile.Prerequisite, error)
	if prerequisites != nil {
		checks = prerequisites(pool)
	}
	return credentialPolicyPut(ctx, args[1:], store, os.ReadFile, checks, stdout, stderr)
}

func credentialPolicyShow(ctx context.Context, args []string, store credentialPolicyStore, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("credential-policy show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "revision id")
	if err := flags.Parse(args); err != nil || *id == "" {
		_, _ = fmt.Fprintln(stderr, "--id is required")
		return 2
	}
	revision, err := store.Revision(ctx, *id)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	status := "ok"
	if err := revision.VerifyDigest(); err != nil {
		status = "DIGEST MISMATCH"
	}
	_, _ = fmt.Fprintf(stdout, "id: %s\ndigest: sha256:%s (%s)\ncreated_by: %s\ncreated_at: %s\n%s\n",
		revision.ID, hex.EncodeToString(revision.Digest[:]), status, revision.CreatedBy,
		revision.CreatedAt.UTC().Format(time.RFC3339), revision.Canonical)
	if status != "ok" {
		return 1
	}
	return 0
}

func credentialPolicyPut(
	ctx context.Context,
	args []string,
	store credentialPolicyStore,
	readFile func(string) ([]byte, error),
	prerequisites func(context.Context) ([]profile.Prerequisite, error),
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("credential-policy put", flag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "new revision id, e.g. 2026-10-r1")
	file := flags.String("file", "", "policy document (elitea.credential-policy.v1 JSON)")
	operator := flags.String("operator", "", "operator identity recorded as created_by")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !profile.ValidRevisionID(*id) || *file == "" || strings.TrimSpace(*operator) == "" {
		_, _ = fmt.Fprintln(stderr, "--id (lowercase, 1..63 chars), --file and --operator are required")
		return 2
	}
	raw, err := readFile(*file)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "read the policy file failed")
		return 1
	}
	if len(raw) > maxPolicyFileBytes {
		_, _ = fmt.Fprintf(stderr, "policy file exceeds %d bytes\n", maxPolicyFileBytes)
		return 1
	}
	doc, canonical, err := profile.ParsePolicyDocument(raw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	revision := profile.PolicyRevision{
		ID: *id, Digest: profile.Digest(canonical), Document: doc,
		Canonical: canonical, CreatedBy: strings.TrimSpace(*operator),
	}
	var checks []profile.Prerequisite
	if prerequisites != nil {
		if checks, err = prerequisites(ctx); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := profile.ValidateForActivation(ctx, revision, checks); err != nil {
		_, _ = fmt.Fprintln(stderr, "refused:", err)
		return 1
	}
	if err := store.PutRevision(ctx, revision); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "stored %s sha256:%s\nactivate with %s=true %s=%s\n",
		revision.ID, hex.EncodeToString(revision.Digest[:]),
		profile.EnvEnabled, profile.EnvPolicyRevision, revision.ID)
	return 0
}

// agentIdentityComposition is what the governed composition root builds for
// ADR-0039 agent identity. Every field is nil with the flag off.
type agentIdentityComposition struct {
	// Admin serves the binding and mandate admin API (#1112).
	Admin *v2agentidentity.Handler
	// ExternalAdmin serves the external agent identity admin API (#1151).
	ExternalAdmin *v2agentidentity.ExternalHandler
	// Broker opens run-scoped agent sessions for admission and turns them
	// into credentials for the credential broker (#1151). Its only callers.
	Broker *agentidentitycore.Broker
	// Reconciler compares external agent identities with their providers
	// (#1154). Nil with the flag off, and with no provider configured.
	Reconciler *reconcile.Reconciler
}

// agentIdentityEpochNotifier passes the reconcile loop's committed epoch
// bumps to the invalidation hub (#1121), so replicas drop cached entries of
// a drifted identity at once. Correctness does not depend on it: every
// enforcement point re-reads the epoch. A nil hub is a nil notifier.
func agentIdentityEpochNotifier(hub *epoch.Hub) reconcile.Notifier {
	if hub == nil {
		return nil
	}
	return reconcileEpochNotifier{hub: hub}
}

type reconcileEpochNotifier struct{ hub *epoch.Hub }

func (n reconcileEpochNotifier) EpochChanged(ctx context.Context, change reconcile.EpochChange) {
	n.hub.Notify(ctx, epoch.Change{
		Epoch:     epoch.SecurityEpoch{Kind: epoch.Kind(change.Kind), ID: change.ID, Value: change.Epoch},
		ChangedAt: time.Now(),
	})
}

// securityInvalidationsAtBoot builds this replica's governed-credential
// invalidation hub (#1121). Nil with the flag off: nothing is signalled, no
// subscription is made.
func securityInvalidationsAtBoot(admission profile.Admission, logger *slog.Logger) (*epoch.Hub, error) {
	if !admission.Governed() {
		return nil, nil
	}
	hub, err := epoch.NewHub(epoch.HubOptions{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("construct the security invalidation hub: %w", err)
	}
	return hub, nil
}

// agentIdentityProviders is the adapter set the registry is built from. No
// adapter exists yet (Entra #1152, Elitea-native #1153, others #1155), so
// the registry is empty: a governed session for a binding denies with
// ReasonProviderNotConfigured, and provisioning answers 503.
func agentIdentityProviders() []agentidentitycore.Provider { return nil }

// agentIdentityAdmin builds the agent identity admin APIs and the session
// broker. With the flag off it builds nothing, and the router then mounts
// none of the routes.
func agentIdentityAdmin(admission profile.Admission, revision profile.PolicyRevision, pool *pgxpool.Pool, invalidations *epoch.Hub) (agentIdentityComposition, error) {
	if !admission.Governed() {
		return agentIdentityComposition{}, nil
	}
	// Every binding, mandate, external identity and session change appends
	// its security audit event in the same transaction; an append failure
	// rolls the change back.
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return agentIdentityComposition{}, fmt.Errorf("construct agent identity security audit appender: %w", err)
	}
	bindings := repos.NewAgentIdentityBindings(pool, audit)
	identities := repos.NewExternalAgentIdentities(pool, audit)
	mandates := repos.NewMandates(pool, audit)
	if invalidations != nil {
		// Every committed epoch change signals the replicas (#1121).
		bindings.WithNotifier(invalidations)
		identities.WithNotifier(invalidations)
		mandates.WithNotifier(invalidations)
	}
	revisions := governance.FixedRevision(revision)
	service, err := governance.New(governance.Config{
		Revisions:          revisions,
		Bindings:           bindings,
		Mandates:           mandates,
		ExternalIdentities: identities,
	})
	if err != nil {
		return agentIdentityComposition{}, fmt.Errorf("construct agent identity governance: %w", err)
	}
	admin, err := v2agentidentity.New(service)
	if err != nil {
		return agentIdentityComposition{}, err
	}
	registry, err := agentidentitycore.NewRegistry(agentIdentityProviders()...)
	if err != nil {
		return agentIdentityComposition{}, fmt.Errorf("construct agent identity provider registry: %w", err)
	}
	external, err := agentidentitycore.NewExternalIdentityService(agentidentitycore.ExternalIdentityConfig{
		Revisions: revisions, Store: identities, Registry: registry,
	})
	if err != nil {
		return agentIdentityComposition{}, fmt.Errorf("construct external agent identity service: %w", err)
	}
	reconcileStore := repos.NewAgentIdentityReconcile(pool, audit)
	externalAdmin, err := v2agentidentity.NewExternal(external, reconcileStore)
	if err != nil {
		return agentIdentityComposition{}, err
	}
	broker, err := agentidentitycore.NewBroker(agentidentitycore.BrokerConfig{
		Registry: registry,
		// Asserted user evidence is never accepted (gate D4): no revision
		// field enables it yet.
		Policy:     agentidentitycore.DenyAssertedUser{ModePolicy: revision.Document},
		Bindings:   bindings,
		Identities: identities,
		Sessions:   repos.NewAgentSessions(pool, audit),
		// A provider the reconcile loop reports past the outage threshold
		// opens no new session on any replica (#1154).
		ProviderHealth: reconcileStore,
	})
	if err != nil {
		return agentIdentityComposition{}, fmt.Errorf("construct agent identity broker: %w", err)
	}
	out := agentIdentityComposition{Admin: admin, ExternalAdmin: externalAdmin, Broker: broker}
	// The reconcile loop runs only for configured providers: with an empty
	// registry it is not built.
	if len(registry.Names()) > 0 {
		out.Reconciler, err = reconcile.New(reconcile.Config{
			Registry: registry, Store: reconcileStore, Interval: revision.Document.ReconcileInterval(),
			OutageThreshold: revision.Document.ReconcileOutageFailures(), Notifier: agentIdentityEpochNotifier(invalidations),
		})
		if err != nil {
			return agentIdentityComposition{}, fmt.Errorf("construct agent identity reconcile loop: %w", err)
		}
	}
	return out, nil
}

// credentialOperationCatalog is THE seam where the composition passes the
// operation catalogue: the provider adapter descriptors' operations, classes
// and material keys (#1120). Both the decider's scope gate and the credential
// broker read this one value. It is pure data: building it reads nothing.
func credentialOperationCatalog() credentialbroker.OperationCatalog {
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		// The built-in descriptors are fixed at compile time and a test
		// builds them, so this is a programming error, not a deployment one.
		panic(fmt.Sprintf("built-in credential adapter descriptors: %v", err))
	}
	return registry
}

// credentialGrantGovernance builds the AND-composed operation decider and
// the credential grant admin API (#1116). Both are nil with the flag off:
// no decider exists and no grant route mounts. With the flag on, a
// construction error refuses boot.
//
// delegations is the Entra delegation custody (#1117) when it is
// configured; nil keeps NoDelegations, so every delegation authority
// denies. operations is credentialOperationCatalog(). The credential broker
// (#1118) is the decider's only caller.
func credentialGrantGovernance(
	admission profile.Admission,
	pool *pgxpool.Pool,
	pins profile.PinReader,
	delegations authzcompose.DelegationReader,
	operations credentialgrant.OperationRegistry,
	invalidations *epoch.Hub,
) (*authzcompose.Decider, *v2credentialgrants.Handler, error) {
	if !admission.Governed() {
		return nil, nil, nil
	}
	if pool == nil || pins == nil || operations == nil {
		return nil, nil, errors.New("credential grant governance needs the platform database and an operation catalogue")
	}
	if delegations == nil {
		delegations = authzcompose.NoDelegations{}
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, nil, fmt.Errorf("construct credential grant security audit appender: %w", err)
	}
	grants := repos.NewCredentialGrants(pool, audit)
	if invalidations != nil {
		grants.WithNotifier(invalidations)
	}
	decider, err := authzcompose.NewGovernedDecider(authzcompose.Dependencies{
		AuthSources:  authzcompose.DefaultAuthSources{},
		Permissions:  legacyrbac.NewPostgresResolver(pool),
		EntityAccess: repos.NewCredentialGrantEntityAccess(pool),
		Bindings:     repos.NewAgentIdentityBindings(pool, audit),
		Toolkits:     grants,
		Grants:       grants,
		References:   grants,
		Operations:   operations,
		Mandates:     repos.NewMandates(pool, audit),
		Delegations:  delegations,
		Pins:         pins,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct the credential operation decider: %w", err)
	}
	service, err := credentialgrant.NewService(credentialgrant.ServiceConfig{Store: grants})
	if err != nil {
		return nil, nil, fmt.Errorf("construct credential grant governance: %w", err)
	}
	handler, err := v2credentialgrants.New(service)
	if err != nil {
		return nil, nil, err
	}
	return decider, handler, nil
}

// entraDelegations is the decider's delegation reader: the Entra custody
// store when it is configured, else nil (credentialGrantGovernance then
// keeps NoDelegations). It returns a real nil, never a nil *Store boxed in
// the interface.
func entraDelegations(entra *entraGovernance) authzcompose.DelegationReader {
	if entra == nil || entra.Delegations == nil {
		return nil
	}
	return entra.Delegations
}

// credentialBrokerAtBoot builds the governed credential broker (#1118). Nil
// with the flag off: RedeemCredential then answers Unimplemented and no
// statement runs. With the flag on every dependency is required, and a
// construction error refuses boot.
func credentialBrokerAtBoot(
	admission profile.Admission,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	decider *authzcompose.Decider,
	pins profile.PinReader,
	sessions *agentidentitycore.Broker,
	entra *entraGovernance,
	catalog credentialbroker.OperationCatalog,
	revision profile.PolicyRevision,
	invalidations *epoch.Hub,
	logger *slog.Logger,
) (*credentialbroker.Broker, error) {
	if !admission.Governed() {
		return nil, nil
	}
	if pool == nil || decider == nil || pins == nil || sessions == nil || catalog == nil {
		return nil, errors.New("the credential broker needs the database, the decider, the pin reader, the agent session broker and the operation catalogue")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, fmt.Errorf("construct credential broker security audit appender: %w", err)
	}
	// The built-in vault (pgvault) is the default backend; Vault/OpenBao
	// joins it when configured (#1113).
	masterKey, err := encodedVaultMasterKeyFromEnv(lookup)
	if err != nil {
		return nil, fmt.Errorf("credential broker vault: %w", err)
	}
	vaultReader, err := storage.NewPostgresSecretVaultLoader(pool, masterKey)
	if err != nil {
		return nil, fmt.Errorf("credential broker vault: %w", err)
	}
	vaultWriter, err := repos.NewCurrentSecretVaultRepository(pool, masterKey)
	if err != nil {
		return nil, fmt.Errorf("credential broker vault: %w", err)
	}
	pg, err := pgvault.New(pool, vaultReader, vaultWriter)
	if err != nil {
		return nil, fmt.Errorf("credential broker vault: %w", err)
	}
	stores, _, err := backends.NewFromEnv(lookup, pg)
	if err != nil {
		return nil, fmt.Errorf("credential broker secret stores: %w", err)
	}
	store, err := credentialbroker.NewPostgresStore(pool, audit)
	if err != nil {
		return nil, err
	}
	grants := repos.NewCredentialGrants(pool, audit)
	cfg := credentialbroker.Config{
		Decider:      decider,
		References:   grants,
		Targets:      grants,
		Toolkits:     grants,
		Bindings:     repos.NewAgentIdentityBindings(pool, audit),
		Mandates:     repos.NewMandates(pool, audit),
		Delegations:  authzcompose.NoDelegations{},
		SecretStores: stores,
		Store:        store,
		Pins:         pins,
		Sessions:     sessions,
		Catalog:      catalog,
		Logger:       logger,
	}
	// Revocation epochs, the credential cache and its invalidation (#1121).
	// The cache's bounds are the active revision's; whether a run may reuse
	// a static secret is its own pinned revision's.
	if cfg.Epochs, err = epoch.NewPostgresStore(pool); err != nil {
		return nil, err
	}
	settings := revision.Document.CacheSettings()
	if cfg.Cache, err = cache.New(cache.Limits{MaxEntries: settings.MaxEntries, MaxBytes: settings.MaxBytes, Skew: settings.Skew}, nil); err != nil {
		return nil, fmt.Errorf("construct the credential cache: %w", err)
	}
	cfg.Invalidations = invalidations
	if revisions, ok := pins.(profile.RevisionStore); ok {
		cfg.Revisions = revisions
	}
	if delegations := entraDelegations(entra); delegations != nil {
		cfg.Delegations = delegations
		// Classic OBO first: Agent OBO denies every exchange until the Entra
		// agent identity adapter (#1152) supplies its client assertion.
		switch {
		case entra.ClassicOBO != nil:
			cfg.Exchanger, cfg.Custody = entra.ClassicOBO, entra.Delegations
		case entra.AgentOBO != nil:
			cfg.Exchanger, cfg.Custody = entra.AgentOBO, entra.Delegations
		}
	}
	broker, err := credentialbroker.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("construct the credential broker: %w", err)
	}
	return broker, nil
}

// entraGovernance is the governed Entra dependency set (#1117): delegation
// custody and the on-behalf-of exchangers the credential broker (#1118)
// consumes. Nil with the flag off, or when the enabled OIDC provider is not
// marked `entra`. Either exchanger may be nil: classic OBO needs a
// federated credential, Agent OBO an agent blueprint.
type entraGovernance struct {
	Delegations *delegationstore.Store
	ClassicOBO  *obo.ClassicOBO
	AgentOBO    *obo.AgentOBO
}

// entraGovernanceAtBoot builds the Entra dependencies. It runs nothing with
// the flag off (flag-off contract item 4): no provider read, no key derived,
// nothing constructed. The provider is read once here; turning a provider
// into an Entra provider takes effect at the next boot.
func entraGovernanceAtBoot(
	ctx context.Context,
	admission profile.Admission,
	pool *pgxpool.Pool,
	providers v2auth.IdentityProviderSource,
	masterKey func() ([]byte, error),
	invalidations *epoch.Hub,
	logger *slog.Logger,
) (*entraGovernance, error) {
	if !admission.Governed() || providers == nil {
		return nil, nil
	}
	provider, err := providers.Enabled(ctx, identityproviders.KindOIDC)
	switch {
	case err == nil:
	case errors.Is(err, identityproviders.ErrNotFound), identityproviders.IsSchemaMissing(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("read the enabled identity provider for Entra governance: %w", err)
	}
	if provider.OIDC == nil || !provider.OIDC.Entra {
		return nil, nil
	}
	document := provider.OIDC
	policy, err := identityoidc.NewEntraPolicy(document.Issuer, document.ClientID, document.EntraTenants, document.EntraAPIScope)
	if err != nil {
		return nil, fmt.Errorf("the Entra settings of provider %q are invalid: %w", provider.Key, err)
	}
	key, err := masterKey()
	if err != nil {
		return nil, fmt.Errorf("delegation custody for Entra needs the vault master key: %w", err)
	}
	defer clear(key)
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	store, err := delegationstore.New(pool, key, audit)
	if err != nil {
		return nil, fmt.Errorf("construct Entra delegation custody: %w", err)
	}
	if invalidations != nil {
		store.WithNotifier(invalidations)
	}
	governed := &entraGovernance{Delegations: store}

	// Client secrets are refused: policy schema v1 has no field that allows
	// them, so only a federated credential authenticates the middle tier.
	if path := document.EntraFederatedTokenFile; path != "" {
		governed.ClassicOBO, err = obo.NewClassicOBO(obo.ClassicConfig{
			Authority:      policy.Authority(),
			ClientID:       policy.ClientID(),
			AllowedTenants: policy.AllowedTenants(),
			Credential: obo.FederatedCredential(func(context.Context) (string, error) {
				raw, err := os.ReadFile(path)
				return string(raw), err
			}),
		})
		if err != nil {
			return nil, fmt.Errorf("construct the classic OBO exchanger: %w", err)
		}
	} else {
		logger.Warn("Entra classic OBO is not available: the provider names no federated token file",
			"provider", provider.Key)
	}
	if blueprint := document.EntraAgentBlueprintID; blueprint != "" {
		// The agent client assertion source is the Entra agent identity
		// adapter (#1152). Until it exists every Agent OBO exchange is
		// denied with obo.ErrAgentAssertionUnavailable.
		governed.AgentOBO, err = obo.NewAgentOBO(obo.AgentConfig{
			Authority:      policy.Authority(),
			BlueprintAppID: blueprint,
			AllowedTenants: policy.AllowedTenants(),
		})
		if err != nil {
			return nil, fmt.Errorf("construct the Agent OBO exchanger: %w", err)
		}
		for _, audience := range governed.AgentOBO.Audiences() {
			if slices.Contains(policy.AssertionAudiences(), audience) {
				return nil, fmt.Errorf("the Entra agent blueprint of provider %q is the middle tier; the two audiences must differ", provider.Key)
			}
		}
	}
	logger.Info("Entra delegation custody enabled", "provider", provider.Key,
		"classic_obo", governed.ClassicOBO != nil, "agent_obo", governed.AgentOBO != nil)
	return governed, nil
}
