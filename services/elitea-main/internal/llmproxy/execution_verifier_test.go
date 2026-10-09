package llmproxy

// The inbound execution id is an AUTHORIZATION-RELEVANT input: the analytics
// reads remove a call from every active-user figure when the id names an
// unattended run, and add the call to a run's spend. These tests hold the
// edge to two rules: an id with a `:` (the in-process evaluation namespace)
// never passes, and every other id passes only when the verifier says it
// names a live execution of the RESOLVED caller.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type stubExecutionVerifier struct {
	ok          bool
	err         error
	calls       []string
	credentials []string
}

func (s *stubExecutionVerifier) VerifyExecution(_ context.Context, projectID, userID, tokenID, nativeClientID, executionID string) (bool, error) {
	s.calls = append(s.calls, projectID+"|"+userID+"|"+executionID)
	s.credentials = append(s.credentials, tokenID+"|"+nativeClientID)
	return s.ok, s.err
}

func TestInjectIdentity_VerifierDecidesTheExecutionID(t *testing.T) {
	cases := map[string]struct {
		verifier *stubExecutionVerifier
		want     string
	}{
		"a live execution of the caller is kept": {&stubExecutionVerifier{ok: true}, "exec-9"},
		"another person's execution is dropped":  {&stubExecutionVerifier{ok: false}, ""},
		"a failed lookup drops the id":           {&stubExecutionVerifier{err: errors.New("db down")}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := http.Header{}
			out.Set(HeaderExecutionID, "exec-9")
			out.Set(HeaderUserID, "999") // client-spoofed: must not reach the verifier
			injectIdentity(fullCtx(), out, []byte(frozenSecret), tc.verifier)
			if got := out.Get(HeaderExecutionID); got != tc.want {
				t.Fatalf("execution id header = %q, want %q", got, tc.want)
			}
			if len(tc.verifier.calls) != 1 || tc.verifier.calls[0] != "42|user-7|exec-9" {
				t.Fatalf("verifier calls = %v, want one call with the resolved identity", tc.verifier.calls)
			}
			// v2 only when the id survived, v1 otherwise: either way the
			// forwarded tuple must verify.
			if !verifyIdentitySignature(out, []byte(frozenSecret)) {
				t.Fatal("the forwarded identity does not verify")
			}
		})
	}
}

// With no resolved project or user there is nothing to check the execution
// against, so the id is dropped without a lookup.
func TestInjectIdentity_VerifierNeedsAResolvedCaller(t *testing.T) {
	verifier := &stubExecutionVerifier{ok: true}
	out := http.Header{}
	out.Set(HeaderExecutionID, "exec-9")
	injectIdentity(t.Context(), out, []byte(frozenSecret), verifier)
	if got := out.Get(HeaderExecutionID); got != "" {
		t.Fatalf("execution id header = %q, want it dropped for an unresolved caller", got)
	}
	if len(verifier.calls) != 0 {
		t.Fatalf("verifier was asked %v for an unresolved caller", verifier.calls)
	}
}

// The shape rule drops an evaluation attribution first, so even a verifier
// that says yes cannot let one through.
func TestInjectIdentity_AnEvaluationIDNeverReachesTheVerifier(t *testing.T) {
	for _, value := range []string{"eval:12:judge:case-1", "eval:12:case:case-1", "exec-9:child"} {
		verifier := &stubExecutionVerifier{ok: true}
		out := http.Header{}
		out.Set(HeaderExecutionID, value)
		injectIdentity(fullCtx(), out, []byte(frozenSecret), verifier)
		if got := out.Get(HeaderExecutionID); got != "" {
			t.Fatalf("inbound %q was forwarded as %q", value, got)
		}
		if len(verifier.calls) != 0 {
			t.Fatalf("verifier was asked %v for %q", verifier.calls, value)
		}
	}
}

// The proxy passes Config.ExecutionVerifier to the rewrite.
func TestProxy_UsesTheConfiguredExecutionVerifier(t *testing.T) {
	verifier := &stubExecutionVerifier{ok: false}
	var seen string
	proxy, err := New(Config{
		TargetURL: "http://gateway.invalid",
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = r.Header.Get(HeaderExecutionID)
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		}),
		ExecutionVerifier: verifier,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, _ := http.NewRequestWithContext(fullCtx(), http.MethodPost, "http://edge/llm/v1/chat/completions", http.NoBody)
	req.Header.Set(HeaderExecutionID, "exec-9")
	proxy.ServeHTTP(newDiscardWriter(), req)
	if len(verifier.calls) != 1 {
		t.Fatalf("verifier calls = %v, want 1", verifier.calls)
	}
	if seen != "" {
		t.Fatalf("the gateway received execution id %q the verifier refused", seen)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type discardWriter struct{ header http.Header }

func newDiscardWriter() *discardWriter               { return &discardWriter{header: http.Header{}} }
func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(int)             {}

// stubCallbackVerifier admits provider invocations too.
type stubCallbackVerifier struct {
	stubExecutionVerifier
	callbackOK bool
	callbacks  []string
}

func (s *stubCallbackVerifier) VerifyCallbackExecution(_ context.Context, projectID, userID, tokenID, tokenUUID string) (bool, error) {
	s.callbacks = append(s.callbacks, projectID+"|"+userID+"|"+tokenID+"|"+tokenUUID)
	return s.callbackOK, nil
}

func callbackCtx(tokenID string) context.Context {
	ctx := fullCtx()
	return auth.ContextWithUser(ctx, auth.User{ID: "user-7", TokenID: tokenID})
}

// A provider invocation's id `callback-<uuid>` goes to the callback rule with
// the AUTHENTICATING token's id, never to the execution_jobs rule.
func TestInjectIdentity_ACallbackIDIsCheckedAgainstTheAuthenticatingToken(t *testing.T) {
	const id = CallbackExecutionPrefix + "11111111-1111-4111-8111-111111111111"
	cases := map[string]struct {
		ctx      context.Context
		verifier ExecutionVerifier
		want     string
		asked    string
	}{
		"the caller's own token is kept": {callbackCtx("31"), &stubCallbackVerifier{callbackOK: true}, id,
			"42|user-7|31|11111111-1111-4111-8111-111111111111"},
		"a token the rule refuses is dropped": {callbackCtx("31"), &stubCallbackVerifier{}, "",
			"42|user-7|31|11111111-1111-4111-8111-111111111111"},
		"a session principal (no token) is dropped":     {callbackCtx(""), &stubCallbackVerifier{callbackOK: true}, "", ""},
		"a verifier without the callback rule drops it": {callbackCtx("31"), &stubExecutionVerifier{ok: true}, "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := http.Header{}
			out.Set(HeaderExecutionID, id)
			injectIdentity(tc.ctx, out, []byte(frozenSecret), tc.verifier)
			if got := out.Get(HeaderExecutionID); got != tc.want {
				t.Fatalf("execution id header = %q, want %q", got, tc.want)
			}
			switch v := tc.verifier.(type) {
			case *stubCallbackVerifier:
				if len(v.calls) != 0 {
					t.Fatalf("the execution_jobs rule was asked %v for a callback id", v.calls)
				}
				if tc.asked == "" && len(v.callbacks) != 0 || tc.asked != "" && (len(v.callbacks) != 1 || v.callbacks[0] != tc.asked) {
					t.Fatalf("callback rule calls = %v, want %q", v.callbacks, tc.asked)
				}
			case *stubExecutionVerifier:
				if len(v.calls) != 0 {
					t.Fatalf("the execution_jobs rule was asked %v for a callback id", v.calls)
				}
			}
			if !verifyIdentitySignature(out, []byte(frozenSecret)) {
				t.Fatal("the forwarded identity does not verify")
			}
		})
	}
}

// The verifier is told the authenticating credential family, so a desktop
// local turn is attributed only to calls from the device or token that
// started it.
func TestInjectIdentity_PassesTheCallersCredentialToTheVerifier(t *testing.T) {
	verifier := &stubExecutionVerifier{ok: true}
	ctx := auth.ContextWithUser(fullCtx(), auth.User{ID: "user-7", TokenID: "70", NativeClientID: "ai.elitea.desktop"})
	out := http.Header{}
	out.Set(HeaderExecutionID, "exec-9")
	injectIdentity(ctx, out, []byte(frozenSecret), verifier)
	if len(verifier.credentials) != 1 || verifier.credentials[0] != "70|ai.elitea.desktop" {
		t.Fatalf("verifier credentials = %v, want the principal's token and native client", verifier.credentials)
	}
}
