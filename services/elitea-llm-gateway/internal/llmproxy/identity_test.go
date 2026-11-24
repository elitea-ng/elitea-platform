package llmproxy

import (
	"net/http"
	"testing"
)

// TestVerifySignature_EmptySecretRefusesEveryIdentity pins the fail-closed
// rule: with no secret there is nothing to verify against, so no identity is
// accepted, signed or not.
func TestVerifySignature_EmptySecretRefusesEveryIdentity(t *testing.T) {
	signed := http.Header{}
	SignIdentityHeaders(signed, []byte("some-other-secret-of-thirty-two-bytes"), "42", "user-7", "tenant-1", "")
	for name, h := range map[string]http.Header{
		"no headers":     {},
		"unsigned":       {headerProjectID: {"42"}},
		"signed by peer": signed,
	} {
		if verifySignature(h, nil) {
			t.Errorf("%s: nil secret accepted the identity", name)
		}
		if verifySignature(h, []byte{}) {
			t.Errorf("%s: zero-length secret accepted the identity", name)
		}
	}
}

// TestNewHandler_EmptySecretRefusesEveryIdentity proves the production
// constructor wires the fail-closed verifier: a Handler built without a secret
// refuses an unsigned identity.
func TestNewHandler_EmptySecretRefusesEveryIdentity(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil)
	hdr := http.Header{}
	hdr.Set(headerProjectID, "42")
	if h.verifyIdentity(hdr) {
		t.Fatal("a Handler without an identity secret accepted an unsigned identity")
	}
}

func TestVerifySignature_MissingHeaderRejected(t *testing.T) {
	h := http.Header{}
	h.Set(headerProjectID, "42")
	if verifySignature(h, []byte("secret")) {
		t.Error("configured secret with no signature header should be rejected")
	}
}

func TestVerifySignature_ValidRoundTrip(t *testing.T) {
	secret := []byte("shared-secret")
	id := identity{projectID: "42", userID: "user-7", tenantID: "tenant-1"}

	h := http.Header{}
	h.Set(headerProjectID, id.projectID)
	h.Set(headerUserID, id.userID)
	h.Set(headerTenantID, id.tenantID)
	h.Set(headerSignature, id.sign(secret))

	if !verifySignature(h, secret) {
		t.Error("valid signature should verify")
	}
}

func TestVerifySignature_TamperedFieldRejected(t *testing.T) {
	secret := []byte("shared-secret")
	id := identity{projectID: "42", userID: "user-7", tenantID: "tenant-1"}

	h := http.Header{}
	h.Set(headerProjectID, id.projectID)
	h.Set(headerUserID, id.userID)
	h.Set(headerTenantID, id.tenantID)
	h.Set(headerSignature, id.sign(secret))

	// Tamper with the project after signing: the recomputed MAC must diverge.
	h.Set(headerProjectID, "999")
	if verifySignature(h, secret) {
		t.Error("tampered projectID should fail verification")
	}
}

func TestVerifySignature_WrongSecretRejected(t *testing.T) {
	id := identity{projectID: "42"}
	h := http.Header{}
	h.Set(headerProjectID, id.projectID)
	h.Set(headerSignature, id.sign([]byte("real-secret")))

	if verifySignature(h, []byte("other-secret")) {
		t.Error("signature under a different secret should fail verification")
	}
}

func TestCanonical_VersionPrefixedAndNewlineSeparated(t *testing.T) {
	id := identity{projectID: "1", userID: "2", tenantID: "3"}
	want := "v1\n1\n2\n3"
	if got := id.canonical(signatureVersionV1); got != want {
		t.Errorf("canonical() = %q, want %q", got, want)
	}
}

func TestCanonical_FieldConcatenationIsUnambiguous(t *testing.T) {
	// project "1" + user "2" must not collide with project "12" + user "".
	a := identity{projectID: "1", userID: "2"}
	b := identity{projectID: "12", userID: ""}
	if a.canonical(signatureVersionV1) == b.canonical(signatureVersionV1) {
		t.Error("newline separation must disambiguate field boundaries")
	}
}

func TestIdentityFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(headerProjectID, "p")
	h.Set(headerUserID, "u")
	h.Set(headerTenantID, "t")

	id := identityFromHeaders(h)
	if id.projectID != "p" || id.userID != "u" || id.tenantID != "t" {
		t.Errorf("identityFromHeaders = %+v, want p/u/t", id)
	}
}

func TestSign_HasSha256Prefix(t *testing.T) {
	id := identity{projectID: "1"}
	sig := id.sign([]byte("k"))
	if len(sig) < len("sha256=") || sig[:len("sha256=")] != "sha256=" {
		t.Errorf("sign() = %q, want sha256= prefix", sig)
	}
}
