package secretstore

import (
	"context"
	"errors"
	"log/slog"
)

// RetireOutcome is what RetireSuperseded did.
type RetireOutcome string

const (
	// RetireDone: the backend version is retired (or was already absent),
	// and a committed ledger row for it, if any, is now 'retired'.
	RetireDone RetireOutcome = "retired"
	// RetireNotOwned: the store removes by name (pgvault), and no committed
	// ledger row proves that a governed write created this name and
	// version. Nothing was removed: the name can be a secret that something
	// else still uses.
	RetireNotOwned RetireOutcome = "not_owned"
)

// RetireSuperseded retires one secret version that a credential reference
// no longer uses (#1505): the value a rotation replaced, or a rotation that
// was cancelled. The caller calls it after the reference change commits.
//
//   - On a VersionScopedRetirer (Vault/OpenBao KV v2) the version is
//     destroyed. The path belongs to the reference, because a rotation
//     writes new versions to it.
//   - On another store (pgvault, which removes a whole name) the name is
//     removed only when a committed ledger row owns it. Otherwise the
//     answer is RetireNotOwned and nothing is removed.
//   - Not found counts as retired.
//   - After the backend retire, the committed ledger rows of that version
//     move to 'retired' (migration 0220).
//
// A backend error is returned and the ledger row stays committed, so the
// version stays visible as owned and an operator can retire it again.
func (l *Ledger) RetireSuperseded(ctx context.Context, store SecretStore, sc StoreContext, ref SecretRef, version int64) (RetireOutcome, error) {
	if l == nil || l.db == nil || store == nil {
		return "", Errorf(ErrMisconfigured, OpRetire, "ledger or store is not configured")
	}
	if err := sc.Validate(OpRetire); err != nil {
		return "", err
	}
	if version <= 0 || ref.Path == "" || len(ref.Path) > 512 || len(ref.Mount) > 256 {
		return "", Errorf(ErrInvalidRequest, OpRetire, "malformed reference or version")
	}
	var owned bool
	if err := l.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_identity.secret_write_ledger
    WHERE state = 'committed' AND backend = $1 AND locator_mount = $2
      AND locator_path = $3 AND intended_version = $4)`,
		ref.Backend, ref.Mount, ref.Path, version).Scan(&owned); err != nil {
		return "", Errorf(ErrUnavailable, OpRetire, "read the ledger")
	}
	if _, scoped := store.(VersionScopedRetirer); !scoped && !owned {
		return RetireNotOwned, nil
	}
	if err := store.Retire(ctx, sc, ref, version); err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if _, err := l.db.Exec(context.WithoutCancel(ctx), `
UPDATE elitea_identity.secret_write_ledger
SET state = 'retired', state_changed_at = clock_timestamp()
WHERE state = 'committed' AND backend = $1 AND locator_mount = $2
  AND locator_path = $3 AND intended_version = $4`,
		ref.Backend, ref.Mount, ref.Path, version); err != nil {
		// The backend version is gone. A row left committed only names a
		// version that no longer exists; a later retire finds it absent.
		slog.Default().Warn("secretstore: retired a superseded version but could not mark its ledger row retired",
			"backend", ref.Backend, "tenant_project_id", sc.TenantProjectID)
	}
	return RetireDone, nil
}
