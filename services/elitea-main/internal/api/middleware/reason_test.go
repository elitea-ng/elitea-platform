package middleware

// The reason check of a dangerous action (ADR-0059 R3.8a, issue 2539), driven
// over a router that mounts every route of the action table, below the audit
// emitter as router.go mounts it. The cases are generated from the registry:
// one per entry whose Reason is set, so an entry added to the table is
// covered the day it is added.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// reasonServedStatus is what every handler of the test router answers: a
// status no gate produces, so "the normal status" is unambiguous.
const reasonServedStatus = http.StatusAccepted

// reasonHandlerSeen is what the last handler saw, for the body and context
// assertions.
type reasonHandlerSeen struct {
	body       string
	reasonCode string
}

// newReasonRouter mounts every (method, pattern) of the action table behind
// Audit and the reason check, as router.go does, and marks the check with the
// finished router.
func newReasonRouter(t *testing.T, recorder audit.Recorder) (http.Handler, *reasonHandlerSeen) {
	t.Helper()
	seen := &reasonHandlerSeen{}
	check := NewReasonCheck()
	r := chi.NewRouter()
	r.Use(Audit(recorder))
	r.Use(check.Middleware)
	handler := func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		seen.body = string(raw)
		seen.reasonCode = audit.ReasonCodeFromContext(req.Context())
		w.WriteHeader(reasonServedStatus)
	}
	mounted := map[string]bool{}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if key := entry.Method + " " + entry.Pattern; !mounted[key] {
			mounted[key] = true
			r.Method(entry.Method, entry.Pattern, http.HandlerFunc(handler))
		}
	}
	check.Mark(r)
	return r, seen
}

var placeholder = regexp.MustCompile(`\{([^}]+)\}`)

// reasonRequestPath fills the pattern of entry: the administration mode, the
// selected path value, and 1 for every other placeholder.
func reasonRequestPath(entry adminaccess.Action) string {
	return placeholder.ReplaceAllStringFunc(entry.Pattern, func(match string) string {
		name := match[1 : len(match)-1]
		switch {
		case name == "mode":
			return "administration"
		case entry.Selector.In == adminaccess.SelectPath && name == entry.Selector.Field:
			return entry.Selector.Value
		}
		return "1"
	})
}

func serveReason(t *testing.T, router http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func lastEvent(t *testing.T, recorder *captureRecorder) audit.Event {
	t.Helper()
	events := recorder.all()
	if len(events) == 0 {
		t.Fatal("no admin audit row was written")
	}
	return events[len(events)-1]
}

// One case per marked entry: a request without a reason answers its normal
// status with Elitea-Reason-Missing: 1, and the admin audit row has
// reason_missing.
func TestEveryMarkedActionWithoutAReasonIsServedAndFlagged(t *testing.T) {
	if !adminaccess.ReasonOptionalUntilSunset {
		t.Skip("the warning period is over (R4.8)")
	}
	marked := 0
	for _, entry := range adminaccess.ActionTable().Entries() {
		if !entry.Reason {
			continue
		}
		marked++
		t.Run(entry.Key(), func(t *testing.T) {
			recorder := &captureRecorder{}
			router, seen := newReasonRouter(t, recorder)
			body := entry.Selector.Body()
			rec := serveReason(t, router, entry.Method, reasonRequestPath(entry), body, nil)
			if rec.Code != reasonServedStatus {
				t.Fatalf("status = %d %s, want the handler's %d", rec.Code, rec.Body, reasonServedStatus)
			}
			if got := rec.Header().Get(adminaccess.ReasonMissingHeader); got != "1" {
				t.Errorf("%s = %q, want 1", adminaccess.ReasonMissingHeader, got)
			}
			if seen.body != body {
				t.Errorf("the handler read %q, want the whole body %q", seen.body, body)
			}
			event := lastEvent(t, recorder)
			if event.ReasonMissing == nil || !*event.ReasonMissing {
				t.Errorf("reason_missing = %v, want true", event.ReasonMissing)
			}
			if event.ReasonCode != "" || event.Reason != "" {
				t.Errorf("reason = %q %q, want none", event.ReasonCode, event.Reason)
			}
		})
	}
	if marked < 30 {
		t.Fatalf("found %d marked entries; the action table was not read", marked)
	}
}

// A route with no reason, and the same project delete in the default mode,
// get no header and no reason_missing.
func TestAnUnmarkedActionIsLeftAlone(t *testing.T) {
	recorder := &captureRecorder{}
	router, _ := newReasonRouter(t, recorder)
	for _, path := range []string{"/api/v2/projects/project/default/1", "/api/v2/admin/native_devices/administration/1"} {
		rec := serveReason(t, router, http.MethodDelete, path, "", nil)
		if rec.Code != reasonServedStatus || rec.Header().Get(adminaccess.ReasonMissingHeader) != "" {
			t.Errorf("DELETE %s = %d with %q, want %d and no header", path, rec.Code,
				rec.Header().Get(adminaccess.ReasonMissingHeader), reasonServedStatus)
		}
		if event := lastEvent(t, recorder); event.ReasonMissing != nil {
			t.Errorf("DELETE %s: reason_missing = %v, want NULL", path, *event.ReasonMissing)
		}
	}
	// An unmarked action of a multiplexed route: the maintenance section off.
	rec := serveReason(t, router, http.MethodPut, "/api/v2/admin/plugin_config_values/administration/email", `{"values":{}}`, nil)
	if rec.Header().Get(adminaccess.ReasonMissingHeader) != "" {
		t.Errorf("PUT the email section answered %s", adminaccess.ReasonMissingHeader)
	}
}

const brandingResetPath = "/api/v2/admin/branding/administration"

// A DELETE with Elitea-Reason-Code and a percent-encoded non-ASCII
// Elitea-Reason stores the code and the decoded text on the audit row, and
// the code alone on the request context for the security events.
func TestADeleteCarriesItsReasonInHeaders(t *testing.T) {
	const text = "Сброс по заявке — 障害対応 #42"
	recorder := &captureRecorder{}
	router, seen := newReasonRouter(t, recorder)
	rec := serveReason(t, router, http.MethodDelete, brandingResetPath, "", http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonRequest},
		adminaccess.ReasonHeader:     {url.PathEscape(text)},
	})
	if rec.Code != reasonServedStatus {
		t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, reasonServedStatus)
	}
	if got := rec.Header().Get(adminaccess.ReasonMissingHeader); got != "" {
		t.Errorf("%s = %q on a request with a reason", adminaccess.ReasonMissingHeader, got)
	}
	event := lastEvent(t, recorder)
	if event.ReasonCode != adminaccess.ReasonRequest || event.Reason != text {
		t.Errorf("audit reason = %q %q, want %q %q", event.ReasonCode, event.Reason, adminaccess.ReasonRequest, text)
	}
	if event.ReasonMissing == nil || *event.ReasonMissing {
		t.Errorf("reason_missing = %v, want false", event.ReasonMissing)
	}
	if seen.reasonCode != adminaccess.ReasonRequest {
		t.Errorf("the handler's context carries the reason code %q, want %q", seen.reasonCode, adminaccess.ReasonRequest)
	}
}

// A header over 500 bytes after decoding answers 400 reason_invalid, and the
// handler is not reached; exactly 500 bytes is accepted.
func TestAReasonHeaderOver500BytesIsRefused(t *testing.T) {
	recorder := &captureRecorder{}
	router, _ := newReasonRouter(t, recorder)
	// Two bytes per rune in UTF-8: 251 runes are 502 bytes, 250 are 500.
	long := strings.Repeat("я", 251)
	rec := serveReason(t, router, http.MethodDelete, brandingResetPath, "", http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident},
		adminaccess.ReasonHeader:     {url.PathEscape(long)},
	})
	assertReasonRefusal(t, rec)
	if event := lastEvent(t, recorder); event.StatusCode == nil || *event.StatusCode != http.StatusBadRequest {
		t.Errorf("the audit row has status %v, want 400", event.StatusCode)
	}
	rec = serveReason(t, router, http.MethodDelete, brandingResetPath, "", http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident},
		adminaccess.ReasonHeader:     {url.PathEscape(strings.Repeat("я", 250))},
	})
	if rec.Code != reasonServedStatus {
		t.Errorf("a 500-byte reason = %d %s, want %d", rec.Code, rec.Body, reasonServedStatus)
	}
}

// The other refusals: an unknown code, `other` with no text, a header that
// does not decode, a header sent twice, a body field that is not a string.
func TestAnInvalidReasonIsRefused(t *testing.T) {
	router, _ := newReasonRouter(t, &captureRecorder{})
	for name, tc := range map[string]struct {
		method, path, body string
		header             http.Header
	}{
		"unknown code":       {http.MethodDelete, brandingResetPath, "", http.Header{adminaccess.ReasonCodeHeader: {"because"}}},
		"other without text": {http.MethodDelete, brandingResetPath, "", http.Header{adminaccess.ReasonCodeHeader: {adminaccess.ReasonOther}}},
		"bad escape": {http.MethodDelete, brandingResetPath, "", http.Header{
			adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident}, adminaccess.ReasonHeader: {"50%"}}},
		"not UTF-8": {http.MethodDelete, brandingResetPath, "", http.Header{
			adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident}, adminaccess.ReasonHeader: {"%ff%fe"}}},
		"header twice": {http.MethodDelete, brandingResetPath, "", http.Header{
			adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident, adminaccess.ReasonOther}}},
		"body code not a string": {http.MethodPost, "/api/v2/admin/auth_users/administration",
			`{"action":"delete","reason_code":7}`, nil},
		"body unknown code": {http.MethodPost, "/api/v2/admin/auth_users/administration",
			`{"action":"delete","reason_code":"because"}`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			assertReasonRefusal(t, serveReason(t, router, tc.method, tc.path, tc.body, tc.header))
		})
	}
}

// The body fields carry the reason where there is a body, and win over the
// headers; the handler reads the body without them.
func TestTheBodyReasonWinsOverTheHeaders(t *testing.T) {
	recorder := &captureRecorder{}
	router, seen := newReasonRouter(t, recorder)
	body := `{"action":"set_admin_role","user_id":2,"role_names":[],"reason_code":"offboarding","reason":"left the team"}`
	rec := serveReason(t, router, http.MethodPost, "/api/v2/admin/auth_users/administration", body, http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident},
		adminaccess.ReasonHeader:     {"from%20the%20header"},
	})
	if rec.Code != reasonServedStatus || rec.Header().Get(adminaccess.ReasonMissingHeader) != "" {
		t.Fatalf("status = %d, missing = %q", rec.Code, rec.Header().Get(adminaccess.ReasonMissingHeader))
	}
	event := lastEvent(t, recorder)
	if event.ReasonCode != adminaccess.ReasonOffboarding || event.Reason != "left the team" {
		t.Errorf("audit reason = %q %q, want the body's", event.ReasonCode, event.Reason)
	}
	if want := `{"action":"set_admin_role","role_names":[],"user_id":2}`; seen.body != want {
		t.Errorf("the handler read %q, want %q", seen.body, want)
	}
	// A body without the fields falls back to the headers, field by field.
	rec = serveReason(t, router, http.MethodPost, "/api/v2/admin/auth_users/administration",
		`{"action":"delete","users":[3],"reason":"duplicate account"}`,
		http.Header{adminaccess.ReasonCodeHeader: {adminaccess.ReasonCorrection}})
	event = lastEvent(t, recorder)
	if rec.Code != reasonServedStatus || event.ReasonCode != adminaccess.ReasonCorrection || event.Reason != "duplicate account" {
		t.Errorf("mixed reason = %d %q %q, want the header's code and the body's text", rec.Code, event.ReasonCode, event.Reason)
	}
}

// A strict handler (a retention write refuses an unknown field) never sees
// the reason fields; a route whose own request has `reason` keeps that one
// and loses reason_code. A body reason over 2000 bytes is refused.
func TestTheHandlerReadsItsOwnBody(t *testing.T) {
	router, seen := newReasonRouter(t, &captureRecorder{})
	for _, tc := range []struct{ path, body, want string }{
		{"/api/v2/admin/retention/policies/conversations",
			`{"max_age_days":30,"min_age_days":0,"allow_project_override":true,"reason_code":"request","Reason":"ticket 7"}`,
			`{"allow_project_override":true,"max_age_days":30,"min_age_days":0}`},
		{"/api/v2/admin/legal_holds/1/release", `{"reason_code":"correction","reason":"case closed"}`, `{"reason":"case closed"}`},
		{"/api/v2/admin/legal_holds/1/release", `{"reason":"case closed"}`, `{"reason":"case closed"}`},
	} {
		method := http.MethodPut
		if strings.HasSuffix(tc.path, "/release") {
			method = http.MethodPost
		}
		if rec := serveReason(t, router, method, tc.path, tc.body, nil); rec.Code != reasonServedStatus || seen.body != tc.want {
			t.Errorf("%s %s = %d, handler read %q, want %q", method, tc.path, rec.Code, seen.body, tc.want)
		}
	}
	// The permission matrix save's keys are role names: the body is data, the
	// headers carry the reason, and a role called "reason" is no reason.
	matrix := `{"reason":["models.view"],"reason_code":["x"]}`
	rec := serveReason(t, router, http.MethodPut, "/api/v2/admin/user_project_permissions/administration", matrix,
		http.Header{adminaccess.ReasonCodeHeader: {adminaccess.ReasonRequest}})
	if rec.Code != reasonServedStatus || seen.body != matrix || rec.Header().Get(adminaccess.ReasonMissingHeader) != "" {
		t.Errorf("the matrix save = %d, handler read %q, missing %q", rec.Code, seen.body, rec.Header().Get(adminaccess.ReasonMissingHeader))
	}
	long := `{"reason_code":"other","reason":"` + strings.Repeat("x", adminaccess.MaxReasonBodyBytes+1) + `"}`
	assertReasonRefusal(t, serveReason(t, router, http.MethodPost, "/api/v2/admin/legal_holds/1/release", long, nil))
}

// A text without a code is recorded and still counts as missing.
func TestATextWithoutACodeIsMissing(t *testing.T) {
	recorder := &captureRecorder{}
	router, _ := newReasonRouter(t, recorder)
	rec := serveReason(t, router, http.MethodPost, "/api/v2/admin/legal_holds/1/release", `{"reason":"case closed"}`, nil)
	if rec.Code != reasonServedStatus || rec.Header().Get(adminaccess.ReasonMissingHeader) != "1" {
		t.Fatalf("status = %d, missing = %q, want %d and 1", rec.Code, rec.Header().Get(adminaccess.ReasonMissingHeader), reasonServedStatus)
	}
	event := lastEvent(t, recorder)
	if event.Reason != "case closed" || event.ReasonMissing == nil || !*event.ReasonMissing {
		t.Errorf("audit reason = %q, missing %v, want the text and true", event.Reason, event.ReasonMissing)
	}
}

func assertReasonRefusal(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != adminaccess.CodeReasonInvalid || body.Error == "" {
		t.Errorf("body = %s (%v), want code %s and a message", rec.Body, err, adminaccess.CodeReasonInvalid)
	}
}

// A control character in the text is refused: a NUL would fail the audit
// row's insert and lose the row of the very action the reason is for. Tab
// and line breaks are text.
func TestAReasonWithAControlCharacterIsRefused(t *testing.T) {
	recorder := &captureRecorder{}
	router, _ := newReasonRouter(t, recorder)
	assertReasonRefusal(t, serveReason(t, router, http.MethodDelete, brandingResetPath, "", http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident}, adminaccess.ReasonHeader: {"x%00y"}}))
	assertReasonRefusal(t, serveReason(t, router, http.MethodPost, "/api/v2/admin/legal_holds/1/release",
		`{"reason_code":"incident","reason":"x\u0000y"}`, nil))
	assertReasonRefusal(t, serveReason(t, router, http.MethodPost, "/api/v2/admin/legal_holds/1/release",
		`{"reason_code":"incident","reason":"bell\u0007"}`, nil))
	rec := serveReason(t, router, http.MethodDelete, brandingResetPath, "", http.Header{
		adminaccess.ReasonCodeHeader: {adminaccess.ReasonIncident}, adminaccess.ReasonHeader: {"line%0Anext%09tab"}})
	if rec.Code != reasonServedStatus || lastEvent(t, recorder).Reason != "line\nnext\ttab" {
		t.Errorf("a reason with a line break = %d %q, want it served and kept", rec.Code, lastEvent(t, recorder).Reason)
	}
}

// An empty body field (a form default) falls back to its header.
func TestAnEmptyBodyFieldFallsBackToTheHeader(t *testing.T) {
	recorder := &captureRecorder{}
	router, _ := newReasonRouter(t, recorder)
	rec := serveReason(t, router, http.MethodPost, "/api/v2/admin/auth_users/administration",
		`{"action":"delete","users":[5],"reason_code":"","reason":" "}`,
		http.Header{adminaccess.ReasonCodeHeader: {adminaccess.ReasonOffboarding}, adminaccess.ReasonHeader: {"left"}})
	event := lastEvent(t, recorder)
	if rec.Code != reasonServedStatus || rec.Header().Get(adminaccess.ReasonMissingHeader) != "" ||
		event.ReasonCode != adminaccess.ReasonOffboarding || event.Reason != "left" {
		t.Errorf("= %d, missing %q, audit %q %q, want the header's reason", rec.Code,
			rec.Header().Get(adminaccess.ReasonMissingHeader), event.ReasonCode, event.Reason)
	}
}

// A request the gate or the handler refuses is not an action served without
// a reason: no header, no reason_missing.
func TestARefusedRequestIsNotFlagged(t *testing.T) {
	recorder := &captureRecorder{}
	check := NewReasonCheck()
	r := chi.NewRouter()
	r.Use(Audit(recorder))
	r.Use(check.Middleware)
	r.Delete(brandingResetPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	check.Mark(r)
	rec := serveReason(t, r, http.MethodDelete, brandingResetPath, "", nil)
	if rec.Code != http.StatusForbidden || rec.Header().Get(adminaccess.ReasonMissingHeader) != "" {
		t.Errorf("= %d with %q, want 403 and no header", rec.Code, rec.Header().Get(adminaccess.ReasonMissingHeader))
	}
	if event := lastEvent(t, recorder); event.ReasonMissing != nil {
		t.Errorf("reason_missing = %v on a refused request, want NULL", *event.ReasonMissing)
	}
}

// An outcome the body decides is resolved: an unsuspend and a scoped
// suspension ask for no reason, a suspend and a platform stop do, and a body
// that does not say keeps the closed failure.
func TestTheBodyDecidesTheOutcome(t *testing.T) {
	router, _ := newReasonRouter(t, &captureRecorder{})
	for _, tc := range []struct {
		method, path, body string
		flagged            bool
	}{
		{http.MethodPut, "/api/v2/admin/user_suspend/administration/7", `{"suspended":false}`, false},
		{http.MethodPut, "/api/v2/admin/user_suspend/administration/7", `{"suspended":true}`, true},
		{http.MethodPut, "/api/v2/admin/user_suspend/administration/7", `{"suspended":false,"Suspended":true}`, true},
		{http.MethodPut, "/api/v2/admin/user_suspend/administration/7", `{}`, true},
		{http.MethodPost, "/api/v2/admin/execution_suspensions", `{"scope":"project","project_id":1,"reason":"x"}`, false},
		{http.MethodPost, "/api/v2/admin/execution_suspensions", `{"scope":"platform","reason":"x"}`, true},
	} {
		rec := serveReason(t, router, tc.method, tc.path, tc.body, nil)
		if got := rec.Header().Get(adminaccess.ReasonMissingHeader) == "1"; got != tc.flagged || rec.Code != reasonServedStatus {
			t.Errorf("%s %s %s = %d flagged %v, want flagged %v", tc.method, tc.path, tc.body, rec.Code, got, tc.flagged)
		}
	}
}
