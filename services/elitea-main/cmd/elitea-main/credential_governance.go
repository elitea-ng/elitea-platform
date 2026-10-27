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
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/vault"
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
		// map itself is built for the credential broker (#1118), its only
		// consumer.
		var external *vault.Store
		if vault.Configured(lookup) {
			cfg, err := vault.ConfigFromEnv(lookup)
			if err != nil {
				return nil, err
			}
			if external, err = vault.New(cfg); err != nil {
				return nil, err
			}
		}
		checks = append(checks, vault.NewPrerequisite(external))
		if pool != nil {
			outbox, err := securityaudit.NewOutboxPrerequisite(pool)
			if err != nil {
				return nil, err
			}
			checks = append(checks, outbox)
		}
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
	prerequisites func(context.Context) ([]profile.Prerequisite, error),
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
	return credentialPolicyPut(ctx, args[1:], store, os.ReadFile, prerequisites, stdout, stderr)
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

// cliCredentialGovernancePrerequisites is the prerequisite set `put` checks a
// new revision against: the same governed dependencies boot would build.
func cliCredentialGovernancePrerequisites(lookup func(string) (string, bool)) func(context.Context) ([]profile.Prerequisite, error) {
	return credentialGovernancePrerequisites(lookup, nil)
}

// agentIdentityAdmin builds the binding and mandate admin API (#1112). It is
// nil with the flag off, and the router then mounts none of its routes.
func agentIdentityAdmin(admission profile.Admission, revision profile.PolicyRevision, pool *pgxpool.Pool) (*v2agentidentity.Handler, error) {
	if !admission.Governed() {
		return nil, nil
	}
	service, err := governance.New(governance.Config{
		Revisions: governance.FixedRevision(revision),
		Bindings:  repos.NewAgentIdentityBindings(pool),
		Mandates:  repos.NewMandates(pool),
		// No binding can activate until external agent identities exist
		// (#1151): an unknown external identity is never active.
		ExternalIdentities: agentidentity.FailClosedExternalIdentities{},
	})
	if err != nil {
		return nil, fmt.Errorf("construct agent identity governance: %w", err)
	}
	return v2agentidentity.New(service)
}
