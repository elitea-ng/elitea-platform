package auth

// The defect: OIDCHandler.provisionUser decided the identity of the session
// from the EMAIL claim alone.
//
// The evidence at the time: the only lookup was
// `INSERT INTO auth_core__user ... ON CONFLICT (email) DO UPDATE ... RETURNING id`.
// The provider subject was never read back — it went into a fire-and-forget
// `INSERT INTO auth_core__user_provider ... ON CONFLICT DO NOTHING`, untargeted,
// so it also swallowed the UNIQUE(provider_ref) violation
// (internal/db/schema/auth_core_baseline.sql:20-27). makeSessionToken then
// minted a 24-hour elitea_session cookie for whatever id the address matched.
//
// Two failures followed:
//
//  1. Any id_token whose email claim named an existing account handed the
//     caller that account. `email_verified` was never read.
//  2. An address change at the identity provider orphaned the account. The same
//     subject arrived with a new email. A SECOND user row was created. The link
//     insert failed on the unique provider_ref and was discarded. The session
//     went to the new empty row, and the real account became unreachable.
//
// The reviewed implementation 40 lines away does it the other way round
// (internal/infra/identityrepo/postgres.go): advisory lock, subject lookup
// first, email only as a fallback. These tests hold this handler to the same
// order. They drive the transaction through a stub, so they need no database.
// The defect survived because this handler had no test file at all.
//
// FAIL-BEFORE. These tests call resolveProvisionedUser, and the same change
// adds that function. So the file cannot compile against the previous commit,
// and no plain fail-before run exists for it. Mutation testing gives the
// evidence instead. Seven mutations of oidc.go all FAIL this package. Each one
// removes a different rule:
//
//	the email fallback runs before the subject lookup
//	the address is not repaired on a linked account
//	the link insert is untargeted again
//	the owner is never read back
//	the bound-account guard is namespace-blind
//	the verified-address rule is dropped
//	a suspended linked account still logs in

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// testBinding is the provider every resolution in these tests asserts from: a
// stored provider key and its configured issuer (issue 2558).
var testBinding = identityrepo.ProviderBinding{Key: "corporate", Issuer: "https://idp.corp.example"}

/* ── transaction stub ──────────────────────────────────────────────────── */

// scriptedRow answers one QueryRow with fixed values, or with an error.
type scriptedRow struct {
	values []any
	err    error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for index, target := range dest {
		if index >= len(r.values) {
			return errors.New("scripted row has too few values")
		}
		switch typed := target.(type) {
		case *int:
			*typed = r.values[index].(int)
		case *bool:
			*typed = r.values[index].(bool)
		default:
			return errors.New("scripted row does not support this destination type")
		}
	}
	return nil
}

// scriptedTx records every statement in order and answers each QueryRow from a
// queue. It embeds pgx.Tx to satisfy the interface; no other method is called.
type scriptedTx struct {
	pgx.Tx
	statements []string
	rows       []scriptedRow
	execErrors map[string]error
}

func (t *scriptedTx) record(sql string) {
	t.statements = append(t.statements, strings.Join(strings.Fields(sql), " "))
}

func (t *scriptedTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	t.record(sql)
	for fragment, err := range t.execErrors {
		if strings.Contains(sql, fragment) {
			return pgconn.CommandTag{}, err
		}
	}
	return pgconn.CommandTag{}, nil
}

func (t *scriptedTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	t.record(sql)
	if len(t.rows) == 0 {
		return scriptedRow{err: errors.New("no scripted row left")}
	}
	row := t.rows[0]
	t.rows = t.rows[1:]
	return row
}

// statementIndex returns the position of the first statement containing the
// fragment, or -1.
func statementIndex(t *testing.T, tx *scriptedTx, fragment string) int {
	t.Helper()
	for index, statement := range tx.statements {
		if strings.Contains(statement, fragment) {
			return index
		}
	}
	return -1
}

/* ── the subject is the identity ───────────────────────────────────────── */

// The subject must be looked up BEFORE the email. This is the whole defect:
// with the order reversed, the address decides the account.
func TestProvisioningReadsTheProviderSubjectBeforeTheEmail(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, true, true}}}}

	userID, err := resolveProvisionedUser(context.Background(), tx, testBinding, "oidc:sub-A", "alice@corp.com", "Alice", nil, false)
	if err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}
	if userID != 7 {
		t.Fatalf("resolved user %d, want the account the subject already owns (7)", userID)
	}

	lock := statementIndex(t, tx, "pg_advisory_xact_lock")
	lookup := statementIndex(t, tx, "auth_core__user_provider AS provider")
	if lock < 0 || lookup < 0 || lock > lookup {
		t.Fatalf("the subject lookup must follow the advisory lock; statements: %v", tx.statements)
	}
	if index := statementIndex(t, tx, "ON CONFLICT (email)"); index >= 0 {
		t.Fatalf("the email upsert ran for a subject that is already linked: %v", tx.statements)
	}
}

// An identity provider that changes a person's address must not orphan the
// account. The subject still names the row, and the row learns the new address.
func TestALinkedSubjectWithANewEmailKeepsItsAccount(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, true, true}}}}

	userID, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-A", "alice.smith@corp.com", "Alice", nil, false,
	)
	if err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}
	if userID != 7 {
		t.Fatalf("resolved user %d, want 7; a changed address must not create a second account", userID)
	}
	if statementIndex(t, tx, "SET last_login = (now() AT TIME ZONE 'UTC'), email =") < 0 {
		t.Fatalf("the account's address was not repaired: %v", tx.statements)
	}
}

// A suspended account still refuses the login, and it refuses it on the path
// that now resolves the account.
func TestALinkedSubjectOnASuspendedAccountIsRefused(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, true, true, true}}}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-A", "alice@corp.com", "Alice", nil, false,
	); !errors.Is(err, errUserSuspended) {
		t.Fatalf("error is %v, want errUserSuspended", err)
	}
}

/* ── the email fallback is narrow ──────────────────────────────────────── */

// The takeover the report leads with. A subject nobody has seen presents the
// address of an account that is already bound to a different subject. The
// address must not hand it over.
func TestANewSubjectCannotJoinAnAccountBoundToAnotherSubject(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows},                       // no link for this subject
		{err: pgx.ErrNoRows},                       // the email upsert refuses the bound row
		{values: []any{false, true, false, false}}, // not suspended, an OIDC subject holds it
	}}

	_, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-B", "alice@corp.com", "Mallory", nil, false,
	)
	if !errors.Is(err, errIdentityConflict) {
		t.Fatalf("error is %v, want errIdentityConflict", err)
	}
	if index := statementIndex(t, tx, "auth_core__user_provider (user_id, provider_ref)"); index >= 0 {
		t.Fatalf("a link was written for a refused login: %v", tx.statements)
	}
}

// A legacy-created database holds BARE provider refs. legacy auth_init writes
// `provider_attr["nameid"]`, which is the preferred_username or the raw subject
// (legacy/plugins/auth_init/rpc/processor.py:55). No row there carries an
// `oidc:` prefix.
//
// A namespace-blind bound-account guard therefore refuses every user who ever
// logged in on the previous platform. The staging dump shows 61 such rows
// against 133 accounts. The guard must read this handler's own namespace only.
func TestTheBoundAccountGuardReadsOnlyThisHandlersNamespace(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows}, // no link for this subject
		{values: []any{11}},  // the upsert adopted the legacy account
		{values: []any{11}},  // the link names that row
	}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-E", "legacy@corp.com", "Legacy", nil, false,
	); err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}

	index := statementIndex(t, tx, "ON CONFLICT (email)")
	if index < 0 {
		t.Fatalf("the email fallback did not run: %v", tx.statements)
	}
	// The guard must still BE there, and must still be scoped rather than blind.
	//
	// It is asserted as a PARAMETERISED match, not as a literal namespace. The
	// literal is what let the SAML path through: this test read
	// `LIKE 'oidc:%'`, SAML wrote `saml:`, and the guard silently stopped
	// applying to it while this assertion went on passing. The namespaces it
	// covers are now a Go value, and WHICH accounts it refuses is proved
	// against a real database in provisioning_postgres_integration_test.go —
	// where a scripted transaction cannot reach, because PostgreSQL does the
	// matching.
	if !strings.Contains(tx.statements[index], "bound.provider_ref LIKE ANY") {
		t.Fatalf("the bound-account guard is missing from the email fallback: %s", tx.statements[index])
	}
	if strings.Contains(tx.statements[index], "'%'") {
		t.Fatalf(
			"the bound-account guard is namespace-blind, so a legacy bare ref locks the "+
				"account out: %s",
			tx.statements[index],
		)
	}
	// And the namespaces it is given are the ones this service writes. A
	// protocol added without extending this list is a protocol the guard does
	// not cover.
	patterns := federatedRefPatterns()
	if len(patterns) != 2 ||
		patterns[0] != OIDCProviderRefPrefix+"%" || patterns[1] != SAMLProviderRefPrefix+"%" {
		t.Fatalf("the guard covers %v, want one pattern per federated namespace", patterns)
	}
}

// The upsert refuses a suspended row and a bound row for the same reason. The
// caller must tell the two apart before it answers.
func TestANewSubjectOnASuspendedAddressIsReportedAsSuspended(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{values: []any{true, false, false, false}}, // suspended
	}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-B", "alice@corp.com", "Alice", nil, false,
	); !errors.Is(err, errUserSuspended) {
		t.Fatalf("error is %v, want errUserSuspended", err)
	}
}

// A first login for an address nobody holds still works. The fallback exists
// for exactly this.
func TestAFirstLoginCreatesAndLinksTheAccount(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows}, // no link for this subject
		{values: []any{11}},  // the upsert created the row
		{values: []any{11}},  // the link names that row
	}}

	userID, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-C", "carol@corp.com", "Carol", nil, false,
	)
	if err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}
	if userID != 11 {
		t.Fatalf("resolved user %d, want 11", userID)
	}
	if !strings.Contains(strings.Join(tx.statements, " | "), "ON CONFLICT (provider_ref) DO NOTHING") {
		t.Fatalf("the link insert is not targeted on provider_ref: %v", tx.statements)
	}
}

// The race backstop. The link insert wrote nothing because another transaction
// owns the subject. Reading the owner back is what turns the swallowed conflict
// into a refusal.
func TestALinkOwnedByAnotherAccountIsRefusedRatherThanSwallowed(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows}, // no link for this subject
		{values: []any{11}},  // the upsert resolved account 11
		{values: []any{7}},   // but the subject belongs to account 7
	}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-A", "carol@corp.com", "Carol", nil, false,
	); !errors.Is(err, errIdentityConflict) {
		t.Fatalf("error is %v, want errIdentityConflict", err)
	}
}

// Requiring a verified address makes an ABSENT email_verified claim fatal on
// the fallback — the one path where the address decides anything.
//
// The flag is a PARAMETER rather than an environment read. It has two sources
// now: the authored provider document's `require_email_verified`, and
// OIDC_REQUIRE_EMAIL_VERIFIED for the environment fallback runtime. Both are
// resolved in oidc_providers.go and arrive here as one boolean, so this test
// covers the rule for either source.
func TestTheEmailFallbackCanRequireAVerifiedAddress(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{err: pgx.ErrNoRows}}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-D", "dave@corp.com", "Dave", nil, true,
	); !errors.Is(err, errEmailNotVerified) {
		t.Fatalf("error is %v, want errEmailNotVerified", err)
	}

	verified := true
	tx = &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{12}},
		{values: []any{12}},
	}}
	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-D", "dave@corp.com", "Dave", &verified, true,
	); err != nil {
		t.Fatalf("a verified address was refused: %v", err)
	}

	// And with the requirement OFF, the same absent claim is accepted. Without
	// this half, a parameter wired to a constant `true` would pass the test
	// above and silently refuse every provider that omits the claim.
	tx = &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{13}},
		{values: []any{13}},
	}}
	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-E", "erin@corp.com", "Erin", nil, false,
	); err != nil {
		t.Fatalf("an absent claim was refused with the requirement off: %v", err)
	}
}

// The environment-configured provider adopts SCIM-provisioned accounts only
// when OIDC_ADOPT_SCIM_USERS says so; unset (the default) is off.
func TestTheEnvironmentProviderAdoptsSCIMUsersOnlyWhenTold(t *testing.T) {
	t.Setenv("OIDC_ADOPT_SCIM_USERS", "")
	if oidcAdoptsSCIMUsers() {
		t.Fatal("an unset OIDC_ADOPT_SCIM_USERS turned SCIM adoption on")
	}
	t.Setenv("OIDC_ADOPT_SCIM_USERS", "TRUE")
	if !oidcAdoptsSCIMUsers() {
		t.Fatal("OIDC_ADOPT_SCIM_USERS=TRUE did not turn SCIM adoption on")
	}
}

// The SCIM adoption guard is part of the email upsert's conflict clause, and
// the provider's setting is bound to it. Which rows it refuses is proved
// against PostgreSQL in provisioning_postgres_integration_test.go.
func TestTheEmailFallbackCarriesTheSCIMAdoptionGuard(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{11}},
		{values: []any{11}},
	}}
	if _, err := resolveProvisionedUserFor(
		context.Background(), tx, testBinding, "oidc:sub-F", "fay@corp.com", "Fay", nil, false, true,
	); err != nil {
		t.Fatalf("resolveProvisionedUserFor: %v", err)
	}
	index := statementIndex(t, tx, "ON CONFLICT (email)")
	if index < 0 || !strings.Contains(tx.statements[index], "elitea_auth.scim_users") {
		t.Fatalf("the email upsert does not consult the SCIM record: %v", tx.statements)
	}
}

// A LINKED subject is not held to the address rule. Its account is decided by
// the subject, so the claim decides nothing there.
func TestALinkedSubjectIsNotHeldToTheVerifiedAddressRule(t *testing.T) {
	t.Setenv("OIDC_REQUIRE_EMAIL_VERIFIED", "true")
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, true, true}}}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-A", "alice@corp.com", "Alice", nil, false,
	); err != nil {
		t.Fatalf("a linked subject was refused: %v", err)
	}
}

// Two accounts, one address. Writing the address onto the linked account would
// need them merged, and only an operator can decide that.
func TestARepairedAddressThatIsTakenIsRefusedRatherThanFailing(t *testing.T) {
	tx := &scriptedTx{
		rows:       []scriptedRow{{values: []any{7, false, true, true}}},
		execErrors: map[string]error{"SET last_login": &pgconn.PgError{Code: "23505"}},
	}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, testBinding, "oidc:sub-A", "taken@corp.com", "Alice", nil, false,
	); !errors.Is(err, errIdentityConflict) {
		t.Fatalf("error is %v, want errIdentityConflict", err)
	}
}

/* ── the nonce ─────────────────────────────────────────────────────────── */

// The state cookie proves the callback belongs to a login this server started.
// It does not prove the id_token does. Without the nonce a captured or replayed
// id_token verifies and is accepted.
func TestTheNonceMustMatchTheLoginInThisBrowser(t *testing.T) {
	handler := &OIDCHandler{}

	for name, testCase := range map[string]struct {
		cookie     string
		tokenNonce string
		want       bool
	}{
		"the token echoes the nonce": {cookie: "n-1", tokenNonce: "n-1", want: true},
		"the token echoes another":   {cookie: "n-1", tokenNonce: "n-2", want: false},
		"the token echoes nothing":   {cookie: "n-1", tokenNonce: "", want: false},
		"the browser has no nonce":   {cookie: "", tokenNonce: "n-1", want: false},
		"neither side has a nonce":   {cookie: "", tokenNonce: "", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
			if testCase.cookie != "" {
				request.AddCookie(&http.Cookie{Name: oidcNonceCookie, Value: testCase.cookie})
			}
			if got := handler.consumeNonce(httptest.NewRecorder(), request, testCase.tokenNonce); got != testCase.want {
				t.Fatalf("consumeNonce = %v, want %v", got, testCase.want)
			}
		})
	}
}

// The nonce cookie is cleared whatever the outcome, so it cannot be replayed
// against a second callback.
func TestTheNonceCookieIsClearedOnEveryCallback(t *testing.T) {
	handler := &OIDCHandler{}
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	request.AddCookie(&http.Cookie{Name: oidcNonceCookie, Value: "n-1"})
	recorder := httptest.NewRecorder()

	handler.consumeNonce(recorder, request, "n-1")

	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == oidcNonceCookie {
			if cookie.MaxAge >= 0 {
				t.Fatalf("the nonce cookie survives the callback: MaxAge = %d", cookie.MaxAge)
			}
			return
		}
	}
	t.Fatal("the callback does not clear the nonce cookie")
}

/* ── recent authentication (ADR-0059 R1.10a) ───────────────────────────── */

// limitedLogin runs Login for a query and returns the recorder.
func limitedLogin(handler *OIDCHandler, query string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.Login(recorder, httptest.NewRequest(http.MethodGet, "/auth/oidc/login?"+query, nil))
	return recorder
}

// limitedCallback completes a login started with query, with the provider
// stating authTime (nil leaves auth_time out of the id_token).
func limitedCallback(t *testing.T, provider *fakeIdentityProvider, handler *OIDCHandler, query string, authTime *time.Time) *httptest.ResponseRecorder {
	t.Helper()
	login := limitedLogin(handler, query)
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302: %s", login.Code, login.Body.String())
	}
	authorizeURL, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := "code-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	provider.mu.Lock()
	provider.codes[code] = fakeAuthorization{
		subject: "subject-1", email: "person@example.test",
		nonce: authorizeURL.Query().Get("nonce"), authTime: authTime,
	}
	provider.mu.Unlock()
	callback := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code="+url.QueryEscape(code)+
		"&state="+url.QueryEscape(authorizeURL.Query().Get("state")), nil)
	for _, cookie := range login.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.Callback(recorder, callback)
	return recorder
}

func TestAnOIDCSignInStartedWithALimitSendsMaxAge(t *testing.T) {
	provider := newFakeIdentityProvider(t)
	handler := provider.handler(t, nil)

	login := limitedLogin(handler, "max_age=600&target_to=%2Fadmin%2Faccess")
	if login.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", login.Code, login.Body.String())
	}
	authorizeURL, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := authorizeURL.Query().Get("max_age"); got != "600" {
		t.Fatalf("max_age = %q, want %q", got, "600")
	}
	// The limit is bound to the SIGNED state, which the callback reads back.
	var stateCookie string
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == oidcStateCookie {
			stateCookie = cookie.Value
		}
	}
	state, ok := handler.consumeState(stateCookie, authorizeURL.Query().Get("state"))
	if !ok || !state.limited || state.limit != 10*time.Minute || state.targetTo != "/admin/access" {
		t.Fatalf("state = %+v (ok %v), want a 10m limit and target /admin/access", state, ok)
	}

	// An ordinary sign-in sends no max_age and binds no limit.
	ordinary := limitedLogin(handler, "target_to=%2F")
	ordinaryURL, err := url.Parse(ordinary.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryURL.Query().Has("max_age") {
		t.Fatalf("an ordinary sign-in sent max_age=%q", ordinaryURL.Query().Get("max_age"))
	}
}

func TestAnOIDCCallbackWithAnOldOrAbsentAuthTimeIsRefused(t *testing.T) {
	provider := newFakeIdentityProvider(t)
	// No pool: a refused callback must stop before any account is read or
	// any session written, so reaching provisioning would fail here.
	handler := provider.handler(t, nil)

	old := time.Now().Add(-20 * time.Minute)
	future := time.Now().Add(20 * time.Minute)
	for name, authTime := range map[string]*time.Time{
		"auth_time is absent":            nil,
		"auth_time is older than limit":  &old,
		"auth_time is in the far future": &future,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := limitedCallback(t, provider, handler, "max_age=600&target_to=%2Fadmin", authTime)
			// Refused back to the sign-in page with its error and target. Every
			// earlier refusal of a callback that carries a code is a plain
			// error status, so this redirect is the auth_time check's: the
			// token, its signature and its nonce all verified.
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != SignInErrorURL("/admin") {
				t.Fatalf("status = %d, Location = %q; want 302 to %q: %s", recorder.Code,
					recorder.Header().Get("Location"), SignInErrorURL("/admin"), recorder.Body.String())
			}
			cleared := map[string]bool{}
			for _, cookie := range recorder.Result().Cookies() {
				if cookie.Name == browsersession.CookieName && cookie.MaxAge >= 0 {
					t.Fatalf("a refused callback set a session cookie: %+v", cookie)
				}
				if cookie.MaxAge < 0 {
					cleared[cookie.Name] = true
				}
			}
			for _, name := range []string{oidcStateCookie, oidcNonceCookie, oidcPKCECookie} {
				if !cleared[name] {
					t.Fatalf("the refused callback left the %s cookie set", name)
				}
			}
		})
	}
}

func TestALimitedSignInRefusesAnAbsoluteOrCrossOriginReturnPath(t *testing.T) {
	provider := newFakeIdentityProvider(t)
	handler := provider.handler(t, nil)

	for _, target := range []string{
		"https://attacker.example/admin",
		"//attacker.example/admin",
		"/\\attacker.example/admin",
		"javascript:alert(1)",
		"admin/relative",
	} {
		t.Run(target, func(t *testing.T) {
			recorder := limitedLogin(handler, "max_age=600&target_to="+url.QueryEscape(target))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for target %q", recorder.Code, target)
			}
			if len(recorder.Result().Cookies()) != 0 {
				t.Fatal("a refused limited sign-in set login cookies")
			}
		})
	}
	if recorder := limitedLogin(handler, "max_age=600&target_to=%2Fadmin%2Fusers%3Ftab%3Droles"); recorder.Code != http.StatusFound {
		t.Fatalf("a same-origin relative path was refused: %d", recorder.Code)
	}
}

func TestTheLimitOfASignInMustBeAWholeNumberOfSecondsInRange(t *testing.T) {
	for raw, want := range map[string]bool{
		"0": true, "900": true, "86400": true,
		"": false, "-1": false, "86401": false, "15m": false, "9e2": false, "+900": false, "0900": false,
		"36028797018963968": false, "9223372036854775807": false, "9223372036854775808": false,
	} {
		_, limited, err := requestedAuthenticationLimit(url.Values{"max_age": {raw}})
		if (err == nil) != want || (err == nil && !limited) {
			t.Fatalf("max_age=%q: limited %v, err %v; want accepted %v", raw, limited, err, want)
		}
	}
	if _, limited, err := requestedAuthenticationLimit(url.Values{}); err != nil || limited {
		t.Fatalf("no max_age: limited %v, err %v; want an ordinary sign-in", limited, err)
	}
}

func TestTheAuthTimeDecidesTheSessionsAuthenticationTime(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	seconds := func(at time.Time) *float64 { value := float64(at.Unix()); return &value }
	limited := oidcLoginState{limit: 10 * time.Minute, limited: true}

	for name, testCase := range map[string]struct {
		authTime *float64
		login    oidcLoginState
		wantOK   bool
		wantAt   time.Time
	}{
		"ordinary, no auth_time: the time is unknown": {login: oidcLoginState{}, wantOK: true},
		"ordinary, an old auth_time is recorded as it is": {
			authTime: seconds(now.Add(-3 * time.Hour)), login: oidcLoginState{}, wantOK: true, wantAt: now.Add(-3 * time.Hour),
		},
		"limited, fresh auth_time": {
			authTime: seconds(now.Add(-time.Minute)), login: limited, wantOK: true, wantAt: now.Add(-time.Minute),
		},
		// Admitted by the skew alone: read as the provider's clock lagging by
		// the whole skew, so it is inside the limit and the check that asked
		// with the same limit passes it (ADR-0059 R1.10b review).
		"limited, within the limit and the clock skew": {
			authTime: seconds(now.Add(-14 * time.Minute)), login: limited, wantOK: true, wantAt: now.Add(-9 * time.Minute),
		},
		"limited, at the outer edge of the limit and the clock skew": {
			authTime: seconds(now.Add(-15 * time.Minute)), login: limited,
		},
		"limited, past the limit and the clock skew": {authTime: seconds(now.Add(-16 * time.Minute)), login: limited},
		"limited, absent": {login: limited},
		"limited, ahead by more than the clock skew": {authTime: seconds(now.Add(6 * time.Minute)), login: limited},
	} {
		t.Run(name, func(t *testing.T) {
			at, ok := checkAuthTime(testCase.authTime, testCase.login, now)
			if ok != testCase.wantOK || !at.Equal(testCase.wantAt) {
				t.Fatalf("checkAuthTime = (%s, %v), want (%s, %v)", at, ok, testCase.wantAt, testCase.wantOK)
			}
		})
	}
}

// The reserved break-glass account (ADR-0059 R2.6b) signs in through no
// single sign-on plane: an assertion naming its address is refused before any
// read or write, whatever the case; a subject linked to it reads as suspended.
func TestSingleSignOnRefusesTheReservedAccount(t *testing.T) {
	for _, email := range []string{"break-glass@centry.user", "BREAK-GLASS@centry.USER"} {
		tx := &scriptedTx{}
		if _, err := resolveProvisionedUser(
			context.Background(), tx, testBinding, "oidc:sub-R", email, "R", nil, false,
		); !errors.Is(err, errReservedAccount) {
			t.Fatalf("%s: error is %v, want errReservedAccount", email, err)
		}
		if len(tx.statements) != 0 {
			t.Fatalf("%s: the refusal ran %v", email, tx.statements)
		}
	}
	linked := &scriptedTx{rows: []scriptedRow{{values: []any{9, true, true, true}}}}
	if _, err := resolveProvisionedUser(
		context.Background(), linked, testBinding, "oidc:sub-L", "someone@corp.com", "S", nil, false,
	); !errors.Is(err, errUserSuspended) {
		t.Fatalf("a linked subject of a suspended or reserved account: %v", err)
	}
	if !strings.Contains(strings.Join(linked.statements, "\n"), "lower(owner.email) = 'break-glass@centry.user'") {
		t.Fatalf("the linked-subject read does not test the reserved address: %v", linked.statements)
	}
}
