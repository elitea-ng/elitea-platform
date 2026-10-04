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
)

type stubExecutionVerifier struct {
	ok    bool
	err   error
	calls []string
}

func (s *stubExecutionVerifier) VerifyExecution(_ context.Context, projectID, userID, executionID string) (bool, error) {
	s.calls = append(s.calls, projectID+"|"+userID+"|"+executionID)
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
