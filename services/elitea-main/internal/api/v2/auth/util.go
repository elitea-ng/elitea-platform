package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

func generateUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:]), nil
}

func safeRedirectTarget(value string) string {
	if value == "" || strings.ContainsAny(value, "\\\r\n") || strings.HasPrefix(value, "//") {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "/"
	}
	return value
}

// signBrowserValue appends a keyed MAC to a per-login value carried in a cookie.
//
// The browser holds the value and can read it; the MAC is what stops it being
// EDITED. Both the OIDC state cookie and the SAML request cookie carry a
// server-chosen identifier plus a redirect target, and a target the browser
// could rewrite would be an open redirect through the login.
//
// The separator is "." and the value may not contain one, which
// verifyBrowserValue enforces by splitting on the LAST one rather than the
// first. Splitting on the first would let a value carrying a dot move the
// boundary and present part of itself as the signature.
func signBrowserValue(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return value + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyBrowserValue returns the signed value, or false when the MAC does not
// match. The comparison is constant time.
func verifyBrowserValue(secret, cookie string) (string, bool) {
	separator := strings.LastIndex(cookie, ".")
	if separator < 0 {
		return "", false
	}
	value, signature := cookie[:separator], cookie[separator+1:]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	if !hmac.Equal([]byte(signature), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return "", false
	}
	return value, true
}

// normalizeAssertedEmail reduces an address an identity provider asserted to
// the form the account directory stores: trimmed and lower case.
//
// SCIM stores a lower-case userName (scimdirectory.NormalizeUserName). An
// identity provider can assert the same person as "Alice@Corp.com". Without
// this, a first login by such a person creates a second account instead of
// adopting the one SCIM provisioned for them.
func normalizeAssertedEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// SignInPath is the sign-in page every single sign-on plane returns to.
const SignInPath = "/forward-auth/login"

// SignInErrorURL is the sign-in page with the generic login error banner, and
// the return target the failed login carried.
func SignInErrorURL(target string) string {
	return SignInPath + "?error=sso_failed&target_to=" + url.QueryEscape(safeRedirectTarget(target))
}
