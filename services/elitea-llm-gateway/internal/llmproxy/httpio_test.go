package llmproxy

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func bErr(status int, errType, code, msg string) *schemas.BifrostError {
	e := &schemas.BifrostError{Error: &schemas.ErrorField{Message: msg}}
	if status != 0 {
		e.StatusCode = intPtr(status)
	}
	if errType != "" {
		e.Error.Type = strPtr(errType)
	}
	if code != "" {
		e.Error.Code = strPtr(code)
	}
	return e
}

func TestStatusAndType(t *testing.T) {
	tests := []struct {
		name       string
		in         *schemas.BifrostError
		wantStatus int
		wantType   string
		wantCode   string
	}{
		{"budget by 402", bErr(http.StatusPaymentRequired, "", "", "over budget"), http.StatusPaymentRequired, "budget_exceeded", "insufficient_quota"},
		{"budget by code on 400", bErr(http.StatusBadRequest, "", "budget_exceeded", "x"), http.StatusPaymentRequired, "budget_exceeded", "insufficient_quota"},
		{"budget by insufficient_quota code", bErr(http.StatusBadRequest, "", "insufficient_quota", "x"), http.StatusPaymentRequired, "budget_exceeded", "insufficient_quota"},
		// FIX finding #13: 429 must always use code="rate_limit_exceeded", not the provider's raw code.
		{"rate limit normalised", bErr(http.StatusTooManyRequests, "", "slow", "x"), http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
		{"rate limit provider code overridden", bErr(http.StatusTooManyRequests, "", "tokens_per_min_exceeded", "x"), http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
		{"rate limit empty code normalised", bErr(http.StatusTooManyRequests, "", "", "x"), http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
		// FIX finding #15: 401 must always use code="unauthenticated".
		{"auth normalised", bErr(http.StatusUnauthorized, "", "", "x"), http.StatusUnauthorized, "authentication_error", "unauthenticated"},
		{"auth keeps provider type", bErr(http.StatusUnauthorized, "invalid_api_key", "", "x"), http.StatusUnauthorized, "invalid_api_key", "unauthenticated"},
		// FIX finding #16: 403 must always use code="forbidden".
		{"permission normalised", bErr(http.StatusForbidden, "", "", "x"), http.StatusForbidden, "permission_error", "forbidden"},
		{"permission keeps provider type", bErr(http.StatusForbidden, "insufficient_permissions", "", "x"), http.StatusForbidden, "insufficient_permissions", "forbidden"},
		{"infra 503", bErr(http.StatusServiceUnavailable, "", "", "x"), http.StatusServiceUnavailable, "api_error", ""},
		{"passthrough 400", bErr(http.StatusBadRequest, "invalid_request_error", "", "x"), http.StatusBadRequest, "invalid_request_error", ""},
		{"no status defaults 500", bErr(0, "", "", "x"), http.StatusInternalServerError, "api_error", ""},
		// A provider adapter's own "not implemented" refusal carries no status.
		// It is a 501 the caller can act on, not a 500 that reads as a crash.
		{"unsupported operation is 501", bErr(0, "", "unsupported_operation", "speech is not supported by vllm provider"), http.StatusNotImplemented, "invalid_request_error", "unsupported_operation"},
		// An upstream that answered with its own status keeps it.
		{"unsupported operation keeps upstream status", bErr(http.StatusBadRequest, "", "unsupported_operation", "x"), http.StatusBadRequest, "api_error", "unsupported_operation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, code := statusAndType(tt.in)
			if status != tt.wantStatus || errType != tt.wantType || code != tt.wantCode {
				t.Errorf("statusAndType = (%d,%q,%q), want (%d,%q,%q)",
					status, errType, code, tt.wantStatus, tt.wantType, tt.wantCode)
			}
		})
	}
}

func TestIsBudgetError(t *testing.T) {
	if !isBudgetError(http.StatusPaymentRequired, "", "") {
		t.Error("402 should be a budget error")
	}
	if !isBudgetError(http.StatusBadRequest, "budget_exceeded", "") {
		t.Error("budget_exceeded type should be a budget error")
	}
	if isBudgetError(http.StatusBadRequest, "", "") {
		t.Error("plain 400 should not be a budget error")
	}
}

func TestOpenAIErrorBody(t *testing.T) {
	body := openAIErrorBody(bErr(http.StatusTooManyRequests, "", "", "slow down"))
	if body.Error.Type != "rate_limit_error" {
		t.Errorf("type = %q, want rate_limit_error", body.Error.Type)
	}
	if body.Error.Message != "slow down" {
		t.Errorf("message = %q, want 'slow down'", body.Error.Message)
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "def") != "def" {
		t.Error("empty string should return default")
	}
	if orDefault("x", "def") != "x" {
		t.Error("non-empty string should be returned as-is")
	}
}

func TestIsStream(t *testing.T) {
	if isStream(nil) {
		t.Error("nil should be false")
	}
	f := false
	if isStream(&f) {
		t.Error("*false should be false")
	}
	tr := true
	if !isStream(&tr) {
		t.Error("*true should be true")
	}
}

func TestHeaderPrefixHelpers(t *testing.T) {
	if canonicalLower("X-LiteLLM-Foo") != "x-litellm-foo" {
		t.Error("canonicalLower should lowercase")
	}
	if !hasPrefix("x-litellm-foo", "x-litellm-") {
		t.Error("hasPrefix should match")
	}
	if hasPrefix("server", "x-litellm-") {
		t.Error("hasPrefix should not match unrelated key")
	}
}

func TestErrHelpers(t *testing.T) {
	if errRequired("model").Error() != "model field is required" {
		t.Errorf("errRequired = %q", errRequired("model").Error())
	}
	if wrapInvalid("n").Error() != "invalid n value" {
		t.Errorf("wrapInvalid = %q", wrapInvalid("n").Error())
	}
}

// ─── issue #13: upstream response bodies must not be echoed to callers ──────

// TestOpenAIErrorBody_DoesNotEchoUpstreamBody is the issue #13 read-primitive
// regression guard.
//
// The destination the gateway dials for the self-hosted provider classes is a
// TENANT-AUTHORED api_base. bifrost/core, when an upstream returns a non-2xx
// body that is neither valid JSON nor HTML, puts the ENTIRE body verbatim into
// Error.Message (core@v1.7.3 providers/utils/utils.go: `fmt.Sprintf("provider
// API error: %s", string(decodedBody))`). openAIErrorBody used to copy that
// straight into the client-visible envelope, which turned a blind SSRF into a
// full READ of anything the gateway pod can reach.
//
// Mutation: restore `message = bErr.Error.Message` in openAIErrorBody (drop the
// sanitiseUpstreamMessage call) — the first two subtests MUST fail.
func TestOpenAIErrorBody_DoesNotEchoUpstreamBody(t *testing.T) {
	// A plaintext body of the shape an in-cluster Go service emits.
	const internalBody = "404 page not found\nX-Internal-Token: topsecret-do-not-leak"

	t.Run("raw upstream body is replaced, not echoed", func(t *testing.T) {
		body := openAIErrorBody(bErr(404, "api_error", "", rawUpstreamBodyPrefix+internalBody))
		if strings.Contains(body.Error.Message, "404 page not found") ||
			strings.Contains(body.Error.Message, "topsecret-do-not-leak") {
			t.Fatalf("upstream response body echoed to the caller: %q — this is an SSRF read primitive (issue #13)",
				body.Error.Message)
		}
		if !strings.Contains(body.Error.Message, "404") {
			t.Fatalf("the replacement should still report the upstream status; got %q", body.Error.Message)
		}
	})

	t.Run("no fragment of a long body survives truncation", func(t *testing.T) {
		long := strings.Repeat("SECRET", 500)
		body := openAIErrorBody(bErr(500, "api_error", "", rawUpstreamBodyPrefix+long))
		if strings.Contains(body.Error.Message, "SECRET") {
			t.Fatalf("a fragment of the upstream body survived: %q", body.Error.Message)
		}
	})

	t.Run("an unrecognised long message is capped", func(t *testing.T) {
		// Second line of defence: a future bifrost verbatim-body path with a
		// different prefix must not leak an unbounded read.
		long := strings.Repeat("A", maxUpstreamMessage*4)
		body := openAIErrorBody(bErr(500, "api_error", "", long))
		if len(body.Error.Message) > maxUpstreamMessage+len("… (truncated)") {
			t.Fatalf("upstream message not capped: %d bytes", len(body.Error.Message))
		}
	})

	t.Run("a parsed provider message is preserved", func(t *testing.T) {
		// Messages bifrost extracted from a STRUCTURED provider error are the
		// tenant's own useful diagnostics; sanitising them too would be a
		// regression in its own right.
		const parsed = "You exceeded your current quota, please check your plan and billing details."
		body := openAIErrorBody(bErr(429, "insufficient_quota", "insufficient_quota", parsed))
		if body.Error.Message != parsed {
			t.Fatalf("parsed provider message was mangled: got %q, want %q", body.Error.Message, parsed)
		}
	})

	t.Run("bifrost's body-free fallbacks are preserved", func(t *testing.T) {
		// "provider API error (status 502)" carries no body and must NOT be
		// caught by the prefix rule (the colon-space is what distinguishes it).
		const fallback = "provider API error (status 502)"
		body := openAIErrorBody(bErr(502, "api_error", "", fallback))
		if body.Error.Message != fallback {
			t.Fatalf("body-free fallback was rewritten: got %q", body.Error.Message)
		}
	})
}

// TestRawUpstreamBodyPrefix_MatchesBifrost pins the prefix constant to the
// format string bifrost actually uses. If a core upgrade changes it, the
// sanitiser silently stops matching and the read primitive returns — so assert
// the exact literal here, where a diff is visible in review.
func TestRawUpstreamBodyPrefix_MatchesBifrost(t *testing.T) {
	want := fmt.Sprintf("provider API error: %s", "")
	if rawUpstreamBodyPrefix != want {
		t.Fatalf("rawUpstreamBodyPrefix = %q, want %q — re-check "+
			"providers/utils/utils.go HandleProviderAPIError in the vendored bifrost/core version",
			rawUpstreamBodyPrefix, want)
	}
}
