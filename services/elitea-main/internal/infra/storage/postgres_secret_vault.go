package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	currentAdminVaultID = "admin"

	// The two failure messages, named because they are also the gate keys.
	vaultReadFailedLog = "secret vault read failed"
	vaultDidNotOpenLog = "secret vault did not open"
)

// ErrVaultAbsent reports that the vault does not exist: neither centry row is
// present for that id.
//
// It is a DIFFERENT answer from ErrContentUnavailable, and it still satisfies
// errors.Is for that sentinel, so a caller that does not distinguish the two
// keeps its behaviour unchanged. A fresh deployment holds no `admin` vault and
// no project vault until something writes a secret, so "absent" is the normal
// state of a new install, not a failure. Treating it as one made every read
// that consults the vault for a DEFAULT answer 500 on a clean deployment.
var ErrVaultAbsent = fmt.Errorf("%w: the vault does not exist", ErrContentUnavailable)

// SecretVault exposes only exact-name reads. The ProjectID variants are the
// narrow string-or-integer compatibility path for current default-model keys;
// ordinary credential reads remain string-only. LookupRegular is used for the
// current shared-admin fallback, which must not expose hidden admin secrets to
// project workloads.
type SecretVault interface {
	Lookup(string) (centrysecrets.Secret, error)
	LookupRegular(string) (centrysecrets.Secret, error)
	LookupProjectID(string) (centrysecrets.Secret, error)
	LookupRegularProjectID(string) (centrysecrets.Secret, error)
	LookupRegularInteger(string) (centrysecrets.Secret, error)
}

// SecretVaultLoader loads one current Centry vault snapshot. Implementations
// must not cache decrypted values beyond the request that asked for them.
type SecretVaultLoader interface {
	LoadProjectVault(context.Context, int64) (SecretVault, error)
	LoadAdminVault(context.Context) (SecretVault, error)
}

// PostgresSecretVaultLoader is a read-only adapter for the current
// centry.secrets_key / centry.secrets_data representation. The optional master
// key is injected explicitly; this package never reads process environment.
type PostgresSecretVaultLoader struct {
	store     contentQueryer
	masterKey []byte

	// reported gates the failure log to the FIRST failure of each kind per
	// vault since that vault last loaded. See logVaultFailure.
	reportedMu sync.Mutex
	reported   map[string]struct{}
}

func NewPostgresSecretVaultLoader(pool *pgxpool.Pool, masterKey []byte) (*PostgresSecretVaultLoader, error) {
	if pool == nil {
		return nil, errors.New("secret vault database pool is required")
	}
	return newPostgresSecretVaultLoader(pool, masterKey)
}

func newPostgresSecretVaultLoader(store contentQueryer, masterKey []byte) (*PostgresSecretVaultLoader, error) {
	if store == nil {
		return nil, errors.New("secret vault database store is required")
	}
	if len(masterKey) > 0 && !validEncodedFernetKey(masterKey) {
		return nil, errors.New("secret vault master key is invalid")
	}
	return &PostgresSecretVaultLoader{
		store:     store,
		masterKey: append([]byte(nil), masterKey...),
		reported:  map[string]struct{}{},
	}, nil
}

// Destroy clears the process-held master key after every consumer has stopped.
// It is a shutdown operation and must not race with LoadProjectVault or
// LoadAdminVault.
func (l *PostgresSecretVaultLoader) Destroy() {
	if l == nil {
		return
	}
	clearContentBytes(l.masterKey)
	l.masterKey = nil
	l.store = nil
	l.reportedMu.Lock()
	l.reported = nil
	l.reportedMu.Unlock()
}

// logVaultFailure logs the FIRST failure of one kind for one vault, and stays
// quiet until that vault loads again.
//
// Every failure here is per-REQUEST, and one model-picker page load asks for up
// to three vaults across six sections. A deployment whose SECRETS_MASTER_KEY
// does not match its stored vaults — the case this log exists to name — takes
// every one of those. Logging each would put tens of identical lines per page
// load per user into the log, continuously, and the volume that makes the cause
// findable is the same volume that pushes the rest of the log out of retention.
//
// A recurrence after a recovery is reported again: forgetVaultFailures clears
// the gate on every successful load, so this is "the first failure since the
// last success", not "once per process".
func (l *PostgresSecretVaultLoader) logVaultFailure(ctx context.Context, kind, vaultID string, cause error) {
	l.reportedMu.Lock()
	if l.reported == nil {
		l.reported = map[string]struct{}{}
	}
	key := kind + "\x00" + vaultID
	_, seen := l.reported[key]
	if !seen {
		l.reported[key] = struct{}{}
	}
	l.reportedMu.Unlock()
	if seen {
		return
	}
	slog.ErrorContext(ctx, kind, "vault_id", vaultID, "error", cause)
}

// forgetVaultFailures re-arms the log for one vault after it loads.
func (l *PostgresSecretVaultLoader) forgetVaultFailures(vaultID string) {
	l.reportedMu.Lock()
	defer l.reportedMu.Unlock()
	if len(l.reported) == 0 {
		return
	}
	for _, kind := range [...]string{vaultReadFailedLog, vaultDidNotOpenLog} {
		delete(l.reported, kind+"\x00"+vaultID)
	}
}

func (l *PostgresSecretVaultLoader) LoadProjectVault(ctx context.Context, projectID int64) (SecretVault, error) {
	if projectID <= 0 {
		return nil, ErrContentRejected
	}
	return l.load(ctx, "project-"+strconv.FormatInt(projectID, 10))
}

func (l *PostgresSecretVaultLoader) LoadAdminVault(ctx context.Context) (SecretVault, error) {
	return l.load(ctx, currentAdminVaultID)
}

func (l *PostgresSecretVaultLoader) load(ctx context.Context, vaultID string) (SecretVault, error) {
	if l == nil || l.store == nil {
		return nil, ErrContentUnavailable
	}
	var storedProjectKey, encryptedVault []byte
	err := l.store.QueryRow(ctx, `
SELECT k.data, d.data
FROM centry.secrets_key AS k
JOIN centry.secrets_data AS d ON d.id = k.id
WHERE k.id = $1`, vaultID).Scan(&storedProjectKey, &encryptedVault)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVaultAbsent
	}
	if err != nil {
		// The cause is logged rather than returned: callers must keep seeing
		// one opaque sentinel, but an operator reading the service log needs
		// to tell a dropped connection from a vault that will not decrypt.
		// Without this line the only symptom is a 500 with no cause anywhere,
		// which is how the model catalogue's outage had to be diagnosed from a
		// database dump.
		l.logVaultFailure(ctx, vaultReadFailedLog, vaultID, err)
		return nil, fmt.Errorf("load encrypted secret vault: %w", ErrContentUnavailable)
	}
	defer clearContentBytes(storedProjectKey)
	defer clearContentBytes(encryptedVault)

	var vault *centrysecrets.Vault
	if len(l.masterKey) == 0 {
		vault, err = centrysecrets.OpenUnwrapped(storedProjectKey, encryptedVault)
	} else {
		vault, err = centrysecrets.OpenWrapped(l.masterKey, storedProjectKey, encryptedVault)
	}
	if err != nil {
		// A vault that EXISTS and will not open is the opposite answer from an
		// absent one, and the two must never be confused: absence means "no
		// secrets have been stored yet", while this means "the secrets are
		// there and this process cannot read them" — a wrong master key, most
		// often. Only the reason class is logged; no stored value can reach a
		// log line.
		l.logVaultFailure(ctx, vaultDidNotOpenLog, vaultID, err)
		return nil, fmt.Errorf("open encrypted secret vault: %w", ErrContentUnavailable)
	}
	l.forgetVaultFailures(vaultID)
	return vault, nil
}

func validEncodedFernetKey(value []byte) bool {
	if len(value) != 44 {
		return false
	}
	var decoded [33]byte
	n, err := base64.URLEncoding.Decode(decoded[:], value)
	clearContentBytes(decoded[:])
	return err == nil && n == 32
}
