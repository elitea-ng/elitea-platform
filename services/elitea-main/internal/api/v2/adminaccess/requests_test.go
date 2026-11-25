package adminaccess_test

// The elevation request routes (ADR-0059 R2.2b, issue 2596), driven behind
// the gates the production router puts in front of them, with a fake ledger:
// the credential check first, the body rules, the duration cap, the mapping
// of the ledger's refusals, and what each read returns to whom.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

var requestClock = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// The people of the directory: 1 requests (emergency_access), 2 approves
// (admin), 3 holds super_admin, 4 is a security auditor (review and
// roles.view, no approve), 5 holds llm_manager (none of them).
const (
	requester  int64 = 1
	approver   int64 = 2
	superAdmin int64 = 3
	auditor    int64 = 4
	bystander  int64 = 5
)

func requestDirectory() *directory {
	d := newDirectory()
	d.users[requester] = []string{registry.RoleEmergencyAccess}
	d.users[approver] = []string{registry.RoleAdmin}
	d.users[superAdmin] = []string{registry.RoleSuperAdmin}
	d.users[auditor] = []string{registry.RoleSecurityAuditor}
	d.users[bystander] = []string{registry.RoleLLMManager}
	return d
}

// fakeElevations is the ledger: it records what it was asked and answers
// what the test scripted.
type fakeElevations struct {
	requests  []registry.ElevationRequest
	answers   []registry.ElevationAnswer
	record    registry.ElevationRecord
	err       error
	latest    *registry.ElevationRecord
	pending   []registry.ElevationRecord
	history   []registry.ElevationRecord
	reviewers map[int64]bool
}

func (f *fakeElevations) Request(_ context.Context, r registry.ElevationRequest) (registry.ElevationRequested, error) {
	f.requests = append(f.requests, r)
	if f.err != nil {
		return registry.ElevationRequested{}, f.err
	}
	return registry.ElevationRequested{Elevation: registry.ElevationRecord{
		ID: uuid.New(), UserID: r.UserID, RequesterUserID: r.UserID, BrowserSessionRef: r.BrowserSessionRef,
		Path: registry.ElevationPathApproved, ReasonCode: r.ReasonCode, Reason: r.Reason, Ticket: r.Ticket,
		Duration: r.Duration, RequestedAt: r.At,
	}, Notices: 2}, nil
}

func (f *fakeElevations) Answer(_ context.Context, a registry.ElevationAnswer) (registry.ElevationRecord, error) {
	f.answers = append(f.answers, a)
	return f.record, f.err
}

func (f *fakeElevations) LatestRequest(context.Context, int64) (registry.ElevationRecord, bool, error) {
	if f.latest == nil {
		return registry.ElevationRecord{}, false, nil
	}
	return *f.latest, true, nil
}

func (f *fakeElevations) Pending(context.Context, time.Time, time.Duration, int) ([]registry.ElevationRecord, error) {
	return f.pending, nil
}

func (f *fakeElevations) History(_ context.Context, limit, offset int) ([]registry.ElevationRecord, int, error) {
	return f.history, len(f.history), nil
}

func (f *fakeElevations) People(_ context.Context, ids []int64) (map[int64]console.Person, error) {
	people := map[int64]console.Person{}
	for _, id := range ids {
		people[id] = console.Person{ID: id, Name: "Person " + itoa(id), Email: "p" + itoa(id) + "@example.com"}
	}
	return people, nil
}

func (f *fakeElevations) HoldsStanding(_ context.Context, userID int64, _ string) (bool, error) {
	return f.reviewers[userID], nil
}

func breakGlassSettings(context.Context) (platformconfig.BreakGlassSettings, error) {
	return platformconfig.DefaultBreakGlassSettings(), nil
}

// requestRouter mounts the routes as the production router does: the
// credential check, then the central gates on the directory.
func requestRouter(store console.ElevationStore, d *directory, now time.Time) http.Handler {
	central := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(d, auth.PermissionModeAdministration, permission)
	}
	r := chi.NewRouter()
	console.NewElevationsHandler(store, breakGlassSettings, d).WithClock(func() time.Time { return now }).
		Register(r, console.ElevationGates{
			Request: central(registry.AccessBreakglassRequest),
			Approve: central(registry.AccessBreakglassApprove),
			History: central(registry.AccessRolesView),
		})
	return r
}

// browser is a standing browser session of the person.
func browser(id int64) auth.User {
	return auth.User{ID: itoa(id), UserID: itoa(id), CredentialKind: auth.CredentialKindSession, BrowserSessionRef: 100 + id}
}

func call(t *testing.T, h http.Handler, user auth.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req = req.WithContext(auth.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func refusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code
}

const validRequest = `{"reason_code":"incident","reason":"the provider is down","ticket":"INC-1"}`

func approvePath(id uuid.UUID) string { return "/access/elevations/" + id.String() + "/approve" }
func denyPath(id uuid.UUID) string    { return "/access/elevations/" + id.String() + "/deny" }

// A request without a reason, or with an unknown reason code, answers 400
// and never reaches the ledger.
func TestARequestWithoutAReasonIsRefused(t *testing.T) {
	t.Parallel()
	store := &fakeElevations{}
	h := requestRouter(store, requestDirectory(), requestClock)
	for name, body := range map[string]string{
		"no reason":     `{"reason_code":"incident"}`,
		"blank reason":  `{"reason_code":"incident","reason":"   "}`,
		"unknown code":  `{"reason_code":"curiosity","reason":"r"}`,
		"unknown field": `{"reason_code":"incident","reason":"r","level":"super_admin"}`,
		"zero duration": `{"reason_code":"incident","reason":"r","duration_minutes":0}`,
		"not json":      `reason`,
	} {
		rec := call(t, h, browser(requester), http.MethodPost, "/access/elevations", body)
		if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != console.CodeElevationRequestInvalid {
			t.Errorf("%s: %d %s, want 400 %s", name, rec.Code, rec.Body, console.CodeElevationRequestInvalid)
		}
	}
	if len(store.requests) != 0 {
		t.Fatalf("a refused body reached the ledger %d times", len(store.requests))
	}
}

// A duration over the section's cap is refused with its own code; an absent
// duration takes the section's default; a valid one is passed on with the
// cap and the effective approval policy.
func TestTheDurationIsTheDefaultOrUnderTheCap(t *testing.T) {
	t.Parallel()
	store := &fakeElevations{}
	h := requestRouter(store, requestDirectory(), requestClock)
	defaults := platformconfig.DefaultBreakGlassSettings()

	over := `{"reason_code":"incident","reason":"r","duration_minutes":` + itoa(int64(defaults.OnlineMax/time.Minute)+1) + `}`
	if rec := call(t, h, browser(requester), http.MethodPost, "/access/elevations", over); rec.Code != http.StatusBadRequest ||
		refusalCode(t, rec) != console.CodeElevationDurationOverCap {
		t.Fatalf("over the cap: %d %s", rec.Code, rec.Body)
	}
	rec := call(t, h, browser(requester), http.MethodPost, "/access/elevations", validRequest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a valid request: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, browser(requester), http.MethodPost, "/access/elevations",
		`{"reason_code":"incident","reason":"r","duration_minutes":90}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a valid duration: %d %s", rec.Code, rec.Body)
	}
	if len(store.requests) != 2 {
		t.Fatalf("%d requests reached the ledger, want 2", len(store.requests))
	}
	first, second := store.requests[0], store.requests[1]
	if first.Duration != defaults.OnlineDefault || second.Duration != 90*time.Minute ||
		first.Cap != defaults.OnlineMax || !first.ApprovalRequired || first.RequestLifetime != defaults.RequestLifetime ||
		first.UserID != requester || first.BrowserSessionRef != 100+requester || first.Ticket != "INC-1" {
		t.Fatalf("requests = %+v", store.requests)
	}
	var view struct {
		State           string `json:"state"`
		DurationMinutes int64  `json:"duration_minutes"`
		TimeLeftSeconds int64  `json:"time_left_seconds"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.State != registry.ElevationStateRequested || view.DurationMinutes != 90 ||
		view.TimeLeftSeconds != int64(defaults.RequestLifetime/time.Second) || view.Reason != "r" {
		t.Fatalf("view = %+v", view)
	}
}

// Only a browser session reaches a gate: a personal access token (of a
// standing approver), a native session, a run token, a system token, a
// sealed session and a session without a server-side row answer 403 with
// elevation_credential_refused on every route, before any permission.
func TestOnlyABrowserSessionCallsAnElevationRoute(t *testing.T) {
	t.Parallel()
	store := &fakeElevations{}
	h := requestRouter(store, requestDirectory(), requestClock)
	id := uuid.New()
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/access/elevations", validRequest},
		{http.MethodPost, approvePath(id), ``},
		{http.MethodPost, denyPath(id), `{"reason_code":"other"}`},
		{http.MethodGet, "/access/elevations/mine", ``},
		{http.MethodGet, "/access/elevations/pending", ``},
		{http.MethodGet, "/access/elevations", ``},
	}
	for name, user := range map[string]auth.User{
		"token":         {ID: "2", UserID: "2", TokenID: "77", CredentialKind: auth.CredentialKindToken},
		"native":        {ID: "2", UserID: "2", CredentialKind: auth.CredentialKindNative},
		"run":           {ID: "2", UserID: "2", CredentialKind: auth.CredentialKindRun},
		"system":        {ID: "2", UserID: "2", CredentialKind: auth.CredentialKindSystem},
		"sealed":        {ID: "2", UserID: "2", CredentialKind: auth.CredentialKindSealed, BrowserSessionRef: 9},
		"legacy cookie": {ID: "2", UserID: "2", CredentialKind: auth.CredentialKindSession},
		"no kind":       {ID: "2", UserID: "2"},
	} {
		for _, route := range routes {
			rec := call(t, h, user, route.method, route.path, route.body)
			if rec.Code != http.StatusForbidden || refusalCode(t, rec) != console.CodeElevationCredentialRefused {
				t.Errorf("%s %s %s: %d %s, want 403 %s", name, route.method, route.path, rec.Code, rec.Body,
					console.CodeElevationCredentialRefused)
			}
		}
	}
	if len(store.requests)+len(store.answers) != 0 {
		t.Fatal("a refused credential reached the ledger")
	}
	// A Form session is a browser session.
	form := browser(requester)
	form.CredentialKind = auth.CredentialKindForm
	if rec := call(t, h, form, http.MethodPost, "/access/elevations", validRequest); rec.Code != http.StatusCreated {
		t.Fatalf("a Form session: %d %s", rec.Code, rec.Body)
	}
}

// The gates: the request needs access.breakglass.request, the answers and
// the queue access.breakglass.approve, the history access.roles.view.
func TestEachRouteHasItsGate(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	for _, c := range []struct {
		method, path, body string
		allowed, refused   int64
	}{
		{http.MethodPost, "/access/elevations", validRequest, requester, bystander},
		{http.MethodGet, "/access/elevations/mine", ``, requester, bystander},
		{http.MethodPost, approvePath(id), ``, approver, requester},
		{http.MethodPost, denyPath(id), `{"reason_code":"other"}`, approver, auditor},
		{http.MethodGet, "/access/elevations/pending", ``, approver, auditor},
		{http.MethodGet, "/access/elevations", ``, auditor, requester},
	} {
		store := &fakeElevations{record: registry.ElevationRecord{ID: id, Path: registry.ElevationPathApproved, RequestedAt: requestClock}}
		h := requestRouter(store, requestDirectory(), requestClock)
		if rec := call(t, h, browser(c.refused), c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s by %d: %d, want 403", c.method, c.path, c.refused, rec.Code)
		}
		if rec := call(t, h, browser(c.allowed), c.method, c.path, c.body); rec.Code/100 != 2 {
			t.Errorf("%s %s by %d: %d %s, want 2xx", c.method, c.path, c.allowed, rec.Code, rec.Body)
		}
	}
}

// The ledger's refusals of an answer reach the caller with their status and
// code: the own request and an approver under an elevation 403, a decided
// request 409, a request past its lifetime 410, an unknown one 404.
func TestTheLedgersRefusalsOfAnAnswer(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		registry.ErrElevationOwnRequest:          {http.StatusForbidden, console.CodeElevationOwnRequest},
		registry.ErrElevationApproverElevated:    {http.StatusForbidden, console.CodeElevationApproverElevated},
		registry.ErrElevationApproverNotEligible: {http.StatusForbidden, console.CodeElevationApproverNotEligible},
		registry.ErrElevationRequestDecided:      {http.StatusConflict, console.CodeElevationRequestDecided},
		registry.ErrElevationRequestExpired:      {http.StatusGone, console.CodeElevationRequestExpired},
		registry.ErrElevationNotFound:            {http.StatusNotFound, console.CodeElevationNotFound},
	} {
		store := &fakeElevations{err: err}
		h := requestRouter(store, requestDirectory(), requestClock)
		rec := call(t, h, browser(approver), http.MethodPost, approvePath(uuid.New()), ``)
		if rec.Code != want.status || refusalCode(t, rec) != want.code {
			t.Errorf("%v: %d %s, want %d %s", err, rec.Code, rec.Body, want.status, want.code)
		}
	}
	// The request route maps its own.
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		registry.ErrElevationRequestPending: {http.StatusConflict, console.CodeElevationRequestPending},
		registry.ErrElevationRequester:      {http.StatusForbidden, console.CodeElevationRequesterNotEligible},
		registry.ErrElevationSession:        {http.StatusForbidden, console.CodeElevationCredentialRefused},
	} {
		h := requestRouter(&fakeElevations{err: err}, requestDirectory(), requestClock)
		rec := call(t, h, browser(requester), http.MethodPost, "/access/elevations", validRequest)
		if rec.Code != want.status || refusalCode(t, rec) != want.code {
			t.Errorf("%v: %d %s, want %d %s", err, rec.Code, rec.Body, want.status, want.code)
		}
	}
}

// A denial needs a known reason code and passes it on; the answer is the
// caller's, with the session that answered.
func TestADenialCarriesItsReasonCode(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	store := &fakeElevations{record: registry.ElevationRecord{
		ID: id, Path: registry.ElevationPathApproved, Decision: registry.ElevationDecisionDenied,
		DenialReasonCode: registry.ElevationDenialNotJustified, ApproverUserID: approver, UserID: requester,
		RequestedAt: requestClock.Add(-time.Minute), DecidedAt: requestClock,
	}}
	h := requestRouter(store, requestDirectory(), requestClock)
	if rec := call(t, h, browser(approver), http.MethodPost, denyPath(id), `{"reason_code":"because"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown denial code: %d", rec.Code)
	}
	rec := call(t, h, browser(approver), http.MethodPost, denyPath(id), `{"reason_code":"not_justified"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"denial_reason_code":"not_justified"`) ||
		!strings.Contains(rec.Body.String(), `"state":"denied"`) {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body)
	}
	if len(store.answers) != 1 || store.answers[0].Approve || store.answers[0].DenialReasonCode != "not_justified" ||
		store.answers[0].ApproverUserID != approver || store.answers[0].ApproverSessionRef != 100+approver {
		t.Fatalf("answers = %+v", store.answers)
	}
}

// The own-request route returns the state, the time left and the roles that
// can approve (never the unassignable `system` role); an expired request
// reads as expired, with no time left.
func TestTheOwnRequestRoute(t *testing.T) {
	t.Parallel()
	pending := registry.ElevationRecord{
		ID: uuid.New(), UserID: requester, RequesterUserID: requester, Path: registry.ElevationPathApproved,
		ReasonCode: registry.ElevationReasonIncident, Reason: "r", Duration: time.Hour,
		RequestedAt: requestClock.Add(-10 * time.Minute),
	}
	store := &fakeElevations{latest: &pending}
	d := requestDirectory()
	d.builtIn["system"] = string(registry.MarkerUnassignable)
	d.roles["system"] = []string{registry.AccessBreakglassApprove}
	h := requestRouter(store, d, requestClock)
	var body struct {
		Request *struct {
			State           string `json:"state"`
			TimeLeftSeconds int64  `json:"time_left_seconds"`
			RequestExpires  string `json:"request_expires_at"`
		} `json:"request"`
		ApprovingRoles []string `json:"approving_roles"`
	}
	rec := call(t, h, browser(requester), http.MethodGet, "/access/elevations/mine", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("mine: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Request == nil || body.Request.State != registry.ElevationStateRequested ||
		body.Request.TimeLeftSeconds != int64((20*time.Minute)/time.Second) || body.Request.RequestExpires == "" {
		t.Fatalf("own request = %s", rec.Body)
	}
	if len(body.ApprovingRoles) == 0 {
		t.Fatal("no approving role")
	}
	for _, role := range body.ApprovingRoles {
		if role == "system" {
			t.Fatalf("the unassignable role is offered as an approver: %v", body.ApprovingRoles)
		}
	}

	// Past its lifetime the same row reads as expired.
	late := requestRouter(store, d, requestClock.Add(time.Hour))
	rec = call(t, late, browser(requester), http.MethodGet, "/access/elevations/mine", ``)
	if !strings.Contains(rec.Body.String(), `"state":"request_expired"`) || !strings.Contains(rec.Body.String(), `"time_left_seconds":0`) {
		t.Fatalf("an expired own request: %s", rec.Body)
	}
	// Nobody's request is null.
	rec = call(t, requestRouter(&fakeElevations{}, d, requestClock), browser(requester), http.MethodGet, "/access/elevations/mine", ``)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"request":null`) {
		t.Fatalf("no request: %d %s", rec.Code, rec.Body)
	}
}

// The queue returns the pending requests and their count, never the
// caller's own.
func TestTheQueueLeavesOutTheCallersOwnRequest(t *testing.T) {
	t.Parallel()
	own := registry.ElevationRecord{ID: uuid.New(), UserID: approver, RequesterUserID: approver,
		Path: registry.ElevationPathApproved, RequestedAt: requestClock.Add(-time.Minute)}
	other := registry.ElevationRecord{ID: uuid.New(), UserID: requester, RequesterUserID: requester,
		Path: registry.ElevationPathApproved, Reason: "the reason", RequestedAt: requestClock.Add(-2 * time.Minute)}
	h := requestRouter(&fakeElevations{pending: []registry.ElevationRecord{own, other}}, requestDirectory(), requestClock)
	rec := call(t, h, browser(approver), http.MethodGet, "/access/elevations/pending", ``)
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || len(body.Rows) != 1 || body.Rows[0].ID != other.ID.String() || body.Rows[0].Reason != "the reason" {
		t.Fatalf("queue = %s", rec.Body)
	}
}

// The history shows the reason text only to a holder of
// access.breakglass.review (standing), the requester, the person and the
// approver; the reason code to everyone it answers.
func TestTheHistoryHidesTheReasonText(t *testing.T) {
	t.Parallel()
	row := registry.ElevationRecord{ID: uuid.New(), UserID: requester, RequesterUserID: requester, ApproverUserID: approver,
		Path: registry.ElevationPathApproved, ReasonCode: registry.ElevationReasonIncident, Reason: "secret reason",
		Decision: registry.ElevationDecisionApproved, Level: registry.ElevationLevelAdmin,
		RequestedAt: requestClock.Add(-time.Hour), DecidedAt: requestClock.Add(-50 * time.Minute)}
	d := requestDirectory()
	d.roles["viewer_role"] = []string{registry.AccessRolesView}
	const viewer int64 = 6
	d.users[viewer] = []string{"viewer_role"}
	store := &fakeElevations{history: []registry.ElevationRecord{row}, reviewers: map[int64]bool{auditor: true}}
	h := requestRouter(store, d, requestClock)
	for user, wantReason := range map[int64]bool{auditor: true, viewer: false, approver: true} {
		rec := call(t, h, browser(user), http.MethodGet, "/access/elevations?limit=10", ``)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reason_code":"incident"`) {
			t.Fatalf("history for %d: %d %s", user, rec.Code, rec.Body)
		}
		if got := strings.Contains(rec.Body.String(), "secret reason"); got != wantReason {
			t.Errorf("history for %d shows the reason text: %v, want %v (%s)", user, got, wantReason, rec.Body)
		}
	}
	if rec := call(t, h, browser(auditor), http.MethodGet, "/access/elevations?limit=500", ``); rec.Code != http.StatusBadRequest {
		t.Fatalf("a limit over the bound: %d", rec.Code)
	}
}
