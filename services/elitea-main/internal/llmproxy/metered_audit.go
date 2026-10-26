package llmproxy

// metered_audit.go — the audit row of a metered tool call (#1238).
//
// The worker cannot write elitea-main's audit trail, and the gateway does not
// own one, so the edge writes it: the settle (and admit) body may carry an
// `audit` object naming the tool, its target and its outcome, and the edge
// records ONE audit.Event per settled call, and one per admission the gateway
// refused. The gateway ignores the key. A settle retry that the gateway answers
// `"duplicate": true` (the event id was already settled) writes no second row.
//
// The target is what the worker chose to disclose: a URL HOST, a sha256 hex of
// a query, or the looked-up IP. Never a full URL (a provider key can ride in a
// query string) and never content. It is bounded and refused when it carries a
// control character; an unusable target is recorded as empty rather than
// failing the call, because an audit row may never fail what it describes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// maxMeteredBodyBytes caps a metered admit or settle body. Both are a few
// hundred bytes; the edge buffers the body to read its audit object.
const maxMeteredBodyBytes = 64 << 10

// maxAuditTargetBytes bounds the audited target.
const maxAuditTargetBytes = 256

// meteredAuditTools are the tools a metered call may name.
var meteredAuditTools = map[string]bool{
	"web_search": true, "web_fetch": true,
	"internet_host_search": true, "internet_host_lookup": true,
}

// meteredAuditRoute is the route pattern the audit row names.
const meteredAuditRoute = "/llm/v1/metered/*"

// meteredAudit is the optional `audit` object of a metered body, plus the
// fields of the body the row repeats.
type meteredAudit struct {
	Tool     string
	Target   string
	Status   string
	Provider string
	EventID  string
}

type meteredBody struct {
	Provider string `json:"provider"`
	EventID  string `json:"event_id"`
	Audit    *struct {
		Tool   string `json:"tool"`
		Target string `json:"target"`
		Status string `json:"status"`
	} `json:"audit"`
}

// readMeteredBody buffers a metered body (refusing one over the cap) and
// returns it with its audit object, nil when the body names none or names an
// unknown tool.
func readMeteredBody(r *http.Request) ([]byte, *meteredAudit, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMeteredBodyBytes+1))
	if err != nil || len(body) > maxMeteredBodyBytes {
		return nil, nil, false
	}
	var parsed meteredBody
	if json.Unmarshal(body, &parsed) != nil || parsed.Audit == nil || !meteredAuditTools[parsed.Audit.Tool] {
		return body, nil, true
	}
	return body, &meteredAudit{
		Tool:     parsed.Audit.Tool,
		Target:   auditSafe(parsed.Audit.Target, maxAuditTargetBytes),
		Status:   auditSafe(parsed.Audit.Status, 32),
		Provider: auditSafe(parsed.Provider, 64),
		EventID:  auditSafe(parsed.EventID, 64),
	}, true
}

// auditSafe returns value when it is valid UTF-8, at most limit bytes and free
// of control characters, and "" otherwise.
func auditSafe(value string, limit int) string {
	if len(value) > limit || !utf8.ValidString(value) {
		return ""
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

// refusalStatus names an admission the gateway refused, by its HTTP status.
func refusalStatus(code int) string {
	switch code {
	case http.StatusPaymentRequired:
		return "refused_budget"
	case http.StatusTooManyRequests:
		return "refused_rate"
	case http.StatusServiceUnavailable:
		return "refused_unavailable"
	case http.StatusUnprocessableEntity:
		return "refused_unpriced"
	}
	return "refused"
}

// recordMeteredAudit writes the row for one metered call: a settled call
// (rpc settle, 2xx, not a duplicate) or a refused admission (rpc admit,
// non-2xx). Anything else writes nothing: an admitted call is recorded when it
// settles, and a deduplicated settle retry was recorded by its first settle.
func (p *Proxy) recordMeteredAudit(ctx context.Context, rpc gatewayRPC, id Identity, entry *meteredAudit, status int, started time.Time, duplicate bool) {
	if p.audit == nil || entry == nil {
		return
	}
	outcome := entry.Status
	switch {
	case rpc == rpcMeteredSettle && status >= 200 && status <= 299 && !duplicate:
	case rpc == rpcMeteredAdmit && (status < 200 || status > 299):
		outcome = refusalStatus(status)
	default:
		return
	}
	action := entry.Tool + " via " + strconv.Quote(entry.Provider) + ": " + outcome +
		"; target " + strconv.Quote(entry.Target) + "; execution " + id.ExecutionID + "; event " + entry.EventID
	code := int32(status) //nolint:gosec // an HTTP status fits
	duration := float64(time.Since(started).Microseconds()) / 1000
	p.audit.Record(context.WithoutCancel(ctx), audit.Event{
		Timestamp:  time.Now().UTC(),
		UserID:     optionalID(id.UserID),
		ProjectID:  optionalID(id.ProjectID),
		EventType:  "agent",
		Action:     action,
		HTTPMethod: http.MethodPost,
		HTTPRoute:  meteredAuditRoute,
		StatusCode: &code,
		DurationMS: &duration,
		IsError:    status >= http.StatusBadRequest || outcome != "ok",
		EntityType: "metered_tool",
		EntityName: entry.Tool,
	})
}

// optionalID parses an identity id, nil when it is not a positive integer.
func optionalID(value string) *int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return audit.ID(n)
}

// maxSettleAnswerBytes caps the settle answer the edge buffers to read its
// duplicate flag (it is two fields).
const maxSettleAnswerBytes = 16 << 10

// settleWasDuplicate reads a 2xx settle answer's `duplicate` flag and puts
// the bytes back for the caller. An unreadable answer reads as not a
// duplicate: the row is then written, as before the flag existed.
func settleWasDuplicate(resp *Response) bool {
	if resp.StatusCode < 200 || resp.StatusCode > 299 || resp.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSettleAnswerBytes+1))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > maxSettleAnswerBytes {
		resp.ContentLength = -1
		return false
	}
	resp.ContentLength = int64(len(body))
	var answer struct {
		Duplicate bool `json:"duplicate"`
	}
	return json.Unmarshal(body, &answer) == nil && answer.Duplicate
}

// meteredRequestBody rewinds a buffered body for the gateway call.
func meteredRequestBody(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}
