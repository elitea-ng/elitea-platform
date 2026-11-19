package microsoftapps

// The registry against a real PostgreSQL: the shipped migration corpus (shared
// 0255 included) applied to a template database, then the store, the resolver and
// the seed over the real tables and the real security audit outbox. CI-only:
// skipped without ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	urlEnv    = "ELITEA_TEST_DATABASE_URL"
	bootstrap = "../infra/db/migrations/001_initial.sql"
)

var (
	templateOnce sync.Once
	template     string
	templateErr  error
)

func newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(urlEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the Microsoft apps integration test", urlEnv)
	}
	templateOnce.Do(func() {
		seed, err := os.ReadFile(bootstrap)
		if err != nil {
			templateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			templateErr = err
			return
		}
		defer admin.Close()
		template, templateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{Files: platformmigrations.Files, Seed: string(seed)})
	})
	require.NoError(t, templateErr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_msapps_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, template, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

func newStore(t *testing.T, pool *pgxpool.Pool) *Store {
	t.Helper()
	audit, err := securityaudit.NewPostgresAppender("elitea-main-test")
	require.NoError(t, err)
	store, err := NewStore(pool, bytes.Repeat([]byte{0x42}, 32), audit)
	require.NoError(t, err)
	return store
}

var admin = Actor{UserID: 7}

func secretPtr(s string) *string { return &s }

func TestMigrationGrantsThePermissionAndIsIdempotent(t *testing.T) {
	pool := newDatabase(t)
	ctx := context.Background()
	for _, role := range []string{"super_admin", "admin"} {
		var granted bool
		require.NoError(t, pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__role_permission p JOIN public.auth_core__role r ON r.id = p.role_id
                WHERE r.mode = 'administration' AND r.name = $1 AND p.permission = 'configuration.microsoft_apps')`, role).Scan(&granted))
		require.True(t, granted, "%s in administration mode must hold configuration.microsoft_apps on a fresh database", role)
	}
	var editorGranted bool
	require.NoError(t, pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__role_permission p WHERE p.permission = 'configuration.microsoft_apps'
                 AND p.role_id IN (SELECT id FROM public.auth_core__role WHERE name NOT IN ('super_admin', 'admin')))`).Scan(&editorGranted))
	require.False(t, editorGranted, "only super_admin and admin hold it")

	file, err := os.ReadFile("../../migrations/shared/0255_microsoft_oauth_apps.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(file))
	require.NoError(t, err, "0255 must be idempotent")

}

func TestTheMigrationRefusesRowsNoReaderCanUse(t *testing.T) {
	pool := newDatabase(t)
	ctx := context.Background()
	insert := func(extra string) error {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.microsoft_oauth_apps (name, client_id, authority, credential_types`+extra)
		return err
	}
	const authority = `'https://login.microsoftonline.com/organizations'`
	good := `) VALUES ('ok', '` + testClientID + `', ` + authority + `, ARRAY['outlook'])`
	require.NoError(t, insert(good))
	for name, sql := range map[string]string{
		"a foreign authority host":       `) VALUES ('a', '` + testClientID + `', 'https://login.microsoftonline.us/x', ARRAY['outlook'])`,
		"a client id that is not a guid": `) VALUES ('b', 'client', ` + authority + `, ARRAY['outlook'])`,
		"no credential type":             `) VALUES ('c', '` + testClientID + `', ` + authority + `, ARRAY[]::text[])`,
		"a type that is not Microsoft":   `) VALUES ('d', '` + testClientID + `', ` + authority + `, ARRAY['gmail'])`,
		"an active app with no secret":   `, state) VALUES ('e', '` + testClientID + `', ` + authority + `, ARRAY['outlook'], 'active')`,
		"a ciphertext without a key":     `, client_secret_ciphertext) VALUES ('f', '` + testClientID + `', ` + authority + `, ARRAY['outlook'], '\x00'::bytea)`,
		"a duplicate name":               `) VALUES ('OK ', '` + testClientID + `', ` + authority + `, ARRAY['outlook'])`,
	} {
		require.Error(t, insert(sql), name)
	}
}

func TestCreateSealsTheSecretAndNothingReadsItBack(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()

	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	created, err := store.Create(ctx, in, admin)
	require.NoError(t, err)
	require.Equal(t, StateActive, created.State)
	require.True(t, created.SecretSet)
	require.Equal(t, 1, created.Revision)
	require.NotNil(t, created.CreatedBy)

	// The stored bytes are ciphertext under the registry's key label.
	var ciphertext []byte
	var label string
	require.NoError(t, pool.QueryRow(ctx, `SELECT client_secret_ciphertext, key_label FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid`,
		created.ID).Scan(&ciphertext, &label))
	require.Equal(t, KeyLabel, label)
	require.NotContains(t, string(ciphertext), plaintextSecret)

	// No column of the row, no audit event and nothing the store lists carries the
	// plaintext (the write-only guarantee, checked at the database).
	var rowText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(a)::text FROM elitea_identity.microsoft_oauth_apps a WHERE id = $1::uuid`, created.ID).Scan(&rowText))
	require.NotContains(t, rowText, plaintextSecret)
	var events string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text, ' '), '') FROM elitea_runtime.security_audit_outbox`).Scan(&events))
	require.NotContains(t, events, plaintextSecret)
	require.NotContains(t, events, fmt.Sprintf("%x", ciphertext))
	require.Contains(t, events, ActionCreated)
	require.Contains(t, events, "user:7")

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotContains(t, fmt.Sprintf("%+v", listed), plaintextSecret)

	// Only the resolver reads it, and only after the availability checks.
	resolver := NewResolver(store)
	resolved, err := resolver.Resolve(ctx, Request{AppID: created.ID, CredentialType: TypeOutlook, ProjectID: 1})
	require.NoError(t, err)
	require.Equal(t, plaintextSecret, resolved.Secret.Reveal())
	require.NotContains(t, fmt.Sprintf("%v %+v %#v", resolved, resolved, resolved), plaintextSecret)
}

func TestACreatedAppWithoutASecretIsADraftAndCannotBeUsed(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	draft, err := store.Create(ctx, validInput(), admin)
	require.NoError(t, err)
	require.Equal(t, StateDraft, draft.State)
	require.False(t, draft.SecretSet)

	_, err = NewResolver(store).Resolve(ctx, Request{AppID: draft.ID, CredentialType: TypeOutlook, ProjectID: 1})
	require.Equal(t, CodeAppDraft, codeOf(t, err))
	offers, err := NewResolver(store).Offers(ctx, TypeOutlook, 1)
	require.NoError(t, err)
	require.Empty(t, offers, "a draft is not offered")

	// Setting the secret makes it active.
	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	active, err := store.Update(ctx, draft.ID, in, admin)
	require.NoError(t, err)
	require.Equal(t, StateActive, active.State)
	require.Equal(t, 2, active.Revision)
}

func TestNamesAreUniqueAndAnUnknownAppIsNotFound(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	_, err := store.Create(ctx, validInput(), admin)
	require.NoError(t, err)
	_, err = store.Create(ctx, validInput(), admin)
	require.ErrorIs(t, err, ErrConflict)

	_, err = store.Get(ctx, "not-a-uuid")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Get(ctx, "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Update(ctx, "00000000-0000-4000-8000-000000000000", validInput(), admin)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.SetDisabled(ctx, "00000000-0000-4000-8000-000000000000", true, admin)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEditingKeepsTheSecretAndTheClientOwnsIt(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	app, err := store.Create(ctx, in, admin)
	require.NoError(t, err)
	resolver := NewResolver(store)
	request := Request{AppID: app.ID, CredentialType: TypeOutlook, ProjectID: 1}

	// An edit without a secret keeps the sealed one.
	edit := validInput()
	edit.Name = "Renamed"
	edit.Authority = "https://login.microsoftonline.com/organizations"
	edit.ScopeCeiling = map[string][]string{TypeOutlook: {"Mail.Read", "Mail.Send"}}
	updated, err := store.Update(ctx, app.ID, edit, admin)
	require.NoError(t, err)
	require.Equal(t, "Renamed", updated.Name)
	require.True(t, updated.SecretSet)
	resolved, err := resolver.Resolve(ctx, request)
	require.NoError(t, err)
	require.Equal(t, plaintextSecret, resolved.Secret.Reveal())
	require.Equal(t, "https://login.microsoftonline.com/organizations", resolved.Authority)
	require.Equal(t, []string{"Mail.Read", "Mail.Send"}, resolved.Ceiling)

	// A new client id without its secret is refused: the old secret is not the new
	// client's.
	moved := edit
	moved.ClientID = "7f8d0c1e-0000-4000-8000-000000000002"
	_, err = store.Update(ctx, app.ID, moved, admin)
	var validation ValidationError
	require.True(t, errors.As(err, &validation))
	require.Equal(t, "secret", validation.Field)
	moved.Secret = secretPtr("new-client-secret")
	_, err = store.Update(ctx, app.ID, moved, admin)
	require.NoError(t, err)
	resolved, err = resolver.Resolve(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "new-client-secret", resolved.Secret.Reveal())
	require.Equal(t, moved.ClientID, resolved.ClientID)

	// Clearing the secret makes it a draft again.
	cleared := moved
	cleared.Secret = secretPtr("")
	draft, err := store.Update(ctx, app.ID, cleared, admin)
	require.NoError(t, err)
	require.Equal(t, StateDraft, draft.State)
	require.False(t, draft.SecretSet)

	var secretEvents int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event->'data'->>'action' IN ($1, $2)`,
		ActionSecretSet, ActionSecretCleared).Scan(&secretEvents))
	require.Equal(t, 2, secretEvents, "the replaced and the cleared secret are audited; the one set at create is part of the created event")
}

func TestADisabledAppStaysDisabledThroughEditsAndIsNotOffered(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	app, err := store.Create(ctx, in, admin)
	require.NoError(t, err)

	disabled, err := store.SetDisabled(ctx, app.ID, true, admin)
	require.NoError(t, err)
	require.Equal(t, StateDisabled, disabled.State)

	_, err = store.Update(ctx, app.ID, validInput(), admin)
	require.NoError(t, err)
	got, err := store.Get(ctx, app.ID)
	require.NoError(t, err)
	require.Equal(t, StateDisabled, got.State, "an edit must not re-enable what an administrator disabled")

	resolver := NewResolver(store)
	_, err = resolver.Resolve(ctx, Request{AppID: app.ID, CredentialType: TypeOutlook, ProjectID: 1})
	require.Equal(t, CodeAppDisabled, codeOf(t, err))
	offers, err := resolver.Offers(ctx, TypeOutlook, 1)
	require.NoError(t, err)
	require.Empty(t, offers)

	enabled, err := store.SetDisabled(ctx, app.ID, false, admin)
	require.NoError(t, err)
	require.Equal(t, StateActive, enabled.State)
}

func TestProjectScopeIsStoredAndChecked(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	in.ProjectScope, in.ProjectIDs = ScopeSelected, []int64{12, 5}
	app, err := store.Create(ctx, in, admin)
	require.NoError(t, err)
	require.Equal(t, []int64{5, 12}, app.ProjectIDs)

	resolver := NewResolver(store)
	_, err = resolver.Resolve(ctx, Request{AppID: app.ID, CredentialType: TypeOutlook, ProjectID: 5})
	require.NoError(t, err)
	_, err = resolver.Resolve(ctx, Request{AppID: app.ID, CredentialType: TypeOutlook, ProjectID: 6})
	require.Equal(t, CodeProjectNotServed, codeOf(t, err))
	offers, err := resolver.Offers(ctx, TypeOutlook, 6)
	require.NoError(t, err)
	require.Empty(t, offers)

	in.ProjectIDs = []int64{6}
	app, err = store.Update(ctx, app.ID, in, admin)
	require.NoError(t, err)
	require.Equal(t, []int64{6}, app.ProjectIDs)
	in.ProjectScope, in.ProjectIDs = ScopeAll, nil
	app, err = store.Update(ctx, app.ID, in, admin)
	require.NoError(t, err)
	require.Empty(t, app.ProjectIDs)
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.microsoft_oauth_app_projects`).Scan(&rows))
	require.Zero(t, rows)
}

// A ciphertext that does not open under its row marks the app instead of failing
// silently, and the credential gets a stable error and no other client.
func TestACiphertextMovedToAnotherRowDoesNotOpen(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	in := validInput()
	in.Secret = secretPtr(plaintextSecret)
	first, err := store.Create(ctx, in, admin)
	require.NoError(t, err)
	other := validInput()
	other.Name = "Other"
	other.Secret = secretPtr("other-secret")
	second, err := store.Create(ctx, other, admin)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
UPDATE elitea_identity.microsoft_oauth_apps SET client_secret_ciphertext =
  (SELECT client_secret_ciphertext FROM elitea_identity.microsoft_oauth_apps WHERE id = $2::uuid) WHERE id = $1::uuid`, first.ID, second.ID)
	require.NoError(t, err)

	resolver := NewResolver(store)
	_, err = resolver.Resolve(ctx, Request{AppID: first.ID, CredentialType: TypeOutlook, ProjectID: 1})
	require.Equal(t, CodeSecretUnavailable, codeOf(t, err))
	marked, err := store.Get(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, StateNeedsAttention, marked.State)
	require.Equal(t, ReasonSecretUnreadable, marked.StateReason)
	untouched, err := store.Get(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, StateActive, untouched.State, "another app is not marked")

	// Replacing the secret is the fix.
	fix := validInput()
	fix.Secret = secretPtr(plaintextSecret)
	fixed, err := store.Update(ctx, first.ID, fix, admin)
	require.NoError(t, err)
	require.Equal(t, StateActive, fixed.State)
	_, err = resolver.Resolve(ctx, Request{AppID: first.ID, CredentialType: TypeOutlook, ProjectID: 1})
	require.NoError(t, err)
}

func TestCustomAppsSettingDefaultsOnAndIsAudited(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	allowed, err := store.CustomAppsAllowed(ctx)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, store.SetCustomAppsAllowed(ctx, false, admin))
	allowed, err = store.CustomAppsAllowed(ctx)
	require.NoError(t, err)
	require.False(t, allowed)
	require.False(t, NewResolver(store).CustomAppsAllowed(ctx))
	require.NoError(t, store.SetCustomAppsAllowed(ctx, true, admin))
	allowed, _ = store.CustomAppsAllowed(ctx)
	require.True(t, allowed)

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event->'data'->>'action' = $1`, ActionSettings).Scan(&count))
	require.Equal(t, 2, count)
}

func TestSeedIsIdempotentAndKeepsAnAdministratorsDisable(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	env := map[string]string{
		SeedConfigEnv: `[{"key":"corp","name":"Corp","clientId":"` + testClientID + `",
			"authority":"https://login.microsoftonline.com/organizations","credentialTypes":["outlook","teams"],
			"scopeCeiling":{"teams":["Chat.Read"]}}]`,
		SeedSecretEnv("corp"): plaintextSecret,
	}
	seeds, err := SeedFromEnv(lookupOf(env))
	require.NoError(t, err)

	report, err := store.Seed(ctx, seeds, nil)
	require.NoError(t, err)
	require.Equal(t, SeedReport{Created: 1}, report)
	apps, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, apps, 1)
	require.Equal(t, StateActive, apps[0].State)
	require.Equal(t, "corp", apps[0].SeedKey)
	require.Equal(t, "helm-seed", strings.TrimPrefix(auditPrincipal(t, pool), ""))

	// A restart changes nothing: no row update, no new audit event.
	var before int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox`).Scan(&before))
	report, err = store.Seed(ctx, seeds, nil)
	require.NoError(t, err)
	require.Equal(t, SeedReport{Unchanged: 1}, report)
	var after int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox`).Scan(&after))
	require.Equal(t, before, after)
	got, _ := store.Get(ctx, apps[0].ID)
	require.Equal(t, 1, got.Revision)

	// The administrator disables it; a restart keeps it disabled.
	_, err = store.SetDisabled(ctx, apps[0].ID, true, admin)
	require.NoError(t, err)
	report, err = store.Seed(ctx, seeds, nil)
	require.NoError(t, err)
	require.Equal(t, SeedReport{Unchanged: 1}, report)
	got, _ = store.Get(ctx, apps[0].ID)
	require.Equal(t, StateDisabled, got.State)

	// A changed value updates the same row; a rotated Secret re-seals.
	env[SeedConfigEnv] = strings.Replace(env[SeedConfigEnv], `"name":"Corp"`, `"name":"Corp Prod"`, 1)
	env[SeedSecretEnv("corp")] = "rotated-secret"
	seeds, err = SeedFromEnv(lookupOf(env))
	require.NoError(t, err)
	report, err = store.Seed(ctx, seeds, nil)
	require.NoError(t, err)
	require.Equal(t, SeedReport{Updated: 1}, report)
	apps, _ = store.List(ctx)
	require.Len(t, apps, 1)
	require.Equal(t, "Corp Prod", apps[0].Name)
	require.Equal(t, StateDisabled, apps[0].State)
	secret, err := store.openSecret(ctx, apps[0])
	require.NoError(t, err)
	require.Equal(t, "rotated-secret", secret.Reveal())
}

func auditPrincipal(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var principal string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT event->'data'->>'principal_ref' FROM elitea_runtime.security_audit_outbox WHERE event->'data'->>'action' = $1 ORDER BY seq LIMIT 1`, ActionCreated).Scan(&principal))
	return principal
}

func TestSeedWithoutASecretLeavesADraft(t *testing.T) {
	pool := newDatabase(t)
	store := newStore(t, pool)
	ctx := context.Background()
	seeds, err := SeedFromEnv(lookupOf(map[string]string{SeedConfigEnv: `[{"key":"corp","name":"Corp","clientId":"` + testClientID + `",
		"authority":"https://login.microsoftonline.com/organizations","credentialTypes":["outlook"]}]`}))
	require.NoError(t, err)
	report, err := store.Seed(ctx, seeds, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	apps, _ := store.List(ctx)
	require.Equal(t, StateDraft, apps[0].State)
}
