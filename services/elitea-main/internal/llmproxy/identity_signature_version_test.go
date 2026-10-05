package llmproxy

// The SIGNING half of the v1/v2 compatibility pair.
//
// The canonical string this file produces is verified by a DIFFERENT GO MODULE
// (services/elitea-llm-gateway/internal/llmproxy). The two cannot import each
// other, so the only way to hold them to one wire format is for both to assert
// the same frozen literals. The three constants below are byte-for-byte the
// ones in that module's identity_signature_version_test.go; if either side
// changes them alone, its own suite fails rather than production.
//
// Why it matters more than usual here: adding a field to the signed tuple in
// place would fail EVERY gateway request in both directions for the length of a
// rolling deploy. So v1 keeps signing exactly what it always signed, v2 is used
// only when there is an execution id to cover, and the gateway accepts both.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

const (
	frozenSecret = "shared-identity-secret"

	// sha256_hmac("v1\n42\n7\ntenant-a") under frozenSecret.
	frozenV1Digest = "sha256=6cca2fb17dc0846a9b1054cc48671c39ac26c82974e4519477b05f05addfb81c"
	// sha256_hmac("v2\n42\n7\ntenant-a\nexec-9") under frozenSecret.
	frozenV2Digest = "sha256=ef572fc69c2bd8190b14b04dcfb1b78cf40bdf01f7d044703cba3d7bd5fd7dd9"
)

func frozenHMAC(t *testing.T, message string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(frozenSecret))
	mac.Write([]byte(message))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestFrozenDigestsMatchTheCanonicalStrings anchors the literals to the strings
// they claim to be, independently of the signing helper.
func TestFrozenDigestsMatchTheCanonicalStrings(t *testing.T) {
	if got := frozenHMAC(t, "v1\n42\n7\ntenant-a"); got != frozenV1Digest {
		t.Fatalf("frozen v1 digest is stale: computed %s, literal %s", got, frozenV1Digest)
	}
	if got := frozenHMAC(t, "v2\n42\n7\ntenant-a\nexec-9"); got != frozenV2Digest {
		t.Fatalf("frozen v2 digest is stale: computed %s, literal %s", got, frozenV2Digest)
	}
}

// TestSignV1IsUnchanged: an identity with no execution id must produce exactly
// the bytes it produced before the v2 scheme existed. A gateway that has not
// been redeployed accepts nothing else.
func TestSignV1IsUnchanged(t *testing.T) {
	id := identity{projectID: "42", userID: "7", tenantID: "tenant-a"}
	if got, want := id.canonical(signatureVersionV1), "v1\n42\n7\ntenant-a"; got != want {
		t.Fatalf("v1 canonical = %q, want %q", got, want)
	}
	if got := id.sign([]byte(frozenSecret)); got != frozenV1Digest {
		t.Fatalf("v1 signature = %s, want %s (a gateway on the old scheme will reject anything else)", got, frozenV1Digest)
	}
}

// TestSignV2CoversTheExecutionID: with an execution id present the edge signs
// v2, and the gateway's own frozen v2 digest is what it must produce.
func TestSignV2CoversTheExecutionID(t *testing.T) {
	id := identity{projectID: "42", userID: "7", tenantID: "tenant-a", executionID: "exec-9"}
	if got, want := id.canonical(signatureVersionV2), "v2\n42\n7\ntenant-a\nexec-9"; got != want {
		t.Fatalf("v2 canonical = %q, want %q", got, want)
	}
	if got := id.sign([]byte(frozenSecret)); got != frozenV2Digest {
		t.Fatalf("v2 signature = %s, want %s", got, frozenV2Digest)
	}
}

// TestVerifyIdentitySignature_AcceptsBothSchemes mirrors the gateway's
// acceptance rule on this side, so a divergence shows up here too.
func TestVerifyIdentitySignature_AcceptsBothSchemes(t *testing.T) {
	v1 := http.Header{}
	v1.Set(HeaderProjectID, "42")
	v1.Set(HeaderUserID, "7")
	v1.Set(HeaderTenantID, "tenant-a")
	v1.Set(HeaderSignature, frozenV1Digest)
	if !verifyIdentitySignature(v1, []byte(frozenSecret)) {
		t.Fatal("a v1-signed tuple must still verify")
	}

	v2 := http.Header{}
	v2.Set(HeaderProjectID, "42")
	v2.Set(HeaderUserID, "7")
	v2.Set(HeaderTenantID, "tenant-a")
	v2.Set(HeaderExecutionID, "exec-9")
	v2.Set(HeaderSignature, frozenV2Digest)
	if !verifyIdentitySignature(v2, []byte(frozenSecret)) {
		t.Fatal("a v2-signed tuple must verify")
	}
}

// TestVerifyIdentitySignature_RefusesAV1SignatureBesideAnExecutionID: the v1
// string does not cover the id, so accepting the pair would make the id
// caller-attachable — the one thing signing it prevents.
func TestVerifyIdentitySignature_RefusesAV1SignatureBesideAnExecutionID(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderProjectID, "42")
	h.Set(HeaderUserID, "7")
	h.Set(HeaderTenantID, "tenant-a")
	h.Set(HeaderSignature, frozenV1Digest)
	h.Set(HeaderExecutionID, "exec-forged")
	if verifyIdentitySignature(h, []byte(frozenSecret)) {
		t.Fatal("a v1 signature must not authenticate an execution id it does not cover")
	}
}

// TestExecutionIDFromHeader_DropsWhatItCannotVouchFor.
//
// The execution id is the ONLY part of the signed tuple a caller supplies, so
// it is the only one that has to be validated. An invalid value is dropped, not
// refused: it decorates analytics and decides nothing, so a malformed header
// must cost a row in a breakdown and not the request.
func TestExecutionIDFromHeader_DropsWhatItCannotVouchFor(t *testing.T) {
	for name, value := range map[string]string{
		"a header split":        "exec-9\r\nX-Injected: 1",
		"a space":               "exec 9",
		"a slash":               "exec/9",
		"a percent escape":      "exec%0d9",
		"over the length bound": strings.Repeat("e", maxExecutionIDLen+1),
		// A `:` is reserved for the attributions elitea-main signs in
		// process. An inbound one names an evaluation run or a derived id.
		"an evaluation attribution": "eval:12:judge:case-1",
		"a derived attribution":     "exec-9:child",
		"any colon":                 "01JQ_A.B:C",
	} {
		h := http.Header{}
		h.Set(HeaderExecutionID, value)
		if got := executionIDFromHeader(h); got != "" {
			t.Errorf("%s: executionIDFromHeader = %q, want the value dropped", name, got)
		}
	}

	for _, value := range []string{"exec-9", "01JQ_A.B-C", "3f2a9c1e-7b4d-4e8a-9f00-1c2d3e4f5a6b", strings.Repeat("e", maxExecutionIDLen)} {
		h := http.Header{}
		h.Set(HeaderExecutionID, value)
		if got := executionIDFromHeader(h); got != value {
			t.Errorf("executionIDFromHeader(%q) = %q, want it kept", value, got)
		}
	}
}

// TestInjectIdentity_ReadsTheExecutionIDBeforeStrippingIt is the ordering trap.
//
// injectIdentity strips every X-Elitea-* header off the outbound request, and
// the outbound header map is a COPY of the inbound one — so an execution id
// read after the strip is always empty. That failure is silent: the column
// would be NULL on every row and the agent breakdown empty forever, with
// nothing erroring.
func TestInjectIdentity_ReadsTheExecutionIDBeforeStrippingIt(t *testing.T) {
	out := http.Header{}
	out.Set(HeaderExecutionID, "exec-9")
	// A client-spoofed project id must still be replaced by the edge-resolved
	// one (here: absent), which is what the strip is for.
	out.Set(HeaderProjectID, "999")

	injectIdentity(t.Context(), out, []byte(frozenSecret), nil)

	if got := out.Get(HeaderExecutionID); got != "exec-9" {
		t.Fatalf("execution id header = %q, want it read before the strip and re-emitted", got)
	}
	if got := out.Get(HeaderProjectID); got != "" {
		t.Fatalf("a client-supplied project id survived the strip: %q", got)
	}
}

// TestInjectIdentity_AnInvalidExecutionIDLeavesNoHeader: the drop has to happen
// before the header is re-emitted, or an unvalidated value would be forwarded
// under a name the gateway trusts.
func TestInjectIdentity_AnInvalidExecutionIDLeavesNoHeader(t *testing.T) {
	out := http.Header{}
	out.Set(HeaderExecutionID, "exec 9")

	injectIdentity(t.Context(), out, []byte(frozenSecret), nil)

	if got := out.Get(HeaderExecutionID); got != "" {
		t.Fatalf("an invalid execution id was forwarded as %q", got)
	}
}
