package auth

// What these tests hold in place.
//
// `gosaml2.RetrieveAssertionInfo` returns `err == nil` for an assertion that is
// EXPIRED and for one addressed to a DIFFERENT AUDIENCE. Both verdicts are on
// `AssertionInfo.WarningInfo`, and a caller that reads only the error accepts
// both. Two of the tests below are that fact, written down: they construct
// exactly the value the library would hand back and assert this package refuses
// it.
//
// The rest cover the three checks the library performs none of — InResponseTo,
// Recipient and the authored clock skew — and the attribute resolution.
//
// None of this needs a database, a network or a signature: `assertionAcceptable`
// takes the library's already-verified output, which is the seam worth testing.
// Signature verification itself is the library's, and reproducing it in a test
// here would be testing the library.

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
)

const (
	testACSURL    = "https://elitea.example.com/auth/saml/acs"
	testRequestID = "_a1b2c3d4"
)

func testRuntime(skewSeconds int) *samlRuntime {
	return &samlRuntime{
		document: identityproviders.SAMLDocument{
			ACSURL:     testACSURL,
			SPEntityID: "https://elitea.example.com/saml",
		},
		clockSkewSeconds: skewSeconds,
		origin:           "test",
	}
}

// validAssertion is what the library returns for an assertion that verified and
// is inside every condition.
func validAssertion() *saml2.AssertionInfo {
	now := time.Now()
	return &saml2.AssertionInfo{
		NameID:      "alice@corp.com",
		WarningInfo: &saml2.WarningInfo{},
		Assertions: []types.Assertion{{
			Conditions: &types.Conditions{
				NotBefore:    now.Add(-time.Minute).Format(time.RFC3339),
				NotOnOrAfter: now.Add(time.Minute).Format(time.RFC3339),
			},
			Subject: &types.Subject{
				SubjectConfirmation: &types.SubjectConfirmation{
					SubjectConfirmationData: &types.SubjectConfirmationData{
						InResponseTo: testRequestID,
						Recipient:    testACSURL,
					},
				},
			},
		}},
	}
}

func TestAVerifiedAssertionInsideItsConditionsIsAccepted(t *testing.T) {
	reason, ok := assertionAcceptable(validAssertion(), testRuntime(0), testRequestID)
	if !ok {
		t.Fatalf("a valid assertion was refused: %s", reason)
	}
}

/* ── the two verdicts the library reports as WARNINGS ───────────────────── */

// An assertion minted for a DIFFERENT service provider verifies: its signature
// is real, and the identity provider is one this deployment trusts. Only the
// audience says it was not meant for us, and the library says so in a warning
// with a nil error.
func TestAnAssertionForAnotherAudienceIsRefused(t *testing.T) {
	assertion := validAssertion()
	assertion.WarningInfo.NotInAudience = true

	if _, ok := assertionAcceptable(assertion, testRuntime(0), testRequestID); ok {
		t.Fatal("an assertion addressed to another audience was accepted")
	}
}

// The same shape for expiry. With no authored skew there is nothing to forgive,
// so the warning is decisive.
func TestAnExpiredAssertionIsRefusedWhenNoSkewIsAuthored(t *testing.T) {
	assertion := validAssertion()
	assertion.WarningInfo.InvalidTime = true

	if _, ok := assertionAcceptable(assertion, testRuntime(0), testRequestID); ok {
		t.Fatal("an assertion outside its validity window was accepted")
	}
}

// A nil WarningInfo is not "no warnings". It is a shape this code does not
// understand, and the structure it would be reading carries the audience and
// expiry verdicts — so the safe reading is to refuse.
func TestAnAssertionWithNoVerificationResultIsRefused(t *testing.T) {
	assertion := validAssertion()
	assertion.WarningInfo = nil

	if _, ok := assertionAcceptable(assertion, testRuntime(0), testRequestID); ok {
		t.Fatal("an assertion with no verification result was accepted")
	}
}

/* ── the authored clock skew is applied, and bounded ────────────────────── */

// An assertion that expired thirty seconds ago is accepted under a sixty-second
// authored tolerance. Without this the skew field would be collected, stored,
// and never consulted — the defect this whole surface exists to remove.
func TestAnAuthoredSkewForgivesADriftedClock(t *testing.T) {
	now := time.Now()
	assertion := validAssertion()
	assertion.WarningInfo.InvalidTime = true
	assertion.Assertions[0].Conditions.NotBefore = now.Add(-5 * time.Minute).Format(time.RFC3339)
	assertion.Assertions[0].Conditions.NotOnOrAfter = now.Add(-30 * time.Second).Format(time.RFC3339)

	if _, ok := assertionAcceptable(assertion, testRuntime(60), testRequestID); !ok {
		t.Fatal("a 30-second drift was refused under a 60-second authored tolerance")
	}
}

// And a drift beyond the tolerance is still refused, so the skew widens the
// window rather than removing it.
func TestASkewDoesNotRemoveTheValidityWindow(t *testing.T) {
	now := time.Now()
	assertion := validAssertion()
	assertion.WarningInfo.InvalidTime = true
	assertion.Assertions[0].Conditions.NotBefore = now.Add(-time.Hour).Format(time.RFC3339)
	assertion.Assertions[0].Conditions.NotOnOrAfter = now.Add(-30 * time.Minute).Format(time.RFC3339)

	if _, ok := assertionAcceptable(assertion, testRuntime(60), testRequestID); ok {
		t.Fatal("an assertion half an hour past its window was accepted under a 60-second tolerance")
	}
}

/* ── the three checks the library performs none of ──────────────────────── */

// Without this check, an assertion captured from ANY login at this identity
// provider can be posted to this endpoint and becomes a session.
func TestAnAssertionAnsweringAnotherRequestIsRefused(t *testing.T) {
	if _, ok := assertionAcceptable(validAssertion(), testRuntime(0), "_a-different-request"); ok {
		t.Fatal("an assertion answering a different authentication request was accepted")
	}
}

// The Recipient names the endpoint the assertion was minted for. The library
// checks the response-level Destination, which an identity provider may omit.
func TestAnAssertionMintedForAnotherEndpointIsRefused(t *testing.T) {
	assertion := validAssertion()
	assertion.Assertions[0].Subject.SubjectConfirmation.SubjectConfirmationData.Recipient =
		"https://attacker.example.com/acs"

	if _, ok := assertionAcceptable(assertion, testRuntime(0), testRequestID); ok {
		t.Fatal("an assertion minted for another endpoint was accepted")
	}
}

// An assertion with no subject confirmation data carries no InResponseTo, so
// the binding to this login cannot be checked at all. It is refused rather than
// treated as unbound-but-fine.
func TestAnAssertionWithNoSubjectConfirmationIsRefused(t *testing.T) {
	assertion := validAssertion()
	assertion.Assertions[0].Subject.SubjectConfirmation = nil

	if _, ok := assertionAcceptable(assertion, testRuntime(0), testRequestID); ok {
		t.Fatal("an assertion with no subject confirmation was accepted")
	}
}

/* ── identity resolution ────────────────────────────────────────────────── */

func attributeAssertion(name, value, nameID string) *saml2.AssertionInfo {
	assertion := validAssertion()
	assertion.NameID = nameID
	assertion.Values = saml2.Values{
		name: types.Attribute{Values: []types.AttributeValue{{Value: value}}},
	}
	return assertion
}

func TestTheAddressIsReadFromACommonAttributeName(t *testing.T) {
	assertion := attributeAssertion(
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		"alice@corp.com", "opaque-subject")

	email, _ := samlIdentity(assertion, identityproviders.SAMLDocument{})
	if email != "alice@corp.com" {
		t.Fatalf("email is %q, want the attribute value", email)
	}
}

// An operator who names an attribute has told this service where to look.
// Falling back to a different one when theirs is absent would make a
// misconfiguration look like a working login against the wrong field.
func TestAnAuthoredAttributeNameIsUsedAlone(t *testing.T) {
	assertion := attributeAssertion("mail", "alice@corp.com", "opaque-subject")

	email, _ := samlIdentity(assertion, identityproviders.SAMLDocument{EmailAttribute: "corpMail"})
	if email != "" {
		t.Fatalf("email is %q, want empty: the authored attribute is absent", email)
	}
}

// A persistent or transient NameID is an OPAQUE identifier. Treating one as an
// address would create an account whose email is a random string, and the next
// login — with a rotated identifier — would match nothing.
func TestAnOpaqueNameIDIsNotUsedAsAnAddress(t *testing.T) {
	assertion := validAssertion()
	assertion.NameID = "AAdzZWNyZXQxNzgyOTM4NA"
	assertion.Values = saml2.Values{}

	email, _ := samlIdentity(assertion, identityproviders.SAMLDocument{})
	if email != "" {
		t.Fatalf("email is %q, want empty for an opaque NameID", email)
	}
}

// A NameID that IS an address is used, which is what an emailAddress-format
// provider sends and is the common case for a deployment that maps no
// attributes at all.
func TestAnEmailNameIDIsUsedWhenNoAttributeCarriesOne(t *testing.T) {
	assertion := validAssertion()
	assertion.NameID = "alice@corp.com"
	assertion.Values = saml2.Values{}

	email, _ := samlIdentity(assertion, identityproviders.SAMLDocument{})
	if email != "alice@corp.com" {
		t.Fatalf("email is %q, want the NameID", email)
	}
}

/* ── the signed browser cookie ──────────────────────────────────────────── */

// The browser holds the value and can read it; the MAC is what stops it being
// EDITED. A target the browser could rewrite would be an open redirect through
// the login.
func TestASignedBrowserValueRoundTripsAndRejectsAnEdit(t *testing.T) {
	const secret = "test-secret"
	signed := signBrowserValue(secret, testRequestID+"|/projects")

	value, ok := verifyBrowserValue(secret, signed)
	if !ok || value != testRequestID+"|/projects" {
		t.Fatalf("round trip returned (%q, %v)", value, ok)
	}

	// An edited target must not verify.
	edited := signBrowserValue(secret, testRequestID+"|/projects")
	edited = "_x|https://attacker.example.com" + edited[len(testRequestID+"|/projects"):]
	if _, ok := verifyBrowserValue(secret, edited); ok {
		t.Fatal("an edited cookie verified")
	}

	// And a value signed with another key must not verify either.
	if _, ok := verifyBrowserValue("another-secret", signed); ok {
		t.Fatal("a cookie signed with a different key verified")
	}
}

// The split is on the LAST separator. Splitting on the first would let a value
// carrying a dot move the boundary and present part of itself as the signature.
func TestAValueContainingADotStillVerifies(t *testing.T) {
	const secret = "test-secret"
	signed := signBrowserValue(secret, "_id|/a.b.c")

	value, ok := verifyBrowserValue(secret, signed)
	if !ok || value != "_id|/a.b.c" {
		t.Fatalf("round trip returned (%q, %v)", value, ok)
	}
}

/* ── ForceAuthn and the authentication time (ADR-0059 R1.10b) ───────────── */

// samlRequestDocument inflates the SAMLRequest of a Login redirect.
func samlRequestDocument(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	if recorder.Code != http.StatusFound {
		t.Fatalf("login status = %d: %s", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	deflated, err := base64.StdEncoding.DecodeString(location.Query().Get("SAMLRequest"))
	if err != nil {
		t.Fatalf("SAMLRequest is not base64: %v", err)
	}
	inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(deflated)))
	if err != nil {
		t.Fatalf("SAMLRequest does not inflate: %v", err)
	}
	return string(inflated)
}

// samlCookieHead reads what the signed request cookie carries, with the
// ACS's own reader.
func samlCookieHead(t *testing.T, handler *SAMLHandler, cookie *http.Cookie) samlLoginRequest {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", nil)
	request.AddCookie(cookie)
	login, ok := handler.consumeRequestCookie(httptest.NewRecorder(), request)
	if !ok {
		t.Fatal("the request cookie did not verify")
	}
	return login
}

func TestASAMLSignInStartedWithALimitSendsForceAuthn(t *testing.T) {
	handler := samlCookieHandler(t, true)
	recorder := httptest.NewRecorder()
	handler.Login(recorder, httptest.NewRequest(http.MethodGet, "/auth/saml/login?max_age=900&target_to=%2Fadmin", nil))

	if document := samlRequestDocument(t, recorder); !strings.Contains(document, `ForceAuthn="true"`) {
		t.Fatalf("a limited sign-in's AuthnRequest carries no ForceAuthn: %s", document)
	}
	login := samlCookieHead(t, handler, samlRequestSetCookie(t, recorder))
	if !login.limited || login.limit != 15*time.Minute || login.targetTo != "/admin" || login.requestID == "" {
		t.Fatalf("the signed cookie = %+v, want the 900-second limit and the target", login)
	}
}

func TestAnOrdinarySAMLSignInSendsNoForceAuthn(t *testing.T) {
	handler := samlCookieHandler(t, true)
	recorder := httptest.NewRecorder()
	handler.Login(recorder, httptest.NewRequest(http.MethodGet, "/auth/saml/login?target_to=%2Fapp", nil))

	if document := samlRequestDocument(t, recorder); strings.Contains(document, "ForceAuthn") {
		t.Fatalf("an ordinary sign-in asked for ForceAuthn: %s", document)
	}
	if login := samlCookieHead(t, handler, samlRequestSetCookie(t, recorder)); login.limited {
		t.Fatalf("an ordinary sign-in's cookie carries a limit: %+v", login)
	}
}

func TestALimitedSAMLSignInRefusesAMalformedLimitOrAForeignTarget(t *testing.T) {
	handler := samlCookieHandler(t, true)
	for _, query := range []string{
		"max_age=abc", "max_age=-1", "max_age=86401", "max_age=900&target_to=https%3A%2F%2Fevil.example",
	} {
		recorder := httptest.NewRecorder()
		handler.Login(recorder, httptest.NewRequest(http.MethodGet, "/auth/saml/login?"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", query, recorder.Code)
		}
	}
}

func TestCheckAuthnInstant(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time { value := now.Add(offset); return &value }
	limited := samlLoginRequest{limit: 15 * time.Minute, limited: true}
	for name, testCase := range map[string]struct {
		instant    *time.Time
		request    samlLoginRequest
		skew       time.Duration
		wantAt     time.Time
		wantProven bool
		wantOK     bool
	}{
		"an ordinary sign-in records no time, whatever it states": {
			instant: at(-time.Minute), wantOK: true,
		},
		"an ordinary sign-in with no AuthnInstant completes": {
			wantOK: true,
		},
		"a ForceAuthn sign-in within the limit is proven": {
			instant: at(-2 * time.Minute), request: limited,
			wantAt: now.Add(-2 * time.Minute), wantProven: true, wantOK: true,
		},
		"a ForceAuthn assertion with no AuthnInstant is refused": {
			request: limited,
		},
		"an AuthnInstant older than the limit is refused": {
			instant: at(-16 * time.Minute), request: limited,
		},
		"the authored skew admits a drift, recorded at the edge": {
			instant: at(-16 * time.Minute), request: limited, skew: 2 * time.Minute,
			wantAt: now.Add(-15 * time.Minute), wantProven: true, wantOK: true,
		},
		"an AuthnInstant in the future beyond the skew is refused": {
			instant: at(3 * time.Minute), request: limited, skew: time.Minute,
		},
	} {
		t.Run(name, func(t *testing.T) {
			gotAt, proven, ok := checkAuthnInstant(testCase.instant, testCase.request, testCase.skew, now)
			if ok != testCase.wantOK || proven != testCase.wantProven || !gotAt.Equal(testCase.wantAt) {
				t.Fatalf("checkAuthnInstant = (%s, %v, %v), want (%s, %v, %v)",
					gotAt, proven, ok, testCase.wantAt, testCase.wantProven, testCase.wantOK)
			}
		})
	}
}

// acsRig drives the real Login and ACS around a stand-in for the two steps
// that need a signed response and a database.
type acsRig struct {
	handler  *SAMLHandler
	sessions *memorySessions
	instant  *time.Time
	// responseFor overrides the InResponseTo of the next assertion; empty
	// answers the request the cookie names.
	responseFor string
}

func newACSRig(t *testing.T) *acsRig {
	t.Helper()
	rig := &acsRig{sessions: newMemorySessions()}
	manager, err := browsersession.NewManager(rig.sessions, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.handler = samlCookieHandler(t, true).WithSessionManager(manager)
	rig.handler.provision = func(context.Context, *samlRuntime, string, string, string) (string, error) {
		return "7", nil
	}
	return rig
}

// signIn runs Login with query, then posts a verified assertion to the ACS.
func (rig *acsRig) signIn(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	rig.handler.Login(login, httptest.NewRequest(http.MethodGet, "/auth/saml/login?"+query, nil))
	cookie := samlRequestSetCookie(t, login)
	requestID := samlCookieHead(t, rig.handler, cookie).requestID
	if rig.responseFor != "" {
		requestID = rig.responseFor
	}
	instant := rig.instant
	rig.handler.retrieveAssertion = func(*samlRuntime, string) (*saml2.AssertionInfo, error) {
		assertion := validAssertion()
		data := assertion.Assertions[0].Subject.SubjectConfirmation.SubjectConfirmationData
		data.InResponseTo = requestID
		data.Recipient = "https://elitea.example.com/auth/saml/acs"
		assertion.AuthnInstant = instant
		return assertion, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/saml/acs",
		strings.NewReader(url.Values{"SAMLResponse": {"stand-in"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	rig.handler.ACS(recorder, request)
	return recorder
}

func (rig *acsRig) rows() []browsersession.Session {
	rig.sessions.mu.Lock()
	defer rig.sessions.mu.Unlock()
	rows := make([]browsersession.Session, 0, len(rig.sessions.rows))
	for _, row := range rig.sessions.rows {
		rows = append(rows, row)
	}
	return rows
}

func TestAForceAuthnSAMLSignInRecordsAProvenAuthenticationTime(t *testing.T) {
	rig := newACSRig(t)
	instant := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	rig.instant = &instant

	recorder := rig.signIn(t, "max_age=900&target_to=%2Fadmin")
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/admin" {
		t.Fatalf("ACS: %d %q %s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	rows := rig.rows()
	if len(rows) != 1 || !rows[0].AuthenticationProven || !rows[0].AuthenticatedAt.Equal(instant) ||
		rows[0].Provider != browsersession.ProviderSAML {
		t.Fatalf("session rows = %+v, want one proven SAML row at %s", rows, instant)
	}
}

// The acceptance criterion: an assertion whose AuthnInstant is older than the
// limit is refused, before any account or session is touched.
func TestAForceAuthnSAMLSignInWithAStaleAuthnInstantIsRefused(t *testing.T) {
	rig := newACSRig(t)
	provisioned := false
	rig.handler.provision = func(context.Context, *samlRuntime, string, string, string) (string, error) {
		provisioned = true
		return "7", nil
	}
	instant := time.Now().Add(-2 * time.Hour)
	rig.instant = &instant

	recorder := rig.signIn(t, "max_age=900&target_to=%2Fadmin")
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != SignInErrorURL("/admin") {
		t.Fatalf("ACS: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if provisioned || len(rig.rows()) != 0 {
		t.Fatalf("a refused sign-in provisioned (%v) or wrote sessions (%d)", provisioned, len(rig.rows()))
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == browsersession.CookieName {
			t.Fatalf("a refused sign-in set the session cookie: %+v", cookie)
		}
	}
}

// The acceptance criterion: a SAML response that was not requested with
// ForceAuthn does not move the authentication time of an existing session.
// The existing row is untouched, and the new row records no time at all, so
// it never satisfies the recent-authentication check however recent the
// AuthnInstant the identity provider states.
func TestASAMLResponseNotRequestedWithForceAuthnMovesNoAuthenticationTime(t *testing.T) {
	rig := newACSRig(t)
	instant := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	rig.instant = &instant
	if recorder := rig.signIn(t, "max_age=900&target_to=%2Fadmin"); recorder.Code != http.StatusFound {
		t.Fatalf("the ForceAuthn sign-in: %d", recorder.Code)
	}
	existing := rig.rows()[0]
	// The existing session is aged, as time would.
	existing.AuthenticatedAt = existing.AuthenticatedAt.Add(-time.Hour)
	rig.sessions.rows[existing.ID] = existing

	fresh := time.Now().UTC().Truncate(time.Second)
	rig.instant = &fresh
	if recorder := rig.signIn(t, "target_to=%2Fapp"); recorder.Code != http.StatusFound {
		t.Fatalf("the ordinary sign-in: %d %s", recorder.Code, recorder.Body.String())
	}

	rows := rig.rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the existing one and the new one", len(rows))
	}
	for _, row := range rows {
		if row.ID == existing.ID {
			if !row.AuthenticatedAt.Equal(existing.AuthenticatedAt) || !row.AuthenticationProven {
				t.Fatalf("the existing session changed: %+v, was %+v", row, existing)
			}
			continue
		}
		if row.AuthenticationProven || !row.AuthenticatedAt.IsZero() {
			t.Fatalf("the ordinary sign-in recorded an authentication time: %+v", row)
		}
	}
}

// An unsolicited or replayed response (one answering another request) never
// reaches the session store: the InResponseTo check refuses it.
func TestAnUnsolicitedSAMLResponseWritesNoSession(t *testing.T) {
	rig := newACSRig(t)
	instant := time.Now().UTC()
	rig.instant = &instant
	rig.responseFor = "_another-request"
	if recorder := rig.signIn(t, "max_age=900&target_to=%2Fadmin"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("an unsolicited response: %d, want 401", recorder.Code)
	}
	if len(rig.rows()) != 0 {
		t.Fatalf("an unsolicited response wrote %d sessions", len(rig.rows()))
	}
}
