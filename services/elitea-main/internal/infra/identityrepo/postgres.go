// Package identityrepo persists authenticated identity provisioning against the
// current auth_core tables.
package identityrepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

const projectViewerRole = "viewer"

const rollbackTimeout = 5 * time.Second

var _ identity.Repository = (*PostgresRepository)(nil)

// PostgresRepository owns the single transaction that maps an IdP identity,
// updates its current user, and applies the new_ai_user project policy.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) (*PostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("identityrepo: PostgreSQL pool is required")
	}
	return &PostgresRepository{pool: pool}, nil
}

func (r *PostgresRepository) Provision(
	ctx context.Context,
	command identity.ProvisionCommand,
) (identity.ProvisionResult, error) {
	if r == nil || r.pool == nil {
		return identity.ProvisionResult{}, errors.New("identityrepo: PostgreSQL pool is unavailable")
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return identity.ProvisionResult{}, fmt.Errorf("identityrepo: begin provisioning: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	queries := sqlcgen.New(tx)
	if err := queries.AcquireAuthProviderAdvisoryLock(ctx, command.ProviderReference); err != nil {
		return identity.ProvisionResult{}, fmt.Errorf("identityrepo: lock provider identity: %w", err)
	}

	user, created, err := resolveProvisioningUser(ctx, queries, command)
	if err != nil {
		return identity.ProvisionResult{}, err
	}
	if user.Suspended {
		return identity.ProvisionResult{UserID: int64(user.ID), Suspended: true}, nil
	}

	if created {
		if _, err := queries.AddNewAuthUserToRootGroup(ctx, user.ID); err != nil {
			return identity.ProvisionResult{}, fmt.Errorf("identityrepo: add new user to root group: %w", err)
		}
	}

	user, err = queries.TouchProvisionedAuthUser(ctx, sqlcgen.TouchProvisionedAuthUserParams{
		Name:   command.Name,
		UserID: user.ID,
	})
	if err != nil {
		return identity.ProvisionResult{}, fmt.Errorf("identityrepo: update provisioned user: %w", err)
	}

	if err := reconcileProjectEnrollment(ctx, queries, user.ID, command.ProjectEnrollment); err != nil {
		return identity.ProvisionResult{}, err
	}
	// The current event delivery races with the subsequent initial-admin write.
	// Resolve that race deterministically in source-call order and least-
	// privilege order: project reconciliation observes only roles that existed
	// before this login. A newly configured initial admin receives additional
	// project-admin roles on a later login.
	if err := applyInitialAdministrationRole(ctx, queries, user.ID, command); err != nil {
		return identity.ProvisionResult{}, err
	}
	// The credential the agent runtime signs this person's calls with.
	//
	// It runs HERE, inside the same transaction, for the same reason the
	// administration grant does: an interrupted login must leave neither a
	// half-granted administrator nor an orphan credential. The OIDC and SAML
	// plane does the identical pair in internal/api/v2/auth.applyFirstLoginGrants.
	// Until this call existed, the two browser planes left DIFFERENT rows
	// behind, and a Form-only install could sign a person in who then could not
	// complete a chat turn: authsvc.LocalIssuer re-signs an existing PAT and
	// never creates one, so the runtime failed at stage `actor_pat_issuance`,
	// an error that names a database stage for what was really missing setup.
	// The only cure was hand-written SQL in deploy/scripts/standalone-stack.sh.
	//
	// EnsureActorPAT owns the three decisions (name, no expiry, only when the
	// account holds no active PAT) and is idempotent, so every later login
	// spends one extra read here and writes nothing.
	patCreated, err := EnsureActorPAT(ctx, queries, user.ID)
	if err != nil {
		return identity.ProvisionResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return identity.ProvisionResult{}, fmt.Errorf("identityrepo: commit provisioning: %w", err)
	}
	if patCreated {
		slog.Info("issued an actor personal access token at first sign-in",
			"user_id", user.ID, "token_name", ActorPATName, "provider", command.Provider)
	}
	return identity.ProvisionResult{UserID: int64(user.ID)}, nil
}

func resolveProvisioningUser(
	ctx context.Context,
	queries *sqlcgen.Queries,
	command identity.ProvisionCommand,
) (sqlcgen.AuthCoreUser, bool, error) {
	user, err := queries.GetAuthUserByProviderForProvisioning(ctx, command.ProviderReference)
	if err == nil {
		return user, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: resolve provider identity: %w", err)
	}

	user, created, err := resolveProvisioningUserByEmail(ctx, queries, command.Email, command.Name)
	if err != nil {
		return sqlcgen.AuthCoreUser{}, false, err
	}
	if user.Suspended {
		return user, created, nil
	}

	if _, err := queries.LinkAuthProviderIfMissing(ctx, sqlcgen.LinkAuthProviderIfMissingParams{
		UserID:      user.ID,
		ProviderRef: command.ProviderReference,
	}); err != nil {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: link provider identity: %w", err)
	}
	linkedUser, err := queries.GetAuthUserByProviderForProvisioning(ctx, command.ProviderReference)
	if err != nil {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: reselect provider identity: %w", err)
	}
	if linkedUser.ID != user.ID {
		return sqlcgen.AuthCoreUser{}, false, errors.New("identityrepo: provider identity changed during provisioning")
	}
	return linkedUser, created, nil
}

func resolveProvisioningUserByEmail(
	ctx context.Context,
	queries *sqlcgen.Queries,
	email string,
	name string,
) (sqlcgen.AuthCoreUser, bool, error) {
	user, err := queries.GetAuthUserByEmailForProvisioning(ctx, email)
	if err == nil {
		return user, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: resolve user email: %w", err)
	}

	user, err = queries.CreateAuthUserByEmailIfMissing(ctx, sqlcgen.CreateAuthUserByEmailIfMissingParams{
		Email: email,
		Name:  name,
	})
	if err == nil {
		return user, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: create user: %w", err)
	}

	// A different provider reference can race for the same globally unique
	// email. The unique constraint chooses the owner; reselect it under lock.
	user, err = queries.GetAuthUserByEmailForProvisioning(ctx, email)
	if err != nil {
		return sqlcgen.AuthCoreUser{}, false, fmt.Errorf("identityrepo: reselect user email: %w", err)
	}
	return user, false, nil
}

func applyInitialAdministrationRole(
	ctx context.Context,
	queries *sqlcgen.Queries,
	userID int32,
	command identity.ProvisionCommand,
) error {
	return ApplyInitialAdministrationRole(
		ctx, queries, userID,
		command.InitialAdministrationMode, command.InitialAdministrationRole,
	)
}

// ApplyInitialAdministrationRole is the single implementation of the
// `initial_global_admins` grant, shared by the two provisioning planes.
//
// It is exported for internal/api/v2/auth, the browser plane that is mounted
// when single sign-on is configured and that provisions through its own
// transaction. Duplicating this there would give "who is an initial admin" two
// definitions, and the second one would drift.
//
// The grant is idempotent and one-shot per user: a user who ALREADY holds any
// role in the administration mode is left exactly as the operator left them, so
// a demotion is never silently undone by the next login.
func ApplyInitialAdministrationRole(
	ctx context.Context,
	queries *sqlcgen.Queries,
	userID int32,
	mode string,
	role string,
) error {
	if mode == "" || role == "" {
		return nil
	}
	count, err := queries.CountAuthUserRolesInMode(ctx, sqlcgen.CountAuthUserRolesInModeParams{
		UserID: userID,
		Mode:   mode,
	})
	if err != nil {
		return fmt.Errorf("identityrepo: count administration roles: %w", err)
	}
	if count != 0 {
		return nil
	}
	assigned, err := queries.AssignAuthUserRoleByNameAndMode(ctx, sqlcgen.AssignAuthUserRoleByNameAndModeParams{
		UserID:   userID,
		RoleName: role,
		Mode:     mode,
	})
	if err != nil {
		return fmt.Errorf("identityrepo: assign initial administration role: %w", err)
	}
	if assigned != 1 {
		return errors.New("identityrepo: initial administration role is unavailable")
	}
	return nil
}

func reconcileProjectEnrollment(
	ctx context.Context,
	queries *sqlcgen.Queries,
	userID int32,
	decision identity.ProjectEnrollmentDecision,
) error {
	if !decision.Eligible || decision.ProjectID == 0 {
		return nil
	}

	roles := []string{projectViewerRole}
	if len(decision.AdditionalGlobalAdminRoles) != 0 {
		globalAdmin, err := queries.HasAuthAdministrationAdminRole(ctx, userID)
		if err != nil {
			return fmt.Errorf("identityrepo: resolve administration roles: %w", err)
		}
		if globalAdmin {
			roles = append(roles, decision.AdditionalGlobalAdminRoles...)
		}
	}
	if _, err := queries.AssignExistingProjectRoles(ctx, sqlcgen.AssignExistingProjectRolesParams{
		ProjectID: decision.ProjectID,
		UserID:    userID,
		RoleNames: roles,
	}); err != nil {
		return fmt.Errorf("identityrepo: reconcile project roles: %w", err)
	}
	return nil
}
