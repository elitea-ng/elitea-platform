package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// SecretsHeaderValueName is the project-vault secret the `X-SECRET` request
// header is compared against.
//
// pylon's `check_secret_header` reads
// `secrets.get("secrets_header_value", "secret")`
// (legacy/plugins/elitea_core/utils/secrets.py:4-9). A project whose vault
// holds no value under this name therefore accepts the literal string
// "secret". api/v2/applications reproduced that fallback for parity until #408
// closed it.
//
// This file removes the REASON for the fallback (#408 steps 1 and 2). The
// provisioner writes a random value into every new project vault, and
// BackfillProjectSecretsHeaderValues writes one into every project vault that
// already exists. The fallback itself is now GONE (#408 step 3): a project with
// no value refuses every caller of the version-details route
// (internal/api/v2/applications/handler.go, secretHeaderRefusal), and the
// worker carries the project's own value to the SDK instead of the literal
// (internal/infra/storage/index_runtime_context.go).
const SecretsHeaderValueName = "secrets_header_value"

// secretsHeaderValueBytes is how much entropy one generated value carries. 32
// bytes is the same width as a Fernet key, and the base64url text of it is 43
// characters, all of which are legal in an HTTP header value.
const secretsHeaderValueBytes = 32

// NewSecretsHeaderValue returns one header value from the operating system's
// cryptographically secure source.
//
// The text is base64url with no padding. A header value must survive the SDK,
// Traefik and pylon unchanged, so the alphabet holds no character that any of
// them quotes, folds or strips.
func NewSecretsHeaderValue() (string, error) {
	raw := make([]byte, secretsHeaderValueBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a secrets header value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// EnsureProjectSecretsHeaderValue writes a random `secrets_header_value` into
// one project vault, and reports whether it wrote one.
//
// It NEVER replaces a value that is already there. The value is a shared
// credential: the SDK sends it on every sub-agent call, so a rewrite would
// refuse calls that are in flight. A second call on the same project therefore
// reports `false, nil`, and so does a project whose owner set the value by
// hand.
//
// The vault must exist. This function does not create one, for the reason
// StoreProjectSecrets does not: an absent vault means the caller ran before
// the project_secrets provisioning step, and minting a vault here would put a
// second minter beside EnsureProjectVault. The provisioning step calls
// EnsureProjectVault first, and the backfill reads the vault table to find its
// work, so neither caller can reach this with no vault.
//
// An UNREADABLE vault is an error and is never overwritten, exactly as every
// other write path in this package treats one.
//
// The check and the write are ONE locked read-modify-write (mutateVaultByID).
// The backfill's advisory lock serialises the pass against other replicas of
// the pass; it is the row lock here that serialises this write against a
// secret being written through the secrets API in the same instant, which used
// to be clobbered entirely (#858).
func (h *Handler) EnsureProjectSecretsHeaderValue(ctx context.Context, projectID string) (bool, error) {
	vaultID := dbKey(projectID)
	written := false
	err := h.mutateVaultCtx(ctx, projectID, false, func(vault *vaultData) (bool, error) {
		// Both maps are consulted because ResolveSecretValue reads both, in
		// this order. A value hidden by the Hide route still answers the
		// `X-SECRET` check, so it counts as set.
		if strings.TrimSpace(vault.Secrets[SecretsHeaderValueName]) != "" ||
			strings.TrimSpace(vault.HiddenSecrets[SecretsHeaderValueName]) != "" {
			return false, nil
		}
		value, err := NewSecretsHeaderValue()
		if err != nil {
			return false, err
		}
		vault.Secrets[SecretsHeaderValueName] = value
		written = true
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("ensure the %s secrets header value: %w", vaultID, err)
	}
	return written, nil
}

// ResolveProjectSecretsHeaderValue reads one project's `X-SECRET` value by its
// numeric id.
//
// It is the runtime's reader (storage.ProjectSecretsHeaderReader): the
// worker gets the value with its claim-bound bearer token and puts it on every
// SDK call, so the SDK stops sending the pylon literal "secret" (#408).
//
// It refuses a non-positive id rather than building a vault name from it. The
// caller is the runtime, whose project id comes from an authorized claim, so a
// zero here is a fault in that path and not a project.
func (h *Handler) ResolveProjectSecretsHeaderValue(ctx context.Context, projectID int64) (string, error) {
	if h == nil || projectID < 1 {
		return "", fmt.Errorf("resolve the secrets header value: the project id %d is not valid", projectID)
	}
	value, err := h.ResolveSecretValue(ctx, strconv.FormatInt(projectID, 10), SecretsHeaderValueName)
	if err != nil {
		return "", fmt.Errorf("resolve the p_%d secrets header value: %w", projectID, err)
	}
	return value, nil
}

// SecretsHeaderBackfillReport counts what one backfill pass did. The caller
// logs it, so an operator can state how many projects the pass touched.
type SecretsHeaderBackfillReport struct {
	// Projects is how many projects the pass examined.
	//
	// It counts PROJECTS and not vaults, because a project with no vault is
	// the state this pass repairs rather than a project it may pass over. See
	// VaultsCreated.
	Projects int
	// VaultsCreated is how many of them had no vault and were given an empty
	// one before the value was sealed into it.
	//
	// A project row can exist with no vault whenever something other than
	// projectprovisioning created it — a database restored from a dump that
	// carried centry.project and not centry.secrets_key, an import, or a test
	// stack that inserts its fixture projects directly. Such a project used to
	// be invisible to this pass, so it never got an X-SECRET value and every
	// sub-agent call inside it was refused for the life of the deployment.
	VaultsCreated int
	// Written is how many of them received a new value.
	Written int
	// AlreadySet is how many of them held one already.
	AlreadySet int
	// Skipped is how many of them could not be opened. Each one is logged
	// with its vault id. A value greater than zero means those projects have
	// no X-SECRET value, so the version-details route refuses every caller for
	// them (#408). The count must never be read as noise.
	Skipped int
	// SkippedLocked reports that another replica held the advisory lock and
	// this pass did nothing at all. Without it, "another replica is doing it"
	// and "every vault already had a value" are the same all-zero report.
	SkippedLocked bool
}

// BackfillProjectSecretsHeaderValues gives a `secrets_header_value` to every
// project that has no value (#408 step 2), CREATING the vault when the project
// has none.
//
// THE VAULT IS PART OF THE WORK, not a precondition. The pass used to enumerate
// vaults, so a project row created by anything other than projectprovisioning —
// a restored dump, an import, a test stack that inserts its fixture projects
// directly — was never reached, and no later start could reach it either: the
// value is only ever written into a vault, and nothing else was going to make
// one. That project then refused every version-details read for ever, which is
// the SDK call a nested agent is materialized by, so an agent attached to
// another agent silently contributed no tool at all.
//
// IT IS GO AND NOT A MIGRATION, and it cannot be a migration. The value is
// sealed with the project's Fernet key, which is itself wrapped with
// SECRETS_MASTER_KEY. SQL cannot open the vault, so a migration could only
// write a value the readers cannot read. The one minter rule (#399/#411) puts
// every vault write in this handler, and this pass is a vault write.
//
// IT IS IDEMPOTENT. A project that holds a value keeps it, so the pass may run
// on every start, and a project provisioned after the pass gets its value from
// the provisioning step instead.
//
// A VAULT THAT WILL NOT OPEN DOES NOT STOP THE PASS. Such a vault is a
// pre-existing condition — a key wrapped with a master key this deployment no
// longer sets — and refusing to give the other projects a value would help
// nobody. Each one is logged and counted in Skipped.
//
// A DATABASE FAULT DOES stop the pass. The two are told apart by a ping: a
// vault that fails while the pool still answers is that vault's problem, and a
// vault that fails while the pool does not answer is the deployment's. Without
// that split, a database that goes away in the middle of the pass would report
// every remaining project as skipped and the pass as a success.
// backfillLockKey keys the advisory lock this pass serialises on.
//
// A constant, so every replica of every elitea-main in the cluster contends on
// the same one. Advisory locks are per-database, which is the scope wanted
// here: two replicas share a database, and it is the database's rows they race
// on.
//
// THIS LOCK REQUIRES A SESSION. A session-scoped advisory lock belongs to the
// PostgreSQL backend that took it, and a connection pooler in transaction mode
// hands that backend to somebody else between statements. Put a pgbouncer with
// `pool_mode = transaction` in front of this deployment and the lock stops
// serialising — not with an error, but by letting every replica through, which
// is the exact defect it was added to close.
//
// MEASURED, not inferred (2026-09-01), against this package's own Postgres
// integration tests:
//
//	direct connection                                          PASS
//	pgbouncer transaction mode, max_prepared_statements = 0     FAIL, 42P05
//	pgbouncer transaction mode, max_prepared_statements = 200   FAIL, the lock
//	                                                            stopped
//	                                                            serialising
//	pgbouncer session mode                                      PASS
//
// So a pooler is not free here. If one is ever introduced, either keep it in
// session mode, or replace this lock first — a row in a locks table taken with
// SELECT ... FOR UPDATE, or moving the whole pass out of the boot path into a
// Job, which needs no lock at all. Do not simply add the pooler and watch this
// package's tests, because they run against a direct connection.
const backfillLockKey int64 = 0x5EC5E7BF // "SECSETBF"

// BackfillProjectSecretsHeaderValues writes an X-SECRET value into every
// project vault that has none.
//
// IT RUNS ON EXACTLY ONE REPLICA AT A TIME, and the lock is not an
// optimisation. EnsureProjectSecretsHeaderValue below is a read-modify-write
// of the WHOLE vault: it decrypts, checks one key, and re-encrypts everything
// back. Two replicas doing that concurrently do not merely duplicate work —
// they lose writes. Each generates a DIFFERENT random value and the last write
// wins, so a project's header value depends on which replica finished second
// and any client that read the first is refused. Worse, a legitimate secret
// written through the secrets API between one replica's read and its write is
// clobbered entirely.
//
// This runs before listeners bind, on every replica, and the chart's default
// is two with an autoscaler that may add eight more. So the race is the
// ordinary case on a cold start, not an unlucky one.
//
// TRY, NOT WAIT. A replica that loses the race skips the pass and starts
// serving; it does not queue behind the winner. The backfill is best-effort by
// contract — its caller logs and continues — and blocking here would put an
// O(projects) sequence of Fernet operations on the readiness path of every
// replica an autoscaler adds, which is exactly when latency is least
// affordable. The winner's writes are visible to the losers immediately; there
// is nothing for them to redo.
func (h *Handler) BackfillProjectSecretsHeaderValues(ctx context.Context) (SecretsHeaderBackfillReport, error) {
	var report SecretsHeaderBackfillReport
	if h == nil || h.pool == nil {
		return report, errors.New("backfill the secrets header values: there is no database pool")
	}

	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return report, fmt.Errorf("backfill the secrets header values: acquire connection: %w", err)
	}
	defer conn.Release()

	// A SESSION lock on a pinned connection: the pass is many statements over
	// its own pool connections and cannot run inside one transaction.
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_catalog.pg_try_advisory_lock($1)`, backfillLockKey).Scan(&acquired); err != nil {
		return report, fmt.Errorf("backfill the secrets header values: lock: %w", err)
	}
	if !acquired {
		// Reported, not silent. "Another replica is doing it" and "there was
		// nothing to do" produce the same counts, and only this field tells
		// them apart in a log.
		report.SkippedLocked = true
		return report, nil
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_catalog.pg_advisory_unlock($1)`, backfillLockKey); err != nil {
			slog.WarnContext(ctx,
				"the secrets header backfill lock was not released; it clears when this connection closes",
				"error", err)
		}
	}()

	projectIDs, err := h.backfillProjectIDs(ctx)
	if err != nil {
		return report, err
	}
	report.Projects = len(projectIDs)
	for _, projectID := range projectIDs {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("backfill the secrets header values: %w", err)
		}
		// The vault first, because EnsureProjectSecretsHeaderValue writes into
		// one and never creates one. A project that already has a vault is
		// read and left alone, so this costs one vault read on the ordinary
		// path and is the whole repair on the other.
		created, vaultErr := h.ensureBackfillVault(ctx, projectID)
		if vaultErr != nil {
			if pingErr := h.pool.Ping(ctx); pingErr != nil {
				return report, fmt.Errorf(
					"backfill the secrets header values: the database stopped answering: %w", vaultErr)
			}
			report.Skipped++
			slog.WarnContext(ctx,
				"the project vault could not be created, so the project gets no X-SECRET value "+
					"and the version details route refuses every caller for it",
				"project_id", projectID,
				"variable", MasterKeyEnvVar,
				"error", vaultErr)
			continue
		}
		if created {
			report.VaultsCreated++
		}
		written, ensureErr := h.EnsureProjectSecretsHeaderValue(ctx, projectID)
		switch {
		case ensureErr == nil && written:
			report.Written++
		case ensureErr == nil:
			report.AlreadySet++
		default:
			if pingErr := h.pool.Ping(ctx); pingErr != nil {
				return report, fmt.Errorf(
					"backfill the secrets header values: the database stopped answering: %w", ensureErr)
			}
			report.Skipped++
			slog.WarnContext(ctx,
				"the project vault would not open, so the project gets no X-SECRET value "+
					"and the version details route refuses every caller for it",
				"project_id", projectID,
				"variable", MasterKeyEnvVar,
				"error", ensureErr)
		}
	}
	return report, nil
}

// ensureBackfillVault gives one project an empty vault when it has none, and
// reports whether it made one.
//
// The "already there" answer is read from the vault rather than assumed from
// the enumeration: two replicas that both lost the advisory-lock race are not
// the only writers of centry.secrets_key, and EnsureProjectVault is the one
// minter (#399/#411), so the decision is left to it and only the COUNT is made
// here.
func (h *Handler) ensureBackfillVault(ctx context.Context, projectID string) (bool, error) {
	_, err := h.readVaultCtx(ctx, projectID)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, ErrVaultAbsent):
		if err := h.EnsureProjectVault(ctx, projectID); err != nil {
			return false, err
		}
		return true, nil
	default:
		// A vault that exists and will not open. EnsureProjectSecretsHeaderValue
		// refuses it too, and it is counted as skipped there — so it is passed
		// through rather than reported as a vault fault here.
		return false, nil
	}
}

// backfillProjectIDs lists the projects the pass must reach.
//
// It reads centry.project and not centry.secrets_key, so a project with no
// vault is included; an ORPHAN vault — rows left by a project that is gone — is
// excluded by construction, because it names no project. RemoveProjectVault
// deletes those rows, and a vault that outlived its project is adopted by the
// next project that draws the same id, so writing a value into one would seal
// material into a vault the next owner inherits.
//
// The three tables are checked with to_regclass first. An empty database has
// none of them, and the service must start against one.
func (h *Handler) backfillProjectIDs(ctx context.Context) ([]string, error) {
	var present bool
	if err := h.pool.QueryRow(ctx,
		`SELECT to_regclass('centry.secrets_key') IS NOT NULL
		    AND to_regclass('centry.secrets_data') IS NOT NULL
		    AND to_regclass('centry.project') IS NOT NULL`,
	).Scan(&present); err != nil {
		return nil, fmt.Errorf("look for the project vault tables: %w", err)
	}
	if !present {
		return nil, nil
	}
	rows, err := h.pool.Query(ctx,
		`SELECT p.id::text
		   FROM centry.project AS p
		  ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("list the projects: %w", err)
	}
	defer rows.Close()

	var projectIDs []string
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			return nil, fmt.Errorf("read a project row: %w", err)
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the project rows: %w", err)
	}
	return projectIDs, nil
}
