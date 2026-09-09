package eliteacore

import (
	"strings"
	"testing"
	"time"
)

// The approval token used to be checked for its SHAPE alone, so any string of
// sixteen lowercase hexadecimal characters skipped the pre-publish quality gate
// on any version (issue 855). These tests pin the four things that make it a
// grant instead: it verifies for the version it was issued for, it does not
// verify for another one, it cannot be produced without the signing secret, and
// it stops working when it is old.

const testContentHash = "1111111111111111111111111111111111111111111111111111111111111111"

func TestAnIssuedTokenVerifiesForItsOwnVersion(t *testing.T) {
	now := time.Now()
	token := issuePublishValidationToken("42", testContentHash, now.Add(publishValidationTokenTTL))

	if len(token) != publishValidationTokenLen || !isLowerHex(token) {
		t.Fatalf("token = %q, want %d lowercase hexadecimal characters", token, publishValidationTokenLen)
	}
	if verdict := verifyPublishValidationToken(token, "42", testContentHash, now); verdict != publishTokenValid {
		t.Fatalf("verdict = %d (%s), want valid", verdict, publishTokenRefusal(verdict))
	}
}

func TestATokenIssuedForAnotherVersionIsRefused(t *testing.T) {
	now := time.Now()
	token := issuePublishValidationToken("42", testContentHash, now.Add(publishValidationTokenTTL))

	verdict := verifyPublishValidationToken(token, "43", testContentHash, now)
	if verdict != publishTokenForged {
		t.Fatalf("verdict = %d, want forged", verdict)
	}
	if refusal := publishTokenRefusal(verdict); !strings.Contains(refusal, "not issued for this version") {
		t.Errorf("refusal = %q, want it to name the version", refusal)
	}
}

// TestAWellFormedTokenNobodyIssuedIsRefused is the case the old shape check
// could not answer: a string that LOOKS exactly like a token, because it was
// built to.
func TestAWellFormedTokenNobodyIssuedIsRefused(t *testing.T) {
	forged := strings.Repeat("ab", publishValidationTokenLen/2)
	if len(forged) != publishValidationTokenLen || !isLowerHex(forged) {
		t.Fatalf("the forged token is not well formed: %q", forged)
	}
	if verdict := verifyPublishValidationToken(forged, "42", testContentHash, time.Now()); verdict != publishTokenForged {
		t.Fatalf("verdict = %d, want forged", verdict)
	}
}

func TestATokenOfTheWrongShapeIsRefusedAsMalformed(t *testing.T) {
	for name, token := range map[string]string{
		"the old sixteen-character shape": "0123456789abcdef",
		"another platform's claim":        "X:42:0123456789abcdef:1700000000",
		"upper case":                      strings.ToUpper(issuePublishValidationToken("42", testContentHash, time.Now().Add(time.Hour))),
		"empty":                           "",
	} {
		if verdict := verifyPublishValidationToken(token, "42", testContentHash, time.Now()); verdict != publishTokenMalformed {
			t.Errorf("%s: verdict = %d, want malformed", name, verdict)
		}
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	issued := time.Now().Add(-2 * publishValidationTokenTTL)
	token := issuePublishValidationToken("42", testContentHash, issued.Add(publishValidationTokenTTL))

	verdict := verifyPublishValidationToken(token, "42", testContentHash, time.Now())
	if verdict != publishTokenExpired {
		t.Fatalf("verdict = %d, want expired", verdict)
	}
	// …and the same token verified while it was still alive, so what this
	// measures is the expiry and not a broken signature.
	if verdict := verifyPublishValidationToken(token, "42", testContentHash, issued); verdict != publishTokenValid {
		t.Fatalf("verdict inside the window = %d, want valid", verdict)
	}
}

// TestATokenIsRefusedWhenTheVersionChanged pins the reason the token carries a
// fingerprint at all: a check that passed describes the agent as it was, and an
// agent edited afterwards has not been checked.
func TestATokenIsRefusedWhenTheVersionChanged(t *testing.T) {
	now := time.Now()
	token := issuePublishValidationToken("42", testContentHash, now.Add(publishValidationTokenTTL))
	changed := strings.Repeat("2", 64)

	if verdict := verifyPublishValidationToken(token, "42", changed, now); verdict != publishTokenStale {
		t.Fatalf("verdict = %d, want stale", verdict)
	}
	// A version that cannot be read at all fingerprints as the empty string,
	// which no token matches.
	if verdict := verifyPublishValidationToken(token, "42", "", now); verdict != publishTokenStale {
		t.Fatalf("verdict for an unreadable version = %d, want stale", verdict)
	}
}

func TestTheWithdrawnNameFitsTheColumn(t *testing.T) {
	long := strings.Repeat("n", versionNameMaxLen)
	name := withdrawnVersionName(long, 7)
	if len([]rune(name)) != versionNameMaxLen {
		t.Fatalf("name length = %d, want %d", len([]rune(name)), versionNameMaxLen)
	}
	if !strings.HasSuffix(name, withdrawnNameMarker+"7") {
		t.Errorf("name = %q, want it to end with the counter", name)
	}
	if short := withdrawnVersionName("v-one", 1); short != "v-one-withdrawn-1" {
		t.Errorf("name = %q, want v-one-withdrawn-1", short)
	}
}
