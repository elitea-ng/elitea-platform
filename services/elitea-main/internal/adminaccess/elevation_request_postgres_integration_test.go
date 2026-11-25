package adminaccess_test

// The elevation requests on the real schema (ADR-0059 decision 5, "Path 1"
// steps 1 to 3; story R2.2b, issue 2596): a request writes its row, its
// `requested` event and one notice row per approver IN ONE TRANSACTION; an
// answer writes its decision and its event; the level is the approver's
// standing level; a late answer records the expiry; nobody answers their own
// request, and nobody under an elevation answers at all.

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const requestLifetime = 30 * time.Minute

// seededRequest is a valid request of a fresh holder of emergency_access in
// a live session of theirs, with approval required.
func seededRequest(t *testing.T, pool *pgxpool.Pool) adminaccess.ElevationRequest {
	t.Helper()
	userID := seedPerson(t, pool, adminaccess.RoleEmergencyAccess)
	return adminaccess.ElevationRequest{
		UserID:            userID,
		BrowserSessionRef: seedBrowserSession(t, pool, userID),
		ReasonCode:        adminaccess.ElevationReasonIncident,
		Reason:            "the provider is down",
		Ticket:            "INC-42",
		Duration:          time.Hour,
		Cap:               4 * time.Hour,
		ApprovalRequired:  true,
		RequestLifetime:   requestLifetime,
		At:                elevationClock.Add(time.Minute),
	}
}

func requestElevation(t *testing.T, pool *pgxpool.Pool, request adminaccess.ElevationRequest) (adminaccess.ElevationRequested, error) {
	t.Helper()
	var written adminaccess.ElevationRequested
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		written, err = adminaccess.RequestElevation(t.Context(), tx, auditAppender(t), request)
		return err
	})
	return written, err
}

func mustRequest(t *testing.T, pool *pgxpool.Pool, request adminaccess.ElevationRequest) adminaccess.ElevationRequested {
	t.Helper()
	written, err := requestElevation(t, pool, request)
	if err != nil {
		t.Fatalf("request an elevation: %v", err)
	}
	return written
}

// answerRequest commits as the route's store does: a refusal rolls back,
// except the recorded expiry of a late answer.
func answerRequest(t *testing.T, pool *pgxpool.Pool, answer adminaccess.ElevationAnswer) (adminaccess.ElevationRecord, error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	record, answerErr := adminaccess.AnswerElevationRequest(t.Context(), tx, auditAppender(t), answer)
	if answerErr != nil && !errors.Is(answerErr, adminaccess.ErrElevationRequestExpired) {
		return record, answerErr
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return record, answerErr
}

func approval(id uuid.UUID, approver int64, at time.Time) adminaccess.ElevationAnswer {
	return adminaccess.ElevationAnswer{
		ID: id, ApproverUserID: approver, ApproverSessionRef: 99, Approve: true,
		RequestLifetime: requestLifetime, At: at,
	}
}

func eventActions(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	var actions []string
	for _, event := range elevationEvents(t, pool, id.String()) {
		actions = append(actions, event.action)
	}
	return actions
}

type requestNoticeRow struct {
	kind      string
	recipient int64
}

func requestNotices(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []requestNoticeRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT kind, recipient_user_id FROM elitea_auth.admin_access_notices
WHERE elevation_id = $1 ORDER BY recipient_user_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (requestNoticeRow, error) {
		var r requestNoticeRow
		return r, row.Scan(&r.kind, &r.recipient)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A request writes its row, its `requested` event and one request notice
// per standing approver other than the requester, in its transaction.
func TestARequestWritesItsRowItsEventAndOneNoticePerApprover(t *testing.T) {
	pool := newNoticeDatabase(t)
	admin := seedPerson(t, pool, adminaccess.RoleAdmin)
	superAdmin := seedPerson(t, pool, adminaccess.RoleSuperAdmin)
	custom := seedPerson(t, pool, seedCustomRole(t, pool, adminaccess.AccessBreakglassApprove))
	seedPerson(t, pool, adminaccess.RoleSecurityAuditor) // review, not approve
	request := seededRequest(t, pool)

	written := mustRequest(t, pool, request)
	e := written.Elevation
	if e.Path != adminaccess.ElevationPathApproved || e.Decision != "" || e.Level != "" || e.ApproverUserID != 0 ||
		e.Duration != time.Hour || e.UserID != request.UserID || e.RequesterUserID != request.UserID ||
		e.BrowserSessionRef != request.BrowserSessionRef || e.Ticket != "INC-42" || !e.StartedAt.IsZero() {
		t.Fatalf("request row = %+v", e)
	}
	if got := adminaccess.RequestStateAt(e, request.At, requestLifetime); got != adminaccess.ElevationStateRequested {
		t.Fatalf("state = %s", got)
	}
	if !slices.Equal(eventActions(t, pool, e.ID), []string{string(securityaudit.ElevationRequested)}) {
		t.Fatalf("events = %v", eventActions(t, pool, e.ID))
	}
	want := []requestNoticeRow{{"request", admin}, {"request", superAdmin}, {"request", custom}}
	slices.SortFunc(want, func(a, b requestNoticeRow) int { return int(a.recipient - b.recipient) })
	if got := requestNotices(t, pool, e.ID); !slices.Equal(got, want) || written.Notices != len(want) {
		t.Fatalf("notice rows = %v (reported %d), want %v", got, written.Notices, want)
	}

	// A second request of the person while the first waits is refused, and
	// writes nothing.
	again := request
	again.At = request.At.Add(time.Minute)
	if _, err := requestElevation(t, pool, again); !errors.Is(err, adminaccess.ErrElevationRequestPending) {
		t.Fatalf("a second pending request: %v", err)
	}
	if n := countElevationsOf(t, pool, request.UserID); n != 1 {
		t.Fatalf("%d rows of the person, want 1", n)
	}
	// After the lifetime, the person may ask again.
	again.At = request.At.Add(requestLifetime + time.Minute)
	mustRequest(t, pool, again)
}

// The request is refused, with nothing written, to a person without a
// standing access.breakglass.request, to a session of another person, and to
// a suspended account. An elevation never counts towards the permission.
func TestARequestNeedsAStandingRequesterInTheirOwnSession(t *testing.T) {
	pool := newElevationDatabase(t)
	for name, c := range map[string]struct {
		change func(*adminaccess.ElevationRequest)
		want   error
	}{
		"no standing permission": {func(r *adminaccess.ElevationRequest) {
			r.UserID = seedPerson(t, pool, adminaccess.RoleLLMManager)
			r.BrowserSessionRef = seedBrowserSession(t, pool, r.UserID)
		}, adminaccess.ErrElevationRequester},
		"another person's session": {func(r *adminaccess.ElevationRequest) {
			r.BrowserSessionRef = seedBrowserSession(t, pool, seedPerson(t, pool))
		}, adminaccess.ErrElevationSession},
		"suspended": {func(r *adminaccess.ElevationRequest) {
			if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, r.UserID); err != nil {
				t.Fatal(err)
			}
		}, adminaccess.ErrElevationPerson},
		"over the cap": {func(r *adminaccess.ElevationRequest) { r.Duration = 5 * time.Hour }, adminaccess.ErrElevationDurationOverCap},
	} {
		t.Run(name, func(t *testing.T) {
			request := seededRequest(t, pool)
			c.change(&request)
			if _, err := requestElevation(t, pool, request); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if n := countElevationsOf(t, pool, request.UserID); n != 0 {
				t.Fatalf("a refused request wrote %d rows", n)
			}
		})
	}
}

// The level of an approval is the approver's standing level: admin for a
// holder of approve without super_admin, super_admin for a standing
// super_admin. The answer writes its event; a denial records its reason code.
func TestAnApprovalTakesTheApproversStandingLevel(t *testing.T) {
	pool := newNoticeDatabase(t)
	admin := seedPerson(t, pool, adminaccess.RoleAdmin)
	superAdmin := seedPerson(t, pool, adminaccess.RoleSuperAdmin)

	for approver, level := range map[int64]string{admin: adminaccess.ElevationLevelAdmin, superAdmin: adminaccess.ElevationLevelSuperAdmin} {
		request := mustRequest(t, pool, seededRequest(t, pool)).Elevation
		at := request.RequestedAt.Add(5 * time.Minute)
		approved, err := answerRequest(t, pool, approval(request.ID, approver, at))
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		if approved.Decision != adminaccess.ElevationDecisionApproved || approved.Level != level ||
			approved.ApproverUserID != approver || !approved.DecidedAt.Equal(at) || !approved.StartedAt.IsZero() {
			t.Fatalf("approved by %d = %+v, want level %s", approver, approved, level)
		}
		if got := eventActions(t, pool, request.ID); !slices.Equal(got,
			[]string{string(securityaudit.ElevationRequested), string(securityaudit.ElevationApproved)}) {
			t.Fatalf("events = %v", got)
		}
		// A second answer is refused.
		if _, err := answerRequest(t, pool, approval(request.ID, approver, at.Add(time.Minute))); !errors.Is(err, adminaccess.ErrElevationRequestDecided) {
			t.Fatalf("a second answer: %v", err)
		}
	}

	request := mustRequest(t, pool, seededRequest(t, pool)).Elevation
	deny := approval(request.ID, admin, request.RequestedAt.Add(time.Minute))
	deny.Approve, deny.DenialReasonCode = false, adminaccess.ElevationDenialStandingAccessSuffices
	denied, err := answerRequest(t, pool, deny)
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if denied.Decision != adminaccess.ElevationDecisionDenied || denied.DenialReasonCode != deny.DenialReasonCode ||
		denied.Level != "" || denied.ApproverUserID != admin {
		t.Fatalf("denied = %+v", denied)
	}
	events := elevationEvents(t, pool, request.ID.String())
	if len(events) != 2 || events[1].action != string(securityaudit.ElevationDenied) ||
		!strings.Contains(events[1].raw, "denial_reason:"+deny.DenialReasonCode) {
		t.Fatalf("deny events = %+v", events)
	}
}

// With approval off the request is approved at once by the system, at the
// admin level on the path `self`, with an `approved` event that says nobody
// approved, and no request notice (nobody has to answer).
func TestWithApprovalOffTheSystemApproves(t *testing.T) {
	pool := newNoticeDatabase(t)
	seedPerson(t, pool, adminaccess.RoleAdmin)
	request := seededRequest(t, pool)
	request.ApprovalRequired = false
	written := mustRequest(t, pool, request)
	e := written.Elevation
	if e.Path != adminaccess.ElevationPathSelf || e.Decision != adminaccess.ElevationDecisionApproved ||
		e.Level != adminaccess.ElevationLevelAdmin || e.ApproverUserID != 0 || !e.DecidedAt.Equal(request.At) {
		t.Fatalf("approval-off row = %+v", e)
	}
	if written.Notices != 0 || len(requestNotices(t, pool, e.ID)) != 0 {
		t.Fatalf("approval off wrote %d notices", written.Notices)
	}
	events := elevationEvents(t, pool, e.ID.String())
	if len(events) != 2 || events[1].action != string(securityaudit.ElevationApproved) ||
		!strings.Contains(events[1].raw, adminaccess.ApproverNone) || !strings.Contains(events[1].raw, "source:system") {
		t.Fatalf("approval-off events = %+v", events)
	}
	// Nobody answers a request that approval-off decided.
	admin := seedPerson(t, pool, adminaccess.RoleAdmin)
	if _, err := answerRequest(t, pool, approval(e.ID, admin, request.At.Add(time.Minute))); !errors.Is(err, adminaccess.ErrElevationRequestDecided) {
		t.Fatalf("an answer to an approval-off request: %v", err)
	}
}

// An answer after the request lifetime is refused, and the expiry is
// recorded at once, with its event: the request then reads as expired to
// every reader, and a later answer gets the same refusal.
func TestALateAnswerRecordsTheExpiry(t *testing.T) {
	pool := newElevationDatabase(t)
	admin := seedPerson(t, pool, adminaccess.RoleAdmin)
	request := mustRequest(t, pool, seededRequest(t, pool)).Elevation
	late := request.RequestedAt.Add(requestLifetime + time.Second)
	record, err := answerRequest(t, pool, approval(request.ID, admin, late))
	if !errors.Is(err, adminaccess.ErrElevationRequestExpired) {
		t.Fatalf("a late answer: %v", err)
	}
	if record.Decision != adminaccess.ElevationDecisionRequestExpired ||
		!record.DecidedAt.Equal(request.RequestedAt.Add(requestLifetime)) || record.ApproverUserID != 0 {
		t.Fatalf("the expiry = %+v", record)
	}
	if stored := readElevation(t, pool, request.ID); stored.Decision != adminaccess.ElevationDecisionRequestExpired {
		t.Fatalf("the expiry was not committed: %+v", stored)
	}
	if got := eventActions(t, pool, request.ID); !slices.Equal(got,
		[]string{string(securityaudit.ElevationRequested), string(securityaudit.ElevationRequestExpired)}) {
		t.Fatalf("events = %v", got)
	}
	if _, err := answerRequest(t, pool, approval(request.ID, admin, late.Add(time.Hour))); !errors.Is(err, adminaccess.ErrElevationRequestExpired) {
		t.Fatalf("an answer to a recorded expiry: %v", err)
	}
	// The reads leave it out of the queue and show it as expired.
	pending, err := adminaccess.PendingElevationRequests(t.Context(), pool, late, requestLifetime, 50)
	if err != nil || slices.ContainsFunc(pending, func(e adminaccess.ElevationRecord) bool { return e.ID == request.ID }) {
		t.Fatalf("the queue holds the expired request (%v)", err)
	}
	latest, found, err := adminaccess.LatestElevationRequestOf(t.Context(), pool, request.UserID)
	if err != nil || !found || adminaccess.RequestStateAt(latest, late, requestLifetime) != adminaccess.ElevationStateRequestExpired {
		t.Fatalf("the latest request = %+v, %v, %v", latest, found, err)
	}
}

// Nobody answers their own request; a person under an active elevation, in
// any session, answers nothing; an answer needs a standing approve. Each
// refusal writes nothing.
func TestWhoMayAnswerARequest(t *testing.T) {
	pool := newElevationDatabase(t)
	auditor := seedPerson(t, pool, adminaccess.RoleSecurityAuditor)
	elevatedApprover := seedPerson(t, pool, adminaccess.RoleAdmin)
	start := validElevationStart()
	start.UserID = elevatedApprover
	start.ApproverUserID = seedPerson(t, pool, adminaccess.RoleAdmin)
	start.BrowserSessionRef = seedBrowserSession(t, pool, elevatedApprover)
	startElevation(t, pool, start)

	// The requester is a standing approver too (admin), so only the "own"
	// rule refuses them.
	requesterID := seedPerson(t, pool, adminaccess.RoleAdmin)
	request := seededRequest(t, pool)
	request.UserID = requesterID
	request.BrowserSessionRef = seedBrowserSession(t, pool, requesterID)
	e := mustRequest(t, pool, request).Elevation
	at := e.RequestedAt.Add(time.Minute)

	for name, c := range map[string]struct {
		approver int64
		want     error
	}{
		"own request":         {requesterID, adminaccess.ErrElevationOwnRequest},
		"elevated approver":   {elevatedApprover, adminaccess.ErrElevationApproverElevated},
		"no standing approve": {auditor, adminaccess.ErrElevationApproverNotEligible},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := answerRequest(t, pool, approval(e.ID, c.approver, at)); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if stored := readElevation(t, pool, e.ID); stored.Decision != "" {
				t.Fatalf("a refused answer decided the request: %+v", stored)
			}
		})
	}
	if got := eventActions(t, pool, e.ID); len(got) != 1 {
		t.Fatalf("refused answers wrote events: %v", got)
	}
	if _, err := answerRequest(t, pool, approval(uuid.New(), auditor, at)); !errors.Is(err, adminaccess.ErrElevationNotFound) {
		t.Fatalf("an unknown request: %v", err)
	}
}
