package llmproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// maxRequestBody bounds a decoded JSON body so a malformed or hostile request
// cannot exhaust memory. It is generous enough for large multi-message chat
// payloads (the multipart image paths use their own ParseMultipartForm limit).
const maxRequestBody = 32 << 20 // 32 MiB

// isStream reports whether a dialect request's optional stream flag is set.
func isStream(v *bool) bool { return v != nil && *v }

// decodeJSON reads and JSON-decodes the request body into dst, writing an
// OpenAI-shaped 400 on any read/parse failure. It reports whether decoding
// succeeded. The body is size-capped via http.MaxBytesReader.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error(), "")
		return false
	}
	return true
}

// decodeJSONRaw decodes the body AND returns the bytes it decoded.
//
// It exists for the MCP allowlist, which must see the request as the caller
// wrote it. Decoding into a bifrost request type drops any tool entry bifrost
// does not model, so a check built on the decoded value would be blind to
// exactly the servers it is supposed to judge (policy.MCPServersFromRequest).
//
// Buffering is safe and bounded: MaxBytesReader already caps the body at
// maxRequestBody before a byte is read, and the same cap applied to the
// streaming decode this replaces.
func decodeJSONRaw(w http.ResponseWriter, r *http.Request, dst interface{}) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error(), "")
		return nil, false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error(), "")
		return nil, false
	}
	return raw, true
}

// writeJSON applies response-header hygiene, sets the JSON content type, writes
// the status, and marshals v.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	finish(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// openAIError is the OpenAI-shaped nested error envelope: {"error":{...}}.
type openAIError struct {
	Error openAIErrorFields `json:"error"`
}

type openAIErrorFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	// Scope is set only by writeBudgetRefusal on a gate-decided budget
	// refusal. See budgetScopeFieldProject.
	Scope string `json:"scope,omitempty"`
}

// writeError writes an OpenAI-shaped error body at the given status.
func writeError(w http.ResponseWriter, status int, errType, message, code string) {
	// Attach the gateway's own error TYPE to the request log, if one is
	// running. The type-assertion is the whole integration: it costs nothing
	// when the log is off, and it means the thirty-odd call sites below need to
	// know nothing about logging.
	//
	// The TYPE and not the MESSAGE. The message is the one field here that can
	// carry caller content — an upstream error routinely quotes the offending
	// fragment of the request back — and internal/requestlog has no column it
	// could reach.
	if sink, ok := w.(requestlog.ErrorCodeSetter); ok {
		classification := errType
		if classification == "" {
			classification = code
		}
		sink.SetErrorCode(classification)
	}
	writeJSON(w, status, openAIError{Error: openAIErrorFields{Message: message, Type: errType, Code: code}})
}

// statusAndType maps a *schemas.BifrostError to the gateway's HTTP status and
// OpenAI error type. Budget exhaustion is 402 (design §2 — differs from
// LiteLLM's 429); rate limits are 429; auth 401; permission 403; anything else
// falls back to the provider status or 500.
func statusAndType(bErr *schemas.BifrostError) (int, string, string) {
	status := http.StatusInternalServerError
	if bErr.StatusCode != nil && *bErr.StatusCode != 0 {
		status = *bErr.StatusCode
	}

	var errType, code string
	if bErr.Error != nil {
		if bErr.Error.Type != nil {
			errType = *bErr.Error.Type
		}
		if bErr.Error.Code != nil {
			code = *bErr.Error.Code
		}
	}

	// Normalise the well-known governance/infra classes to the platform's
	// contract regardless of what the provider reported.
	//
	// The spec §2.5 normative code table mandates specific code values:
	//   429 → "rate_limit_exceeded"   (not the provider's raw code string)
	//   401 → "unauthenticated"       (not empty or provider code)
	//   403 → "forbidden"             (not empty or provider code)
	// Provider-specific codes are preserved in the message field (not the code
	// field) at a higher level; the code field is always spec-normalised here.
	switch {
	case isBudgetError(status, errType, code):
		// A budget refusal the PROVIDER reported, not one this gateway's gate
		// decided. It carries the project code because that is the only scope
		// an upstream can speak to: it knows nothing of a member cap. See
		// budgetErrorType for why the type must stay the shared one.
		return http.StatusPaymentRequired, budgetErrorType, budgetCodeProject
	case status == http.StatusTooManyRequests:
		// FIX finding #13: spec §2.5 mandates code="rate_limit_exceeded".
		// The provider's raw code (e.g. "tokens_per_min_exceeded", "slow") must
		// not be forwarded; it belongs in the message, not the code field.
		return http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"
	case status == http.StatusUnauthorized:
		// FIX finding #15: spec §2.5 mandates code="unauthenticated".
		return http.StatusUnauthorized, orDefault(errType, "authentication_error"), "unauthenticated"
	case status == http.StatusForbidden:
		// FIX finding #16: spec §2.5 mandates code="forbidden".
		return http.StatusForbidden, orDefault(errType, "permission_error"), "forbidden"
	case status == http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable, orDefault(errType, "api_error"), code
	case code == unsupportedOperationCode && (bErr.StatusCode == nil || *bErr.StatusCode == 0):
		return http.StatusNotImplemented, orDefault(errType, "invalid_request_error"), code
	}
	return status, orDefault(errType, "api_error"), code
}

// unsupportedOperationCode is the code bifrost puts on the refusal a provider
// adapter gives for an operation it does not implement
// (providers/utils.NewUnsupportedOperationError). The refusal carries no HTTP
// status, so it used to reach the caller as a 500 `api_error`. That reads as a
// crash. The browser voice client (and any other caller) cannot then tell "this
// provider cannot synthesise speech" from "the gateway failed".
//
// It is answered 501 instead. The status says the operation is not
// implemented for this model's provider; a retry does not help. Only the
// status-less refusal is remapped: an UPSTREAM that answers with a status of
// its own keeps that status.
const unsupportedOperationCode = "unsupported_operation"

// isBudgetError recognises budget exhaustion signalled either as HTTP 402 or by
// a budget-shaped type/code on a 4xx.
func isBudgetError(status int, errType, code string) bool {
	if status == http.StatusPaymentRequired {
		return true
	}
	return errType == "budget_exceeded" || code == "budget_exceeded" || code == "insufficient_quota"
}

// writeOpenAIError maps a bifrost error to the OpenAI-shaped error body.
func (h *Handler) writeOpenAIError(w http.ResponseWriter, bErr *schemas.BifrostError) {
	body := openAIErrorBody(bErr)
	status, _, _ := statusAndType(bErr)
	writeJSON(w, status, body)
}

// openAIErrorBody builds the OpenAI-shaped error envelope from a bifrost error
// (used both for unary responses and mid-stream error frames). It is the SINGLE
// point at which an upstream-originated message reaches a client, so the
// sanitiser below lives here.
func openAIErrorBody(bErr *schemas.BifrostError) openAIError {
	status, errType, code := statusAndType(bErr)
	message := ""
	if bErr.Error != nil {
		message = sanitiseUpstreamMessage(bErr.Error.Message, status)
	}
	return openAIError{Error: openAIErrorFields{Message: message, Type: errType, Code: code}}
}

// rawUpstreamBodyPrefix is the marker bifrost/core puts in front of an upstream
// response body it could not parse. From
// core@v1.7.3 providers/utils/utils.go (HandleProviderAPIError):
//
//	message := fmt.Sprintf("provider API error: %s", string(decodedBody))
//
// i.e. when a non-2xx body is neither valid JSON nor HTML, the ENTIRE body is
// placed verbatim in Error.Message. Its structured siblings ("provider API
// error" / "provider API error (status N)", set by the per-provider parsers when
// no message could be extracted) carry no body and are deliberately NOT matched
// by this prefix — the colon-space is what distinguishes them.
const rawUpstreamBodyPrefix = "provider API error: "

// maxUpstreamMessage caps every upstream-originated message that is not caught
// by the prefix rule. It is the second line of defence: if a future bifrost
// version introduces another verbatim-body path with a different prefix, a
// caller reads at most this many bytes of it instead of the whole response.
const maxUpstreamMessage = 256

// sanitiseUpstreamMessage strips the SSRF read primitive out of client-visible
// error messages (issue #13).
//
// The destination the gateway dials for the self-hosted provider classes is a
// TENANT-AUTHORED api_base. Echoing the upstream's response body back to the
// caller therefore turns "the gateway will connect where I say" into "the
// gateway will connect where I say AND read the answer back to me" — a full
// SSRF read primitive against anything the gateway pod can reach, not a blind
// one. Provider messages that bifrost actually PARSED out of a structured error
// body (OpenAI's quota text, a provider's rate-limit detail) are still useful to
// tenants and are preserved; only the verbatim-body echo is removed.
//
// Nothing here depends on which provider or address was dialled: the gateway
// cannot tell an internal host's plaintext 404 from a public one's, so the rule
// is applied to every upstream message.
func sanitiseUpstreamMessage(message string, status int) string {
	if strings.HasPrefix(message, rawUpstreamBodyPrefix) {
		// The remainder IS the upstream response body. Report the fact of the
		// error and its status; never the content.
		return fmt.Sprintf("upstream provider returned an unparsable error response (status %d)", status)
	}
	if schemas.ErrProviderResponseHTML != "" && message == schemas.ErrProviderResponseHTML {
		// bifrost's HTML branch keeps the body out of Message (it goes in
		// Error.Error, which this envelope never reads), but the constant
		// itself says nothing useful to a tenant either.
		return fmt.Sprintf("upstream provider returned an HTML error page (status %d)", status)
	}
	if len(message) > maxUpstreamMessage {
		return message[:maxUpstreamMessage] + "… (truncated)"
	}
	return message
}

// orDefault returns s if non-empty, else def.
func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// canonicalLower lowercases a header key for prefix matching.
func canonicalLower(s string) string { return strings.ToLower(s) }

// hasPrefix reports whether s starts with prefix.
func hasPrefix(s, prefix string) bool { return strings.HasPrefix(s, prefix) }

// setElapsedHeader stamps the gateway's own overhead as X-Elapsed-Ms with
// sub-ms precision. The value covers the request from the instant the handler
// accepted it to the instant bifrost/core held the provider credential: decode,
// identity verification, loop breaker, budget check, the wait in the core
// provider queue, core routing, and the credential resolution (the Account's
// Postgres read and Fernet decrypt).
//
// The value does NOT cover the provider round-trip, which starts only after
// core holds the key, and it does not cover the response marshalling that
// follows. A request that resolves no credential reports the pre-dispatch time
// alone. See the Chat handler's t0 comment and internal/overhead (issue #17).
//
// Compute the argument with overhead.Meter.Overhead. Do NOT compute it with
// time.Since at the call site: the call site runs after the router returned, so
// time.Since would add the provider round-trip to the gateway's metric.
//
// Consumed by the BFF.9d k6 overhead gate (design §10.2). Must be called before
// the first body/status write.
func setElapsedHeader(w http.ResponseWriter, overhead time.Duration) {
	if overhead < 0 {
		overhead = 0
	}
	w.Header().Set("X-Elapsed-Ms", strconv.FormatFloat(float64(overhead)/float64(time.Millisecond), 'f', 3, 64))
}
