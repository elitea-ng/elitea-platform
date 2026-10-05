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
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false}}}}

	userID, err := resolveProvisionedUser(context.Background(), tx, "oidc:sub-A", "alice@corp.com", "Alice", nil, false)
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
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false}}}}

	userID, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-A", "alice.smith@corp.com", "Alice", nil, false,
	)
	if err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}
	if userID != 7 {
		t.Fatalf("resolved user %d, want 7; a changed address must not create a second account", userID)
	}
	if statementIndex(t, tx, "SET last_login = now(), email =") < 0 {
		t.Fatalf("the account's address was not repaired: %v", tx.statements)
	}
}

// A suspended account still refuses the login, and it refuses it on the path
// that now resolves the account.
func TestALinkedSubjectOnASuspendedAccountIsRefused(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, true}}}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-A", "alice@corp.com", "Alice", nil, false,
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
		{err: pgx.ErrNoRows},         // no link for this subject
		{err: pgx.ErrNoRows},         // the email upsert refuses the bound row
		{values: []any{false, true}}, // not suspended, already bound
	}}

	_, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-B", "alice@corp.com", "Mallory", nil, false,
	)
	if !errors.Is(err, errIdentityConflict) {
		t.Fatalf("error is %v, want errIdentityConflict", err)
	}
	if index := statementIndex(t, tx, "auth_core__user_provider (user_id, provider_ref)"); index >= 0 {
		t.Fatalf("a link was written for a refused login: %v", tx.statements)
	}
}

// A pylon-created database holds BARE provider refs. legacy auth_init writes
// `provider_attr["nameid"]`, which is the preferred_username or the raw subject
// (legacy/plugins/auth_init/rpc/processor.py:55). No row there carries an
// `oidc:` prefix.
//
// A namespace-blind bound-account guard therefore refuses every user who ever
// logged in on pylon. The staging dump shows 61 such rows against 133 accounts.
// The guard must read this handler's own namespace only.
func TestTheBoundAccountGuardReadsOnlyThisHandlersNamespace(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows}, // no link for this subject
		{values: []any{11}},  // the upsert adopted the legacy account
		{values: []any{11}},  // the link names that row
	}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-E", "legacy@corp.com", "Legacy", nil, false,
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
		{values: []any{true}}, // suspended
	}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-B", "alice@corp.com", "Alice", nil, false,
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
		context.Background(), tx, "oidc:sub-C", "carol@corp.com", "Carol", nil, false,
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
		context.Background(), tx, "oidc:sub-A", "carol@corp.com", "Carol", nil, false,
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
		context.Background(), tx, "oidc:sub-D", "dave@corp.com", "Dave", nil, true,
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
		context.Background(), tx, "oidc:sub-D", "dave@corp.com", "Dave", &verified, true,
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
		context.Background(), tx, "oidc:sub-E", "erin@corp.com", "Erin", nil, false,
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
		context.Background(), tx, "oidc:sub-F", "fay@corp.com", "Fay", nil, false, true,
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
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false}}}}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-A", "alice@corp.com", "Alice", nil, false,
	); err != nil {
		t.Fatalf("a linked subject was refused: %v", err)
	}
}

// Two accounts, one address. Writing the address onto the linked account would
// need them merged, and only an operator can decide that.
func TestARepairedAddressThatIsTakenIsRefusedRatherThanFailing(t *testing.T) {
	tx := &scriptedTx{
		rows:       []scriptedRow{{values: []any{7, false}}},
		execErrors: map[string]error{"SET last_login": &pgconn.PgError{Code: "23505"}},
	}

	if _, err := resolveProvisionedUser(
		context.Background(), tx, "oidc:sub-A", "taken@corp.com", "Alice", nil, false,
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
