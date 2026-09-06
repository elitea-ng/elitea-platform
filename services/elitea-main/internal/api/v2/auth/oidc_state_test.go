package auth

// THE STATE COOKIE OF ONE OIDC LOGIN.
//
// THE DEFECT. The cookie is `<nonce>|<target_to>.<mac>`, and Callback split it
// on the FIRST ".". `target_to` is a path the caller chose and may carry a dot;
// the MAC is hex and never does. A login sent to `/artifacts/report.v2.pdf`
// therefore had its boundary moved, the MAC was verified over half the state,
// and the browser was answered "state cookie signature invalid" — a login that
// could never succeed while that target was asked for.
//
// saml.go signs and verifies the same shape with signBrowserValue and
// verifyBrowserValue, which split on the LAST ".", because the same defect was
// found and fixed there. This handler kept a private copy of the split. It now
// uses the shared pair.

import "testing"

func TestAStateCookieWhoseTargetCarriesADotStillVerifies(t *testing.T) {
	handler := &OIDCHandler{secretKey: "test-secret"}

	for name, target := range map[string]string{
		"a plain path":       "/projects",
		"a dotted file":      "/artifacts/report.v2.pdf",
		"a dotted path part": "/redirect/dev.elitea.ai/chat",
		"the default":        "/",
	} {
		t.Run(name, func(t *testing.T) {
			state := "nonce-1|" + target
			cookie := signBrowserValue(handler.secretKey, state)

			got, ok := handler.consumeState(cookie, state)
			if !ok {
				t.Fatalf("the state cookie for target %q was refused", target)
			}
			if got != target {
				t.Fatalf("target = %q, want %q", got, target)
			}
		})
	}
}

// The state must still be the state of THIS callback, and the target must
// still be the one the login signed. The dotted-target fix costs neither
// check.
func TestAStateCookieIsRefusedWhenItDoesNotBelongToThisCallback(t *testing.T) {
	handler := &OIDCHandler{secretKey: "test-secret"}
	const state = "nonce-1|/artifacts/report.v2.pdf"
	cookie := signBrowserValue(handler.secretKey, state)

	for name, testCase := range map[string]struct {
		cookie     string
		queryState string
	}{
		"the provider echoed another state": {cookie: cookie, queryState: "nonce-2|/"},
		"the provider echoed nothing":       {cookie: cookie, queryState: ""},
		"the browser edited the target": {
			cookie:     "nonce-1|/evil" + cookie[len(state):],
			queryState: "nonce-1|/evil",
		},
		"the cookie was signed with another key": {
			cookie:     signBrowserValue("another-secret", state),
			queryState: state,
		},
		"the cookie carries no signature at all": {cookie: state, queryState: state},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := handler.consumeState(testCase.cookie, testCase.queryState); ok {
				t.Fatal("the callback accepted a state cookie it must refuse")
			}
		})
	}
}

// The target is read back through safeRedirectTarget, so a state cookie cannot
// carry an open redirect even when this server signed it.
func TestTheStateTargetIsStillNarrowedToThisSite(t *testing.T) {
	handler := &OIDCHandler{secretKey: "test-secret"}
	const state = "nonce-1|https://attacker.example.com/"

	target, ok := handler.consumeState(signBrowserValue(handler.secretKey, state), state)
	if !ok {
		t.Fatal("a correctly signed state cookie was refused")
	}
	if target != "/" {
		t.Fatalf("target = %q, want %q", target, "/")
	}
}
