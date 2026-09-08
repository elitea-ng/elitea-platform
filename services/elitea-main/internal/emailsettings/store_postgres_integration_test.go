package emailsettings

// The STORE against a real PostgreSQL.
//
// The DDL is executed from the migration FILE rather than restated here, so
// this test cannot pass against a schema the migration does not create — the
// rule `internal/identityproviders`'s integration test states, and what keeps
// a constraint from being asserted in a copy nobody deploys.
//
// The VAULT half is a double, deliberately. The vault seam exists so that this
// package can be tested without a master key and a secrets corpus, and the
// property this file is about is the ROWS: that a save writes every declared
// key, that clearing a field clears the row, and that a stored port survives
// the `jsonb` round trip as a number rather than as a float that reads back as
// zero.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// memoryVault is the hidden bucket, in memory.
type memoryVault struct {
	values map[string]string
	// storeErr makes a seal fail, so a test can prove the ORDER: a failed
	// seal must not leave a row claiming a password that is not there.
	storeErr error
}

func newMemoryVault() *memoryVault { return &memoryVault{values: map[string]string{}} }

func (v *memoryVault) StoreAdminHiddenSecret(_ context.Context, name, value string) error {
	if v.storeErr != nil {
		return v.storeErr
	}
	v.values[name] = value
	return nil
}

func (v *memoryVault) LookupAdminHiddenSecret(_ context.Context, name string) (string, error) {
	if value, ok := v.values[name]; ok {
		return value, nil
	}
	return "", secrets.ErrSecretNotFound
}

func (v *memoryVault) DeleteAdminHiddenSecret(_ context.Context, name string) error {
	delete(v.values, name)
	return nil
}

func TestSaveWritesEveryKeyAndLoadReadsThemBack(t *testing.T) {
	pool := newEmailPool(t)
	vault := newMemoryVault()
	store := NewStore(pool, vault)
	ctx := context.Background()

	settings := Settings{
		Host: "smtp.acme.example", Port: 2525, Username: "relay-user", TLS: TLSStartTLS,
		From: "noreply@acme.example", ReplyTo: "help@acme.example",
		PublicBaseURL: "https://ai.acme.example",
	}
	password := "not-a-real-relay-password"
	require.NoError(t, store.Save(ctx, settings, &password, "ops@acme.example"))

	loaded, sealed, err := store.Load(ctx)
	require.NoError(t, err)
	require.True(t, sealed)
	// The PORT specifically: it goes through `jsonb` as a number and comes
	// back as a float64. A reader that only accepted an int would read it as
	// zero, which layers as "unstated" and silently restores the default.
	require.Equal(t, 2525, loaded.Port)
	require.Equal(t, settings, loaded)

	// Every declared key has a row, including the ones a form left blank. A
	// key written only when non-empty would keep its previous value while the
	// form showed it blank.
	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.platform_config WHERE section = $1`, Section).Scan(&rows))
	require.Equal(t, len(storedKeys), rows)
}

func TestSaveClearsAFieldRatherThanLeavingThePreviousValue(t *testing.T) {
	pool := newEmailPool(t)
	store := NewStore(pool, newMemoryVault())
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, Settings{
		Host: "smtp.acme.example", From: "noreply@acme.example", ReplyTo: "help@acme.example",
	}, nil, "ops"))
	// The operator deletes the reply-to and saves.
	require.NoError(t, store.Save(ctx, Settings{
		Host: "smtp.acme.example", From: "noreply@acme.example",
	}, nil, "ops"))

	loaded, _, err := store.Load(ctx)
	require.NoError(t, err)
	require.Empty(t, loaded.ReplyTo, "a cleared field came back on the next read")
}

func TestPasswordIsTriStateAcrossSaves(t *testing.T) {
	pool := newEmailPool(t)
	vault := newMemoryVault()
	store := NewStore(pool, vault)
	ctx := context.Background()
	base := Settings{Host: "smtp.acme.example", Username: "relay-user", From: "noreply@acme.example"}

	sealed := "not-a-real-relay-password"
	require.NoError(t, store.Save(ctx, base, &sealed, "ops"))
	require.Equal(t, sealed, vault.values[SecretName()])

	// An OMITTED password must leave the sealed one alone. This is the case
	// that erases a credential when absent and empty are collapsed: the
	// operator edits the host, the form sends no password, and every message
	// after that is refused at AUTH.
	changed := base
	changed.Host = "smtp2.acme.example"
	require.NoError(t, store.Save(ctx, changed, nil, "ops"))
	require.Equal(t, sealed, vault.values[SecretName()], "an unrelated edit erased the sealed password")

	// An EMPTY password clears it — and the user name has to go with it, or
	// the relay refuses the session.
	empty := ""
	require.Error(t, store.Save(ctx, changed, &empty, "ops"),
		"a user name with no password was accepted")
	withoutAuth := changed
	withoutAuth.Username = ""
	require.NoError(t, store.Save(ctx, withoutAuth, &empty, "ops"))
	_, stillSealed, err := store.Load(ctx)
	require.NoError(t, err)
	require.False(t, stillSealed)
	require.NotContains(t, vault.values, SecretName())
}

func TestSaveRefusesAnInvalidValueBeforeTouchingTheVault(t *testing.T) {
	pool := newEmailPool(t)
	vault := newMemoryVault()
	store := NewStore(pool, vault)
	ctx := context.Background()

	password := "not-a-real-relay-password"
	err := store.Save(ctx, Settings{Host: "h", Username: "u", From: "not an address"}, &password, "ops")

	var field FieldError
	require.True(t, errors.As(err, &field))
	require.Equal(t, "from", field.Field)
	// A refused save must seal nothing and write nothing. Sealing before the
	// last refusal could pass is how a 400 overwrites a live credential on its
	// way out.
	require.Empty(t, vault.values)
	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.platform_config WHERE section = $1`, Section).Scan(&rows))
	require.Zero(t, rows)
}

func TestPasswordSealedDistinguishesAbsentFromUnreadable(t *testing.T) {
	// "The operator has not set a password" and "this service cannot read its
	// own vault" call for different messages. Collapsing them is how a broken
	// vault gets reported to an operator as a missing configuration.
	pool := newEmailPool(t)
	store := NewStore(pool, newMemoryVault())
	sealed, err := store.PasswordSealed(context.Background())
	require.NoError(t, err)
	require.False(t, sealed)

	broken := NewStore(pool, brokenVault{})
	_, err = broken.PasswordSealed(context.Background())
	require.Error(t, err)
}

type brokenVault struct{}

func (brokenVault) StoreAdminHiddenSecret(context.Context, string, string) error {
	return errors.New("vault unavailable")
}
func (brokenVault) LookupAdminHiddenSecret(context.Context, string) (string, error) {
	return "", errors.New("vault unavailable")
}
func (brokenVault) DeleteAdminHiddenSecret(context.Context, string) error {
	return errors.New("vault unavailable")
}

func newEmailPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	require.NoError(t, adminPool.Ping(ctx))

	databaseName := fmt.Sprintf("elitea_email_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	// The table this package writes, as `001_initial.sql` declares it. No new
	// migration is needed for this feature: `centry.platform_config` has held
	// arbitrary sections since the beginning, and the credential — the one
	// value that could not live in a plaintext row — goes to the vault
	// instead.
	_, err = pool.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL,
    key VARCHAR(128) NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_by VARCHAR(255),
    PRIMARY KEY (section, key)
);`)
	require.NoError(t, err)
	return pool
}
