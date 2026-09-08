package emailsettings

// An UNINITIALISED global vault reads as "no password", not as a failure.
//
// The defect this file pins down is the "absent reads as failure" class. On a
// FRESH install nothing has sealed a credential yet, so neither
// `centry.secrets_key` nor `centry.secrets_data` holds a row and every lookup
// answers `secrets.ErrVaultAbsent`. `Store.PasswordSealed` treated that as an
// error, `Load` returned it, and `Resolve` logged
//
//	outbound e-mail settings could not be read; using the environment defaults
//	error=secrets: vault has not been initialised
//
// on every send and on every read of Admin › E-mail, then answered from the
// ENVIRONMENT while the host the operator had typed sat unused in
// `centry.platform_config`. `Store.Save` failed the same way whenever the form
// omitted the password, because the user name/password pair check reads the
// sealed state first.
//
// The vault that will NOT OPEN keeps its error. That distinction is the reason
// `secrets` has two sentinels, and collapsing it would report a broken vault as
// a missing configuration.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// uninitialisedVault is the global vault of a fresh install. Every lookup
// answers ErrVaultAbsent, WRAPPED the way `secrets.Handler` returns it, so the
// test also proves the check unwraps rather than comparing with ==.
type uninitialisedVault struct{}

func (uninitialisedVault) StoreAdminHiddenSecret(context.Context, string, string) error { return nil }

func (uninitialisedVault) LookupAdminHiddenSecret(_ context.Context, name string) (string, error) {
	return "", fmt.Errorf("look up %s: %w", name, secrets.ErrVaultAbsent)
}

func (uninitialisedVault) DeleteAdminHiddenSecret(context.Context, string) error { return nil }

// unreadableVault is the OTHER absence: rows exist and would not open — a wrong
// SECRETS_MASTER_KEY, a body that is not a Fernet token. It must stay an error.
type unreadableVault struct{}

func (unreadableVault) StoreAdminHiddenSecret(context.Context, string, string) error { return nil }

func (unreadableVault) LookupAdminHiddenSecret(context.Context, string) (string, error) {
	return "", errors.New("decrypt admin vault: cipher: message authentication failed")
}

func (unreadableVault) DeleteAdminHiddenSecret(context.Context, string) error { return nil }

func TestAnUninitialisedVaultReadsAsNoPassword(t *testing.T) {
	// No pool is needed: both readers touch the vault only, and this is the
	// exact pair of calls `Load`, `Save` and `Resolve` make.
	store := NewStore(nil, uninitialisedVault{})
	ctx := context.Background()

	sealed, err := store.PasswordSealed(ctx)
	require.NoError(t, err, "an uninitialised vault was reported as a read failure")
	require.False(t, sealed)

	password, err := store.Password(ctx)
	require.NoError(t, err)
	require.Empty(t, password)
}

func TestAnUnreadableVaultIsStillAnError(t *testing.T) {
	store := NewStore(nil, unreadableVault{})
	ctx := context.Background()

	_, err := store.PasswordSealed(ctx)
	require.Error(t, err, "a vault that would not open was reported as an empty one")
	_, err = store.Password(ctx)
	require.Error(t, err)
}

func TestResolveOverAnUninitialisedVaultKeepsTheStoredLayerAndIsSilent(t *testing.T) {
	// The end-to-end shape of the fresh install, over a real database: the
	// stored rows win, the environment fills the rest, `password_set` is a
	// clean false, and nothing is logged.
	pool := newEmailPool(t)
	store := NewStore(pool, uninitialisedVault{})
	ctx := context.Background()

	// A save with NO password must survive an uninitialised vault too.
	require.NoError(t, store.Save(ctx, Settings{
		Host: "smtp.acme.example", From: "noreply@acme.example",
	}, nil, "ops@acme.example"))

	warnings := captureWarnings(t)
	resolution := NewResolver(store, Settings{
		Port: 25, TLS: TLSNone, From: "chart@acme.example",
		PublicBaseURL: "https://chart.acme.example",
	}, "").Resolve(ctx)

	require.True(t, resolution.Configured, "reason: %s", resolution.Reason)
	require.Equal(t, "smtp.acme.example", resolution.Stored.Host)
	require.Equal(t, "smtp.acme.example", resolution.Effective.Host)
	require.Equal(t, "noreply@acme.example", resolution.Effective.From)
	// A field the database does not state still comes from the environment.
	require.Equal(t, "https://chart.acme.example", resolution.Effective.PublicBaseURL)
	require.Equal(t, 25, resolution.Effective.Port)
	// What `GET /admin/email/administration` answers as `password_set`.
	require.False(t, resolution.PasswordSet)
	require.Equal(t, SourceUnset, resolution.PasswordSource)
	require.Empty(t, warnings.String(), "an uninitialised vault logged a warning")
}

func TestResolveStillConsultsAnInitialisedVault(t *testing.T) {
	// The other half of the rule. A fix that answered "no password" for every
	// vault failure would pass the test above and break this one.
	pool := newEmailPool(t)
	vault := newMemoryVault()
	store := NewStore(pool, vault)
	ctx := context.Background()

	sealed := "not-a-real-relay-password"
	require.NoError(t, store.Save(ctx, Settings{
		Host: "smtp.acme.example", Username: "relay-user", From: "noreply@acme.example",
	}, &sealed, "ops@acme.example"))

	resolution := NewResolver(store, Settings{
		PublicBaseURL: "https://chart.acme.example",
	}, "").Resolve(ctx)

	require.True(t, resolution.Configured, "reason: %s", resolution.Reason)
	require.Equal(t, sealed, resolution.Config.Password)
	require.True(t, resolution.PasswordSet)
	require.Equal(t, SourceDatabase, resolution.PasswordSource)
}

// captureWarnings redirects the default logger for one test and returns what it
// received. The log line IS the defect here: the resolver kept answering, so
// only that line said a stored configuration had been discarded.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}
