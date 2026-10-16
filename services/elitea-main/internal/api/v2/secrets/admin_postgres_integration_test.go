package secrets

// Unit A14 acceptance for the GLOBAL secret vault (admin.go).
//
// The failure this file exists to make impossible is not "the endpoint 500s".
// It is "the endpoint answered 200 and wrote the WRONG STORE". A global-vault
// write is platform-wide and silent, so every test here proves WHICH row moved,
// not merely that a request succeeded:
//
//  1. Every write is re-read through the product's own GET — the `{"secret": …}`
//     body the page calls — never through the handler's internals. A 200 proves
//     nothing; #130/#180 both shipped handlers that returned success and did
//     nothing.
//  2. Every write is followed by an assertion that the PROJECT vaults did not
//     move. That is the specific hazard the pre-A14 501 was guarding against:
//     admin_ui sends the placeholder project id `0`, and a handler that keyed on
//     it would read and write `project-0` while looking perfectly healthy.
//  3. The negative authorisation case mounts the REAL route middleware and
//     proves a caller without the permission is REFUSED and that nothing moved
//     — not merely that a control would be hidden.
//
// No test asserts on a secret value that means anything; the fixtures are
// marker strings.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── fixtures ──────────────────────────────────────────────────────────── */

// Marker values. None of these is or resembles a credential.
const (
	probeName    = "e2e_probe"
	probeValue   = "marker-one"
	probeUpdated = "marker-two"
	renamedName  = "e2e_probe_renamed"

	// A secret that already lives in the global vault before each test, so
	// "the vault still has its other contents" is assertable after every write.
	residentName  = "resident_marker"
	residentValue = "marker-resident"

	// The project vaults that must never move. `project-0` is the one
	// admin_ui's placeholder id would address.
	projectZeroSecret = "project_zero_marker"
	projectOneSecret  = "project_one_marker"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context, user auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, user, mode, projectID)
}

// grantingResolver answers with exactly the permissions given, for any caller.
func grantingResolver(permissions ...string) permissionResolverFunc {
	return func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
	}
}

// allSecretPermissions is a caller who may do everything, in BOTH modes: the
// four an administration-mode `admin` holds on the reference deployment, plus
// the three the project routes add. The project ones are needed because these
// tests also read the PROJECT surface (to prove the two stores stay separate),
// and those routes are gated too — see project_postgres_integration_test.go.
// A resolver limited to the admin four would make those reads 403 and the
// store-separation assertions unreachable.
func allSecretPermissions() permissionResolverFunc {
	return grantingResolver(
		permSecretView, permSecretCreate, permSecretEdit, permSecretDelete,
		permSecretList, permSecretUnsecret, permSecretHide,
	)
}

// secretsRouter mounts the handler's OWN Routes() — the real thing, including
// the real mode dispatch and the real in-package permission gates. Nothing here
// re-implements the routing, so a test cannot pass against a route table the
// server does not have.
func secretsRouter(t *testing.T, pool *pgxpool.Pool, resolver auth.PermissionResolver) chi.Router {
	t.Helper()
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1", UserID: "1"})))
		})
	})
	router.Mount("/secrets", NewHandler(pool, WithPermissionResolver(resolver)).Routes())
	return router
}

func do(t *testing.T, router chi.Router, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const adminBase = "/secrets/secret/administration/0/"

// readAdminSecret re-reads one global secret through the PRODUCT's GET. The
// body is pylon's `{"secret": <value|null>}`.
func readAdminSecret(t *testing.T, router chi.Router, name string) (string, bool) {
	t.Helper()
	recorder := do(t, router, http.MethodGet, adminBase+name, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", name, recorder.Code, recorder.Body.String())
	}
	var body struct {
		Secret *string `json:"secret"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode reveal body: %v", err)
	}
	if body.Secret == nil {
		return "", false
	}
	return *body.Secret, true
}

// listAdminSecretNames re-reads the listing through the PRODUCT's GET and
// asserts every row is masked — the listing must never carry plaintext.
func listAdminSecretNames(t *testing.T, router chi.Router) []string {
	t.Helper()
	recorder := do(t, router, http.MethodGet, "/secrets/secrets/administration/0", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET listing status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var rows []adminSecretListItem
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode listing %q: %v", recorder.Body.String(), err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Secret != adminSecretMask {
			t.Fatalf("listing row %q carries %q, not the mask — the listing must never expose plaintext",
				row.Name, row.Secret)
		}
		names = append(names, row.Name)
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

/* ── store-identity probes (raw SQL, not the handler) ──────────────────── */

// vaultRowDigest returns the two stored blobs for one vault id. Comparing these
// before and after a write is what proves WHICH row moved, independently of any
// handler that might be reading the wrong one.
func vaultRowDigest(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var keyRow, dataRow []byte
	err := pool.QueryRow(context.Background(),
		`SELECT k.data, d.data FROM centry.secrets_key k
		 JOIN centry.secrets_data d ON d.id = k.id WHERE k.id = $1`, id).Scan(&keyRow, &dataRow)
	if err == pgx.ErrNoRows {
		return "<absent>"
	}
	if err != nil {
		t.Fatalf("read vault row %q: %v", id, err)
	}
	return fmt.Sprintf("%x/%x", keyRow, dataRow)
}

/* ── database ──────────────────────────────────────────────────────────── */

func newSecretsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_admin_secrets_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE centry.secrets_key  (id TEXT PRIMARY KEY, data BYTEA NOT NULL);
CREATE TABLE centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA NOT NULL);`); err != nil {
		t.Fatalf("create centry vault tables: %v", err)
	}
	return pool
}

// prepareVaults seeds the global vault plus the two project vaults a wrong
// handler would address. They are written through the handler's own writers, so
// the fixture is in exactly the format the product reads.
func prepareVaults(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	handler := NewHandler(pool)

	if err := handler.writeAdminVault(ctx, vaultData{
		Secrets:       map[string]string{residentName: residentValue},
		HiddenSecrets: map[string]string{},
	}); err != nil {
		t.Fatalf("seed global vault: %v", err)
	}
	for projectID, name := range map[string]string{"0": projectZeroSecret, "1": projectOneSecret} {
		if err := handler.writeVaultCtx(ctx, projectID, vaultData{
			Secrets:       map[string]string{name: "marker-" + projectID},
			HiddenSecrets: map[string]string{},
		}); err != nil {
			t.Fatalf("seed project %s vault: %v", projectID, err)
		}
	}
}

/* ── the round trip ────────────────────────────────────────────────────── */

func TestAdminSecretCreateUpdateRenameAndDeleteSurviveAReReadThroughTheProductGET(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	// ── create ────────────────────────────────────────────────────────────
	if code := do(t, router, http.MethodPost, adminBase+probeName,
		map[string]string{"secret": probeValue}).Code; code != http.StatusOK {
		t.Fatalf("create status = %d, want 200", code)
	}
	value, found := readAdminSecret(t, router, probeName)
	if !found || value != probeValue {
		t.Fatalf("after create, GET returned (%q, %v); want the value that was written", value, found)
	}
	if names := listAdminSecretNames(t, router); !contains(names, probeName) || !contains(names, residentName) {
		t.Fatalf("listing = %v; want both the new secret and the resident one", names)
	}

	// ── update in place (old_name == the path name; what admin_ui sends) ───
	update := map[string]any{"secret": map[string]string{"old_name": probeName, "value": probeUpdated}}
	if code := do(t, router, http.MethodPut, adminBase+probeName, update).Code; code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", code)
	}
	if value, _ := readAdminSecret(t, router, probeName); value != probeUpdated {
		t.Fatalf("after update, GET returned %q; the write did not land", value)
	}

	// ── rename (old_name != the path name) ────────────────────────────────
	rename := map[string]any{"secret": map[string]string{"old_name": probeName, "value": probeUpdated}}
	if code := do(t, router, http.MethodPut, adminBase+renamedName, rename).Code; code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", code)
	}
	if value, found := readAdminSecret(t, router, renamedName); !found || value != probeUpdated {
		t.Fatalf("after rename, GET %s returned (%q, %v)", renamedName, value, found)
	}
	if _, found := readAdminSecret(t, router, probeName); found {
		t.Fatalf("after rename, the OLD name is still readable — the rename copied instead of moving")
	}

	// ── delete ────────────────────────────────────────────────────────────
	if code := do(t, router, http.MethodDelete, adminBase+renamedName, nil).Code; code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
	if _, found := readAdminSecret(t, router, renamedName); found {
		t.Fatalf("after delete, GET still returns a value")
	}
	names := listAdminSecretNames(t, router)
	if contains(names, renamedName) {
		t.Fatalf("after delete, the listing still names it: %v", names)
	}
	// The rest of the vault survived every one of those writes.
	if !contains(names, residentName) {
		t.Fatalf("the resident secret is gone: %v — a write replaced the vault instead of editing it", names)
	}
}

/* ── the store-identity proof ──────────────────────────────────────────── */

// The defect the pre-A14 501 was preventing: admin_ui sends the placeholder
// project id `0`, so a handler keyed on it would read and WRITE `project-0`.
// This asserts the admin row moved and BOTH project rows did not — byte for
// byte — and that the project handler still answers with its own contents.
func TestAdminSecretWritesTheAdminRowAndNeverAProjectVault(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	adminBefore := vaultRowDigest(t, pool, "admin")
	zeroBefore := vaultRowDigest(t, pool, "project-0")
	oneBefore := vaultRowDigest(t, pool, "project-1")

	for _, write := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, adminBase + probeName, map[string]string{"secret": probeValue}},
		{http.MethodPut, adminBase + probeName,
			map[string]any{"secret": map[string]string{"old_name": probeName, "value": probeUpdated}}},
		{http.MethodDelete, adminBase + probeName, nil},
	} {
		recorder := do(t, router, write.method, write.path, write.body)
		if recorder.Code >= 400 {
			t.Fatalf("%s %s status = %d (body %s)", write.method, write.path, recorder.Code, recorder.Body.String())
		}
	}

	if got := vaultRowDigest(t, pool, "admin"); got == adminBefore {
		t.Fatalf("three writes left the `admin` vault row byte-identical — nothing was written anywhere")
	}
	if got := vaultRowDigest(t, pool, "project-0"); got != zeroBefore {
		t.Fatalf("an administration-mode write changed `project-0`; the placeholder project id is being used as a key")
	}
	if got := vaultRowDigest(t, pool, "project-1"); got != oneBefore {
		t.Fatalf("an administration-mode write changed `project-1`")
	}

	// …and the project surface still answers with its own contents, read
	// through the PROJECT handler's own GET.
	recorder := do(t, router, http.MethodGet, "/secrets/secret/default/0/"+projectZeroSecret, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "marker-0") {
		t.Fatalf("project 0's own secret is no longer readable: %d %s", recorder.Code, recorder.Body.String())
	}
	// The global secret is NOT visible through the project surface. The two
	// stores are separate; a handler serving one from the other would show it.
	if recorder := do(t, router, http.MethodGet, "/secrets/secret/default/0/"+residentName, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("the global vault's secret is readable through the PROJECT handler (status %d): the stores are not separate",
			recorder.Code)
	}
}

/* ── authorisation ─────────────────────────────────────────────────────── */

// The negative case. `window.admin_ui_config.permissions` is presentation
// state; these route gates are the authorisation, and a caller holding only
// the VIEW permission must be refused every write — and must not have written.
func TestAdminSecretsRefuseACallerWithoutTheWritePermissions(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)

	guarded := secretsRouter(t, pool, grantingResolver(permSecretView))
	// A second, fully-permitted router is used only to READ back, so "nothing
	// moved" does not depend on the gate under test.
	open := secretsRouter(t, pool, allSecretPermissions())

	before := vaultRowDigest(t, pool, "admin")

	for _, attempt := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, adminBase + probeName, map[string]string{"secret": probeValue}},
		{http.MethodPut, adminBase + residentName,
			map[string]any{"secret": map[string]string{"old_name": residentName, "value": probeValue}}},
		{http.MethodDelete, adminBase + residentName, nil},
	} {
		recorder := do(t, guarded, attempt.method, attempt.path, attempt.body)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s as a view-only caller status = %d, want 403 (body %s)",
				attempt.method, recorder.Code, recorder.Body.String())
		}
	}

	if got := vaultRowDigest(t, pool, "admin"); got != before {
		t.Fatalf("a refused write changed the vault anyway")
	}
	if value, found := readAdminSecret(t, open, residentName); !found || value != residentValue {
		t.Fatalf("the resident secret changed under refused writes: (%q, %v)", value, found)
	}
	if _, found := readAdminSecret(t, open, probeName); found {
		t.Fatalf("a refused create wrote the secret anyway")
	}
}

// And the READ is gated: the global vault is the platform's shared credentials.
// The listing is gated on the same permission — a name is a disclosure too.
func TestAdminSecretReadsAreGated(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	// A caller with every WRITE permission and no read permission. This is not
	// hypothetical: on the reference deployment the administration-mode
	// `editor` role holds exactly that set.
	router := secretsRouter(t, pool, grantingResolver(permSecretCreate, permSecretEdit, permSecretDelete))

	for _, path := range []string{"/secrets/secrets/administration/0", adminBase + residentName} {
		if code := do(t, router, http.MethodGet, path, nil).Code; code != http.StatusForbidden {
			t.Fatalf("GET %s without the view permission status = %d, want 403", path, code)
		}
	}
}

// A Handler built with no resolver at all — the two programmatic constructors
// in applications/ and conversations/ — must expose nothing.
func TestAdminSecretsFailClosedWithoutAResolver(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, nil)

	if code := do(t, router, http.MethodGet, "/secrets/secrets/administration/0", nil).Code; code != http.StatusForbidden {
		t.Fatalf("listing without a resolver status = %d, want 403", code)
	}
	if code := do(t, router, http.MethodPost, adminBase+probeName, map[string]string{"secret": probeValue}).Code; code != http.StatusForbidden {
		t.Fatalf("create without a resolver status = %d, want 403", code)
	}
}

/* ── the destructive-write guards ──────────────────────────────────────── */

// Pylon's admin POST assigns into the dict unconditionally, so creating a name
// that already exists destroys the current value with a 200. Here it is a 400,
// and the existing value is intact.
func TestAdminSecretCreateDoesNotOverwriteAnExistingName(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	recorder := do(t, router, http.MethodPost, adminBase+residentName, map[string]string{"secret": probeValue})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("re-creating an existing name status = %d, want 400", recorder.Code)
	}
	if value, _ := readAdminSecret(t, router, residentName); value != residentValue {
		t.Fatalf("the refused create overwrote the existing value anyway")
	}
}

// A rename onto an occupied name would destroy that entry just as silently.
func TestAdminSecretRenameDoesNotOverwriteAnExistingName(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	if code := do(t, router, http.MethodPost, adminBase+probeName,
		map[string]string{"secret": probeValue}).Code; code != http.StatusOK {
		t.Fatalf("seed create failed with %d", code)
	}
	rename := map[string]any{"secret": map[string]string{"old_name": probeName, "value": probeValue}}
	if code := do(t, router, http.MethodPut, adminBase+residentName, rename).Code; code != http.StatusBadRequest {
		t.Fatalf("renaming onto an occupied name status = %d, want 400", code)
	}
	if value, _ := readAdminSecret(t, router, residentName); value != residentValue {
		t.Fatalf("the refused rename overwrote the occupied name anyway")
	}
	if value, found := readAdminSecret(t, router, probeName); !found || value != probeValue {
		t.Fatalf("the refused rename removed the source: (%q, %v)", value, found)
	}
}

// The project path's readOrInitVault writes a fresh EMPTY vault whenever the
// read fails, including when the rows exist but cannot be opened. On the global
// vault that is a platform-wide wipe reported as a successful first write.
func TestAdminSecretWriteRefusesAnUnreadableVaultInsteadOfReplacingIt(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	if _, err := pool.Exec(context.Background(),
		`UPDATE centry.secrets_data SET data = $1 WHERE id = 'admin'`,
		[]byte("this is not a fernet token"),
	); err != nil {
		t.Fatalf("corrupt the vault data row: %v", err)
	}
	corrupted := vaultRowDigest(t, pool, "admin")

	recorder := do(t, router, http.MethodPost, adminBase+probeName, map[string]string{"secret": probeValue})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("create against an unreadable vault status = %d, want 500 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if got := vaultRowDigest(t, pool, "admin"); got != corrupted {
		t.Fatalf("the failed write REPLACED the unreadable vault — that is a silent platform-wide secret wipe")
	}
	// The same must hold for the reads: they report the failure rather than
	// answering "there is nothing here", which would invite exactly that write.
	if code := do(t, router, http.MethodGet, "/secrets/secrets/administration/0", nil).Code; code != http.StatusInternalServerError {
		t.Fatalf("listing an unreadable vault status = %d, want 500", code)
	}
}

// A name outside `[A-Za-z0-9_]` can be stored but never resolved, because that
// is the class `{{secret.<name>}}` interpolation matches. Pylon accepts it.
func TestAdminSecretCreateRejectsAnUnresolvableName(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	recorder := do(t, router, http.MethodPost, adminBase+"not.a.valid.name", map[string]string{"secret": probeValue})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("creating an unresolvable name status = %d, want 400", recorder.Code)
	}
	if names := listAdminSecretNames(t, router); contains(names, "not.a.valid.name") {
		t.Fatalf("the rejected name was stored anyway: %v", names)
	}
}

// Updating a name that does not exist is pylon's 404, not a create.
func TestAdminSecretUpdateOfAnUnknownNameIsNotACreate(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	body := map[string]any{"secret": map[string]string{"old_name": probeName, "value": probeValue}}
	if code := do(t, router, http.MethodPut, adminBase+probeName, body).Code; code != http.StatusNotFound {
		t.Fatalf("updating an unknown name status = %d, want 404", code)
	}
	if _, found := readAdminSecret(t, router, probeName); found {
		t.Fatalf("the refused update created the secret")
	}
}

/* ── key format (#196/#197) ────────────────────────────────────────────── */

// The global vault is where the chat-config limits live, and its key row is
// read by centrysecrets, which requires the 44-byte base64 form. A first write
// that minted the raw 32 bytes would make those limits unreadable — the exact
// regression #196/#197 fixed on the project path.
func TestAdminVaultFirstWriteStoresCentrysKeyFormat(t *testing.T) {
	pool := newSecretsPool(t)
	router := secretsRouter(t, pool, allSecretPermissions())

	// No vault at all: the listing is an empty set, not an error.
	if names := listAdminSecretNames(t, router); len(names) != 0 {
		t.Fatalf("an uninitialised global vault listed %v, want []", names)
	}
	if code := do(t, router, http.MethodPost, adminBase+probeName,
		map[string]string{"secret": probeValue}).Code; code != http.StatusOK {
		t.Fatalf("first create status = %d, want 200", code)
	}

	var keyRow []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT data FROM centry.secrets_key WHERE id = 'admin'`).Scan(&keyRow); err != nil {
		t.Fatalf("read the minted key row: %v", err)
	}
	if len(keyRow) != 44 {
		t.Fatalf("minted key row is %d bytes; centry stores the 44-byte base64 encoding, "+
			"and centrysecrets rejects anything else", len(keyRow))
	}
	if _, err := fernetDecodeKey(string(keyRow)); err != nil {
		t.Fatalf("the minted key row is not a decodable Fernet key: %v", err)
	}
	if value, found := readAdminSecret(t, router, probeName); !found || value != probeValue {
		t.Fatalf("the first write is not readable back: (%q, %v)", value, found)
	}
}

/* ── the routes pylon defines but does not serve ───────────────────────── */

func TestAdminHideIsRefusedTheWayPylonRefusesIt(t *testing.T) {
	pool := newSecretsPool(t)
	prepareVaults(t, pool)
	router := secretsRouter(t, pool, allSecretPermissions())

	recorder := do(t, router, http.MethodPost, "/secrets/hide/administration/0/"+residentName, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("hide in administration mode status = %d, want 401", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "no hidden secrets in administration mode") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
	// …and it did not hide anything.
	if value, found := readAdminSecret(t, router, residentName); !found || value != residentValue {
		t.Fatalf("the refused hide moved the secret: (%q, %v)", value, found)
	}
}
