package repos

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxCurrentVaultMutations = 256

var (
	ErrInvalidCurrentVaultMutation = errors.New("invalid current secret-vault mutation")
	ErrCurrentVaultUnavailable     = errors.New("current secret vault is unavailable")
)

// errCurrentVaultAbsent reports that the project holds NEITHER vault row.
//
// It is a distinct answer from ErrCurrentVaultUnavailable, and it still
// satisfies errors.Is for that sentinel, so an existing caller reads it
// unchanged. Only this answer permits a create. A vault that exists and will
// not open must never be replaced, because the replacement destroys every
// secret in it.
var errCurrentVaultAbsent = fmt.Errorf("%w: the project has no vault", ErrCurrentVaultUnavailable)

// ProjectVaultCreator creates the empty vault of a project that has none.
//
// internal/api/v2/secrets.Handler is the ONE implementation (#399). It holds
// the only master key a deployment sets, so it alone decides whether the stored
// Fernet key is wrapped. This repository must not mint a second one.
type ProjectVaultCreator interface {
	EnsureProjectVault(ctx context.Context, projectID string) error
}

// CurrentSecretVaultOption configures one CurrentSecretVaultRepository.
type CurrentSecretVaultOption func(*CurrentSecretVaultRepository)

// WithProjectVaultCreator lets a seal create the vault it needs.
//
// WHY A WRITE PATH NEEDS THIS. A project can hold rows and no vault. The
// bootstrap file migrations/001_initial.sql inserts centry.project id 1 and
// creates p_1 directly, and it does not call the provisioner, so the default
// project of a FRESH install has no vault. Without a creator, the first
// credential save into that project answers 503 and the deployment can store no
// provider key at all.
//
// Without this option the repository keeps the earlier behaviour and refuses
// the write. It never falls back to a plaintext row.
func WithProjectVaultCreator(creator ProjectVaultCreator) CurrentSecretVaultOption {
	return func(r *CurrentSecretVaultRepository) {
		r.creator = creator
	}
}

// CurrentSecretVaultRepository atomically rewrites the existing
// centry.secrets_key/secrets_data representation. It does not invent a second
// secret store and never exposes enumeration through this application-facing
// adapter.
type CurrentSecretVaultRepository struct {
	store     sharedStore
	masterKey []byte
	// creator makes the vault of a project that has none. nil refuses such a
	// write. See WithProjectVaultCreator.
	creator ProjectVaultCreator
}

func NewCurrentSecretVaultRepository(
	pool *pgxpool.Pool,
	masterKey []byte,
	opts ...CurrentSecretVaultOption,
) (*CurrentSecretVaultRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentSecretVaultRepository(store, masterKey, opts...)
}

func newCurrentSecretVaultRepository(
	store sharedStore,
	masterKey []byte,
	opts ...CurrentSecretVaultOption,
) (*CurrentSecretVaultRepository, error) {
	if store == nil {
		return nil, errors.New("current secret-vault database is required")
	}
	if len(masterKey) != 0 && !validEncodedCurrentFernetKey(masterKey) {
		return nil, errors.New("current secret-vault master key is invalid")
	}
	repository := &CurrentSecretVaultRepository{
		store:     store,
		masterKey: append([]byte(nil), masterKey...),
	}
	for _, opt := range opts {
		opt(repository)
	}
	return repository, nil
}

func (r *CurrentSecretVaultRepository) MutateProject(
	ctx context.Context,
	projectID int64,
	mutations []centrysecrets.Mutation,
) error {
	if projectID <= 0 {
		return ErrInvalidCurrentVaultMutation
	}
	return r.mutate(ctx, "project-"+strconv.FormatInt(projectID, 10), mutations)
}

func (r *CurrentSecretVaultRepository) MutateAdmin(ctx context.Context, mutations []centrysecrets.Mutation) error {
	return r.mutate(ctx, "admin", mutations)
}

// SealProjectHiddenSecrets writes hidden secret values into one project vault
// inside the transaction the caller owns.
//
// The compatibility configurations route needs the row write and the vault
// write to commit together. A separate transaction can leave a stored
// {{secret.NAME}} reference that names nothing, and the gateway then resolves
// an unusable credential.
//
// The caller commits or rolls back. This method never commits.
func (r *CurrentSecretVaultRepository) SealProjectHiddenSecrets(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	mutations []configurationapp.HiddenSecretMutation,
) error {
	if r == nil {
		return ErrCurrentVaultUnavailable
	}
	if ctx == nil || tx == nil || projectID <= 0 {
		return ErrInvalidCurrentVaultMutation
	}
	if len(mutations) == 0 || len(mutations) > maxCurrentVaultMutations {
		return ErrInvalidCurrentVaultMutation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vaultMutations := make([]centrysecrets.Mutation, len(mutations))
	for index, mutation := range mutations {
		if mutation.Name == "" || mutation.Value == "" {
			return ErrInvalidCurrentVaultMutation
		}
		vaultMutations[index] = centrysecrets.Mutation{
			Collection: centrysecrets.HiddenSecrets,
			Name:       mutation.Name,
			Value:      mutation.Value,
		}
	}
	executor := pgxExecutor{queryer: tx}
	project := strconv.FormatInt(projectID, 10)
	vault, err := lockCurrentSecretVault(ctx, executor, "project-"+project)
	if errors.Is(err, errCurrentVaultAbsent) {
		if createErr := r.createAbsentProjectVault(ctx, project); createErr != nil {
			return createErr
		}
		vault, err = lockCurrentSecretVault(ctx, executor, "project-"+project)
	}
	if err != nil {
		return err
	}
	defer vault.destroy()
	return vault.mutate(ctx, executor, r.masterKey, vaultMutations)
}

// PurgeProjectHiddenSecrets deletes hidden secrets from one project vault
// inside the transaction the caller owns (#1204).
//
// It is the inverse of SealProjectHiddenSecrets: a configuration delete
// removes the row and the secrets only that row referenced, and both commit
// together. The caller decides which names are no longer referenced; this
// method only removes them, and only from the hidden collection, so a
// user-named secret of the same name in the visible list is never touched.
//
// A project with no vault holds no secret to remove, so that answer is nil.
// No vault is created for a purge. The caller commits or rolls back.
func (r *CurrentSecretVaultRepository) PurgeProjectHiddenSecrets(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	names []string,
) error {
	if r == nil {
		return ErrCurrentVaultUnavailable
	}
	if ctx == nil || tx == nil || projectID <= 0 {
		return ErrInvalidCurrentVaultMutation
	}
	if len(names) == 0 {
		return nil
	}
	if len(names) > maxCurrentVaultMutations {
		return ErrInvalidCurrentVaultMutation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vaultMutations := make([]centrysecrets.Mutation, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			return ErrInvalidCurrentVaultMutation
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		vaultMutations = append(vaultMutations, centrysecrets.Mutation{
			Collection: centrysecrets.HiddenSecrets,
			Name:       name,
			Delete:     true,
		})
	}
	executor := pgxExecutor{queryer: tx}
	vault, err := lockCurrentSecretVault(ctx, executor, "project-"+strconv.FormatInt(projectID, 10))
	if errors.Is(err, errCurrentVaultAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	defer vault.destroy()
	return vault.mutate(ctx, executor, r.masterKey, vaultMutations)
}

// createAbsentProjectVault gives a project its empty vault, so that the seal
// above can lock one.
//
// It runs on the pool, OUTSIDE the caller's transaction, and that is
// deliberate. The caller's transaction holds locks in the tenant schema only,
// and the two statement sets touch different tables, so the second connection
// cannot deadlock against the first. The re-read after it uses a new READ
// COMMITTED snapshot, so it sees the committed rows.
//
// An empty vault that a later rollback leaves behind is harmless. It carries no
// secret, and the creator is idempotent.
func (r *CurrentSecretVaultRepository) createAbsentProjectVault(ctx context.Context, project string) error {
	if r == nil || r.creator == nil {
		return errCurrentVaultAbsent
	}
	if err := r.creator.EnsureProjectVault(ctx, project); err != nil {
		return fmt.Errorf("%w: create the project vault: %w", ErrCurrentVaultUnavailable, err)
	}
	return nil
}

// THIS TYPE CREATES NO VAULT, AND MUST NOT (#399). It used to carry
// EnsureProjectVault and DeleteProjectVault, keyed off
// ELITEA_VAULT_MASTER_KEY_FILE. No file under deploy/ sets that variable, while
// five set SECRETS_MASTER_KEY, which is the key the secrets handler uses. Two
// creators with two key sources compose without an error — both are
// create-if-absent and idempotent — and leave a vault that one path cannot
// decrypt. Nothing reports the fault until a later read fails.
//
// internal/api/v2/secrets.Handler is now the ONE creator, called by the
// project_secrets provisioning step. Every writer that needs a vault must run
// after that step and hold that step's key.
//
// createAbsentProjectVault above does not break that rule. It mints nothing. It
// calls that same handler, through ProjectVaultCreator, for the one project the
// provisioning step never ran for: the default project of a fresh install.
// MutateProject and MutateAdmin below keep the earlier behaviour and create
// nothing, because neither of them fails a user's write closed.

func (r *CurrentSecretVaultRepository) mutate(ctx context.Context, vaultID string, mutations []centrysecrets.Mutation) error {
	if r == nil || r.store == nil {
		return ErrCurrentVaultUnavailable
	}
	if ctx == nil || vaultID == "" || len(mutations) == 0 || len(mutations) > maxCurrentVaultMutations {
		return ErrInvalidCurrentVaultMutation
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		vault, err := lockCurrentSecretVault(ctx, tx, vaultID)
		if err != nil {
			return err
		}
		defer vault.destroy()
		return vault.mutate(ctx, tx, r.masterKey, mutations)
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

// lockedCurrentSecretVault owns the encrypted bytes selected under a qualified
// centry row lock. It is transaction-local and must be destroyed before the
// owning transaction callback returns.
type lockedCurrentSecretVault struct {
	id                  string
	encryptedProjectKey []byte
	encryptedVault      []byte
}

func lockCurrentSecretVault(
	ctx context.Context,
	tx sqlExecutor,
	vaultID string,
) (*lockedCurrentSecretVault, error) {
	if ctx == nil || tx == nil || vaultID == "" {
		return nil, ErrCurrentVaultUnavailable
	}
	vault := &lockedCurrentSecretVault{id: vaultID}
	if err := tx.QueryRow(ctx, `
SELECT k.data, d.data
FROM centry.secrets_key AS k
JOIN centry.secrets_data AS d ON d.id = k.id
WHERE k.id = $1
FOR UPDATE OF k, d`, vaultID).Scan(&vault.encryptedProjectKey, &vault.encryptedVault); err != nil {
		vault.destroy()
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errCurrentVaultAbsent
		}
		return nil, fmt.Errorf("lock current secret vault: %w", ErrCurrentVaultUnavailable)
	}
	if len(vault.encryptedProjectKey) == 0 || len(vault.encryptedVault) == 0 {
		vault.destroy()
		return nil, ErrCurrentVaultUnavailable
	}
	return vault, nil
}

func (v *lockedCurrentSecretVault) mutate(
	ctx context.Context,
	tx sqlExecutor,
	masterKey []byte,
	mutations []centrysecrets.Mutation,
) error {
	if v == nil || tx == nil || v.id == "" || len(mutations) == 0 || len(mutations) > maxCurrentVaultMutations {
		return ErrInvalidCurrentVaultMutation
	}
	var rewritten []byte
	var err error
	if len(masterKey) == 0 {
		rewritten, err = centrysecrets.RewriteUnwrapped(v.encryptedProjectKey, v.encryptedVault, mutations)
	} else {
		rewritten, err = centrysecrets.RewriteWrapped(masterKey, v.encryptedProjectKey, v.encryptedVault, mutations)
	}
	if errors.Is(err, centrysecrets.ErrInvalidMutation) {
		return ErrInvalidCurrentVaultMutation
	}
	if err != nil {
		return ErrCurrentVaultUnavailable
	}
	defer clearCurrentVaultBytes(rewritten)

	tag, err := tx.Exec(ctx, `
UPDATE centry.secrets_data
SET data = $2
WHERE id = $1`, v.id, rewritten)
	if err != nil || tag.RowsAffected() != 1 {
		return ErrCurrentVaultUnavailable
	}
	return nil
}

func (v *lockedCurrentSecretVault) destroy() {
	if v == nil {
		return
	}
	clearCurrentVaultBytes(v.encryptedProjectKey)
	clearCurrentVaultBytes(v.encryptedVault)
	v.encryptedProjectKey = nil
	v.encryptedVault = nil
	v.id = ""
}

// Destroy clears the repository-owned master-key copy. The repository must not
// be reused after this call.
func (r *CurrentSecretVaultRepository) Destroy() {
	if r == nil {
		return
	}
	clearCurrentVaultBytes(r.masterKey)
	r.masterKey = nil
	r.store = nil
}

func validEncodedCurrentFernetKey(value []byte) bool {
	if len(value) != 44 {
		return false
	}
	decoded := make([]byte, base64.URLEncoding.DecodedLen(len(value)))
	n, err := base64.URLEncoding.Decode(decoded, value)
	clearCurrentVaultBytes(decoded)
	return err == nil && n == 32
}

func clearCurrentVaultBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
