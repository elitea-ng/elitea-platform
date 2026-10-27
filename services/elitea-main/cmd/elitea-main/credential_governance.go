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

	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2credentialgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/backends"
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

// agentIdentityAdmin builds the binding and mandate admin API (#1112). It is
// nil with the flag off, and the router then mounts none of its routes.
func agentIdentityAdmin(admission profile.Admission, revision profile.PolicyRevision, pool *pgxpool.Pool) (*v2agentidentity.Handler, error) {
	if !admission.Governed() {
		return nil, nil
	}
	// Every binding and mandate change appends its security audit event in
	// the same transaction; an append failure rolls the change back.
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, fmt.Errorf("construct agent identity security audit appender: %w", err)
	}
	service, err := governance.New(governance.Config{
		Revisions: governance.FixedRevision(revision),
		Bindings:  repos.NewAgentIdentityBindings(pool, audit),
		Mandates:  repos.NewMandates(pool, audit),
		// No binding can activate until external agent identities exist
		// (#1151): an unknown external identity is never active.
		ExternalIdentities: agentidentity.FailClosedExternalIdentities{},
	})
	if err != nil {
		return nil, fmt.Errorf("construct agent identity governance: %w", err)
	}
	return v2agentidentity.New(service)
}

// credentialGrantGovernance builds the AND-composed operation decider and
// the credential grant admin API (#1116). Both are nil with the flag off:
// no decider exists and no grant route mounts. With the flag on, a
// construction error refuses boot.
//
// No request path calls the decider yet; the credential broker (#1118) is
// its first caller. The operation registry is empty until the adapter
// descriptors exist (#1120) and delegations are unusable until delegation
// custody exists (#1117), so every decision it could take today is a deny.
func credentialGrantGovernance(admission profile.Admission, pool *pgxpool.Pool, pins profile.PinReader) (*authzcompose.Decider, *v2credentialgrants.Handler, error) {
	if !admission.Governed() {
		return nil, nil, nil
	}
	if pool == nil || pins == nil {
		return nil, nil, errors.New("credential grant governance needs the platform database")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, nil, fmt.Errorf("construct credential grant security audit appender: %w", err)
	}
	grants := repos.NewCredentialGrants(pool, audit)
	decider, err := authzcompose.NewGovernedDecider(authzcompose.Dependencies{
		AuthSources:  authzcompose.DefaultAuthSources{},
		Permissions:  legacyrbac.NewPostgresResolver(pool),
		EntityAccess: repos.NewCredentialGrantEntityAccess(pool),
		Bindings:     repos.NewAgentIdentityBindings(pool, audit),
		Toolkits:     grants,
		Grants:       grants,
		References:   grants,
		Operations:   credentialgrant.EmptyRegistry,
		Mandates:     repos.NewMandates(pool, audit),
		Delegations:  authzcompose.NoDelegations{},
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
