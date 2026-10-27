// Package pgvault puts the encrypted PostgreSQL vault behind the governed
// SecretStore seam.
//
// The vault keeps its legacy-schema-compatible representation
// (centry.secrets_key / centry.secrets_data, one Fernet-encrypted document per
// project). This adapter changes nothing about it: reads go through the same
// storage.SecretVaultLoader the runtime uses, and writes through the same
// repos.CurrentSecretVaultRepository seal and purge the configurations path
// uses. Existing callers keep calling those directly.
//
// The vault has no versions, so this adapter models each secret as
// single-version and immutable:
//
//   - A SecretRef has an empty Mount; Path is the secret name in the vault of
//     StoreContext.TenantProjectID. The shared admin vault is not reachable.
//   - Write accepts only casVersion 0 and creates version 1 of a name that
//     does not exist yet (in either collection, so a governed secret is never
//     shadowed by a same-named visible one). Rotation is a new name.
//   - Read and Retire accept only version 1.
//
// Reads return the bytes the direct path returns for the same name
// (vault.Lookup), so the runtime and the broker can never disagree.
package pgvault

import (
	"context"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// The only version a pgvault secret has.
const onlyVersion int64 = 1

const maxSecretNameBytes = 128

// Sealer is the write half of the vault, satisfied by
// *repos.CurrentSecretVaultRepository. Both methods run inside a transaction
// the caller owns and never commit.
type Sealer interface {
	SealProjectHiddenSecrets(ctx context.Context, tx pgx.Tx, projectID int64, mutations []configurationapp.HiddenSecretMutation) error
	PurgeProjectHiddenSecrets(ctx context.Context, tx pgx.Tx, projectID int64, names []string) error
}

// TxBeginner opens the transaction a write runs in. *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the pgvault SecretStore.
type Store struct {
	db     TxBeginner
	loader storage.SecretVaultLoader
	sealer Sealer
}

var _ secretstore.SecretStore = (*Store)(nil)

// New builds the adapter over the existing vault reader and writer. Pass the
// same loader and repository the rest of the process uses, so the adapter
// holds the deployment's one master key and nothing else.
func New(db TxBeginner, loader storage.SecretVaultLoader, sealer Sealer) (*Store, error) {
	if db == nil || loader == nil || sealer == nil {
		return nil, errors.New("pgvault: database, vault loader and vault sealer are required")
	}
	return &Store{db: db, loader: loader, sealer: sealer}, nil
}

func validateRef(op secretstore.Op, ref secretstore.SecretRef) error {
	if err := secretstore.ValidateBackend(op, ref, secretstore.BackendPGVault); err != nil {
		return err
	}
	if ref.Mount != "" || !validSecretName(ref.Path) {
		return secretstore.Errorf(secretstore.ErrInvalidRequest, op, "malformed vault reference")
	}
	return nil
}

// validSecretName is the vault's own name rule (centrysecrets).
func validSecretName(name string) bool {
	if len(name) == 0 || len(name) > maxSecretNameBytes {
		return false
	}
	for index := 0; index < len(name); index++ {
		c := name[index]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

// Read returns version 1, the only version.
func (s *Store) Read(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) (secretstore.SecretVersion, error) {
	if err := sc.Validate(secretstore.OpRead); err != nil {
		return secretstore.SecretVersion{}, err
	}
	if err := validateRef(secretstore.OpRead, ref); err != nil {
		return secretstore.SecretVersion{}, err
	}
	if err := secretstore.ValidateReadVersion(version); err != nil {
		return secretstore.SecretVersion{}, err
	}
	if version != onlyVersion {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrNotFound, secretstore.OpRead, "the vault holds version 1 only")
	}
	secret, err := s.lookup(ctx, secretstore.OpRead, sc.TenantProjectID, ref.Path)
	if err != nil {
		return secretstore.SecretVersion{}, err
	}
	return secretstore.SecretVersion{
		Version: onlyVersion,
		Value:   secretstore.NewRuntimeSecret([]byte(secret.Value)),
	}, nil
}

// Metadata reports version 1 for an existing secret. CASRequired is true:
// the adapter itself refuses every write but a create.
func (s *Store) Metadata(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef) (secretstore.Metadata, error) {
	if err := sc.Validate(secretstore.OpMetadata); err != nil {
		return secretstore.Metadata{}, err
	}
	if err := validateRef(secretstore.OpMetadata, ref); err != nil {
		return secretstore.Metadata{}, err
	}
	if _, err := s.lookup(ctx, secretstore.OpMetadata, sc.TenantProjectID, ref.Path); err != nil {
		return secretstore.Metadata{}, err
	}
	return secretstore.Metadata{CurrentVersion: onlyVersion, Versions: []int64{onlyVersion}, CASRequired: true}, nil
}

// Write creates version 1 of a name that does not exist yet.
func (s *Store) Write(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, value secretstore.RuntimeSecret, casVersion int64) (int64, error) {
	if err := sc.Validate(secretstore.OpWrite); err != nil {
		return 0, err
	}
	if err := validateRef(secretstore.OpWrite, ref); err != nil {
		return 0, err
	}
	if casVersion != 0 {
		return 0, secretstore.Errorf(secretstore.ErrMisconfigured, secretstore.OpWrite, "the vault holds one version per name; rotate to a new reference")
	}
	raw := value.UnsafeBytes()
	if len(raw) == 0 || !utf8.Valid(raw) {
		return 0, secretstore.Errorf(secretstore.ErrInvalidRequest, secretstore.OpWrite, "the vault stores non-empty UTF-8 text only")
	}
	err := s.inLockedTx(ctx, secretstore.OpWrite, sc.TenantProjectID, func(tx pgx.Tx) error {
		_, lookupErr := s.lookup(ctx, secretstore.OpWrite, sc.TenantProjectID, ref.Path)
		switch {
		case lookupErr == nil:
			return secretstore.Errorf(secretstore.ErrVersionConflict, secretstore.OpWrite, "the name already exists")
		case !errors.Is(lookupErr, secretstore.ErrNotFound):
			return lookupErr
		}
		// The string conversion copies; the copy lives until the seal
		// returns, as on every other vault write path.
		mutation := []configurationapp.HiddenSecretMutation{{Name: ref.Path, Value: string(raw)}}
		if sealErr := s.sealer.SealProjectHiddenSecrets(ctx, tx, sc.TenantProjectID, mutation); sealErr != nil {
			return secretstore.Errorf(secretstore.ErrUnavailable, secretstore.OpWrite, "seal the vault")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return onlyVersion, nil
}

// Retire removes version 1. Retiring an absent secret succeeds.
func (s *Store) Retire(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) error {
	if err := sc.Validate(secretstore.OpRetire); err != nil {
		return err
	}
	if err := validateRef(secretstore.OpRetire, ref); err != nil {
		return err
	}
	if version != onlyVersion {
		return secretstore.Errorf(secretstore.ErrNotFound, secretstore.OpRetire, "the vault holds version 1 only")
	}
	return s.inLockedTx(ctx, secretstore.OpRetire, sc.TenantProjectID, func(tx pgx.Tx) error {
		if err := s.sealer.PurgeProjectHiddenSecrets(ctx, tx, sc.TenantProjectID, []string{ref.Path}); err != nil {
			return secretstore.Errorf(secretstore.ErrUnavailable, secretstore.OpRetire, "purge the vault")
		}
		return nil
	})
}

// inLockedTx serialises this adapter's writers for one project vault with a
// transaction-scoped advisory lock, held across the existence check and the
// seal, so two creates of one name cannot both succeed. The lock is taken
// even when the vault rows do not exist yet, which a row lock cannot do.
func (s *Store) inLockedTx(ctx context.Context, op secretstore.Op, projectID int64, body func(pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "begin the vault transaction")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The existence check reads through the loader on a second pool
	// connection while this one waits for the lock. lock_timeout bounds the
	// wait, so writers queued here can never hold the whole pool while the
	// lock holder waits for a connection.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "lock the vault")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"elitea.secretstore.pgvault:project-"+strconv.FormatInt(projectID, 10)); err != nil {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "lock the vault")
	}
	if err := body(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "commit the vault transaction")
	}
	return nil
}

func (s *Store) lookup(ctx context.Context, op secretstore.Op, projectID int64, name string) (centrysecrets.Secret, error) {
	vault, err := s.loader.LoadProjectVault(ctx, projectID)
	switch {
	case errors.Is(err, storage.ErrVaultAbsent):
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrNotFound, op, "the project has no vault")
	case errors.Is(err, storage.ErrContentRejected):
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrInvalidRequest, op, "the project id is rejected")
	case err != nil:
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrUnavailable, op, "load the vault")
	case vault == nil:
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrUnavailable, op, "load the vault")
	}
	secret, err := vault.Lookup(name)
	switch {
	case errors.Is(err, centrysecrets.ErrSecretNotFound):
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrNotFound, op, "no such secret")
	case errors.Is(err, centrysecrets.ErrInvalidSecret):
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrMisconfigured, op, "the stored value is not text")
	case err != nil:
		return centrysecrets.Secret{}, secretstore.Errorf(secretstore.ErrUnavailable, op, "open the vault")
	}
	return secret, nil
}
