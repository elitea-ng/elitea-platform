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
	"testing"
	"time"

	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
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
