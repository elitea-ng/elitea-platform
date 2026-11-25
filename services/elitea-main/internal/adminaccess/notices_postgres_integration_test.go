package adminaccess_test

// The access notices on the real schema (ADR-0059 decision 5, "Loud" and
// "The mailer is down"; story R2.5a, issue 2519): the outbox of shared 0277,
// written in the transaction of the event, and the sender that delivers it
// through a mailer fake. Also the `inherit` token notice's delivery stamp
// (R3.15b, issue 2619; shared 0278).

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// noticeMailer is the mailer fake: it records every notice it accepted.
type noticeMailer struct {
	mu           sync.Mutex
	unconfigured bool
	failing      bool
	sent         []adminaccess.Notice
}

func (m *noticeMailer) Configured(context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.unconfigured
}

func (m *noticeMailer) SendAccessNotice(_ context.Context, notice adminaccess.Notice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return errors.New("smtp: 421 service not available")
	}
	m.sent = append(m.sent, notice)
	return nil
}

func (m *noticeMailer) fail(failing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failing = failing
}

func (m *noticeMailer) recipients() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sent))
	for _, notice := range m.sent {
		out = append(out, notice.RecipientEmail)
	}
	slices.Sort(out)
	return out
}

// newNoticeDatabase is the elevation database with every account the corpus
// seeded suspended, so the people a test seeds are the only recipients.
func newNoticeDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newElevationDatabase(t)
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true`); err != nil {
		t.Fatalf("suspend the seeded accounts: %v", err)
	}
	return pool
}

// seedRecipient is an account with an address and the given administration
// roles; it is a member of no project.
func seedRecipient(t *testing.T, pool *pgxpool.Pool, roles ...string) (int64, string) {
	t.Helper()
	id := seedPerson(t, pool, roles...)
	var email string
	if err := pool.QueryRow(t.Context(), `SELECT email FROM public.auth_core__user WHERE id = $1`, id).Scan(&email); err != nil {
		t.Fatalf("read the address: %v", err)
	}
	return id, email
}

// seedCustomRole is an administration role holding only permissions.
func seedCustomRole(t *testing.T, pool *pgxpool.Pool, permissions ...string) string {
	t.Helper()
	name := "custom_" + uuid.NewString()[:8]
	if _, err := pool.Exec(t.Context(), `
WITH role AS (INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration') RETURNING id)
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, permission FROM role, unnest($2::text[]) AS permission`, name, permissions); err != nil {
		t.Fatalf("seed a custom role: %v", err)
	}
	return name
}

func newNoticeSender(t *testing.T, pool *pgxpool.Pool, mailer adminaccess.NoticeMailer, now func() time.Time) *adminaccess.NoticeSender {
	t.Helper()
	sender, err := adminaccess.NewNoticeSender(pool, mailer, "https://elitea.example", now)
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	return sender
}

func sendPass(t *testing.T, sender *adminaccess.NoticeSender) adminaccess.NoticeSendResult {
	t.Helper()
	result, err := sender.Pass(t.Context())
	if err != nil {
		t.Fatalf("sender pass: %v", err)
	}
	return result
}

// startWithNotices starts the elevation and writes its activation notices in
// the same transaction, as R2.2c and R2.7b will.
func startWithNotices(t *testing.T, pool *pgxpool.Pool, start adminaccess.ElevationStart) (adminaccess.ElevationRecord, int) {
	t.Helper()
	var elevation adminaccess.ElevationRecord
	var written int
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		if elevation, err = adminaccess.StartElevation(t.Context(), tx, auditAppender(t), start); err != nil {
			return err
		}
		written, err = adminaccess.WriteElevationNotices(t.Context(), tx, adminaccess.ElevationNotice{
			Kind: adminaccess.NoticeActivation, Elevation: elevation, At: start.At,
		})
		return err
	}); err != nil {
		t.Fatalf("start an elevation with its notices: %v", err)
	}
	return elevation, written
}

func writeNotices(t *testing.T, pool *pgxpool.Pool, notice adminaccess.ElevationNotice) int {
	t.Helper()
	var written int
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		written, err = adminaccess.WriteElevationNotices(t.Context(), tx, notice)
		return err
	}); err != nil {
		t.Fatalf("write notices: %v", err)
	}
	return written
}

type noticeRow struct {
	recipient string
	attempts  int
	lastError string
	sent      bool
}

func noticeRows(t *testing.T, pool *pgxpool.Pool) []noticeRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT recipient_email, attempts, COALESCE(last_error, ''), sent_at IS NOT NULL
FROM elitea_auth.admin_access_notices ORDER BY recipient_email`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (noticeRow, error) {
		var r noticeRow
		return r, row.Scan(&r.recipient, &r.attempts, &r.lastError, &r.sent)
	})
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return out
}

func sorted(values ...string) []string {
	slices.Sort(values)
	return values
}

// An activation tells every holder of an administrator role and every holder
// of access.breakglass.review, one e-mail each, and nobody else: not the
// elevated person, not a holder of another administration role, not an
// account with no role. None of them is a member of any project.
func TestActivationSendsOneEmailPerRecipient(t *testing.T) {
	pool := newNoticeDatabase(t)
	_, admin := seedRecipient(t, pool, adminaccess.RoleAdmin)
	_, superAdmin := seedRecipient(t, pool, adminaccess.RoleSuperAdmin)
	_, auditor := seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	seedRecipient(t, pool, adminaccess.RoleLLMManager)
	seedRecipient(t, pool)

	start := seededStart(t, pool)
	elevation, written := startWithNotices(t, pool, start)
	// The approver of seededStart holds `admin` too.
	var approver string
	if err := pool.QueryRow(t.Context(), `SELECT email FROM public.auth_core__user WHERE id = $1`,
		start.ApproverUserID).Scan(&approver); err != nil {
		t.Fatal(err)
	}
	want := sorted(admin, superAdmin, auditor, approver)
	if written != len(want) {
		t.Fatalf("wrote %d notices, want %d", written, len(want))
	}

	mailer := &noticeMailer{}
	result := sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return start.At }))
	if result.Sent != len(want) || result.Failed != 0 || result.Pending != 0 || !result.MailerConfigured {
		t.Fatalf("pass = %+v", result)
	}
	if got := mailer.recipients(); !slices.Equal(got, want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for _, notice := range mailer.sent {
		if notice.Kind != adminaccess.NoticeActivation || notice.ElevationID != elevation.ID ||
			notice.ReasonCode != adminaccess.ElevationReasonIncident || notice.Level != adminaccess.ElevationLevelAdmin ||
			notice.PersonName == "" || notice.ApprovalURL != "" {
			t.Errorf("notice = %+v", notice)
		}
		// Every recipient here holds access.breakglass.review, so each may
		// read the reason in the console.
		if notice.Reason != start.Reason {
			t.Errorf("%s: reason %q, want the text", notice.RecipientEmail, notice.Reason)
		}
	}
	var members int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__user account ON account.id = assignment.user_id
WHERE account.email = ANY($1::text[])`, want).Scan(&members); err != nil || members != 0 {
		t.Fatalf("premise: the recipients hold %d project memberships (%v)", members, err)
	}
	// A second pass sends nothing.
	if again := sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return start.At.Add(time.Hour) })); again.Sent != 0 {
		t.Fatalf("second pass sent %d", again.Sent)
	}
}

// A holder of an administrator role who holds neither approve nor review
// (a deployment that took them out of the role) is told without the reason
// text; the reason code is still there.
func TestNoticeCarriesTheReasonOnlyToApproversAndReviewers(t *testing.T) {
	pool := newNoticeDatabase(t)
	if _, err := pool.Exec(t.Context(), `
DELETE FROM public.auth_core__role_permission grant_row
USING public.auth_core__role role
WHERE role.id = grant_row.role_id AND role.name = 'super_admin' AND role.mode = 'administration'
  AND grant_row.permission IN ($1, $2)`, adminaccess.AccessBreakglassApprove, adminaccess.AccessBreakglassReview); err != nil {
		t.Fatal(err)
	}
	_, superAdmin := seedRecipient(t, pool, adminaccess.RoleSuperAdmin)
	start := seededStart(t, pool)
	startWithNotices(t, pool, start)
	mailer := &noticeMailer{}
	sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return start.At }))
	found := false
	for _, notice := range mailer.sent {
		if notice.RecipientEmail != superAdmin {
			continue
		}
		found = true
		if notice.Reason != "" || notice.ReasonCode != adminaccess.ElevationReasonIncident {
			t.Errorf("a recipient who may not read the reason got %q (code %q)", notice.Reason, notice.ReasonCode)
		}
	}
	if !found {
		t.Fatalf("the super_admin was not told: %v", mailer.recipients())
	}
}

// With the mailer failing the elevation still starts and its rows stay for
// retry, with the attempt and the error; the next pass after the retry delay
// delivers each once, and a pass after that sends nothing.
func TestMailerFailureNeverBlocksTheElevation(t *testing.T) {
	pool := newNoticeDatabase(t)
	seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	start := seededStart(t, pool)
	elevation, written := startWithNotices(t, pool, start)
	if written != 2 {
		t.Fatalf("wrote %d notices, want 2 (the auditor and the approver)", written)
	}
	if current := readElevation(t, pool, elevation.ID); !current.ActiveAt(start.At.Add(time.Minute)) {
		t.Fatalf("the elevation is not active: %+v", current)
	}

	clock := start.At
	now := func() time.Time { return clock }
	mailer := &noticeMailer{failing: true}
	sender := newNoticeSender(t, pool, mailer, now)
	if result := sendPass(t, sender); result.Sent != 0 || result.Failed != 2 || result.Pending != 2 {
		t.Fatalf("failing pass = %+v", result)
	}
	for _, row := range noticeRows(t, pool) {
		if row.sent || row.attempts != 1 || row.lastError == "" {
			t.Fatalf("row after a failed send = %+v", row)
		}
	}
	// Not due again before the retry delay.
	mailer.fail(false)
	clock = start.At.Add(30 * time.Second)
	if result := sendPass(t, sender); result.Sent != 0 || result.Pending != 2 {
		t.Fatalf("pass inside the retry delay = %+v", result)
	}
	clock = start.At.Add(adminaccess.NoticeRetryDelay(1) + time.Second)
	if result := sendPass(t, sender); result.Sent != 2 || result.Pending != 0 {
		t.Fatalf("retry pass = %+v", result)
	}
	clock = clock.Add(time.Hour)
	if result := sendPass(t, sender); result.Sent != 0 {
		t.Fatalf("a pass after delivery sent %d", result.Sent)
	}
	if got := mailer.recipients(); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("delivered = %v, want each row once", got)
	}
	for _, row := range noticeRows(t, pool) {
		if !row.sent || row.attempts != 2 || row.lastError != "" {
			t.Fatalf("row after delivery = %+v", row)
		}
	}
}

// With no mailer configured nothing is claimed: every row stays pending, the
// pass says how many, and they go out once a mailer is configured.
func TestNoMailerKeepsTheNoticesPending(t *testing.T) {
	pool := newNoticeDatabase(t)
	start := seededStart(t, pool)
	startWithNotices(t, pool, start)
	mailer := &noticeMailer{unconfigured: true}
	sender := newNoticeSender(t, pool, mailer, func() time.Time { return start.At })
	if result := sendPass(t, sender); result.MailerConfigured || result.Sent != 0 || result.Pending != 1 {
		t.Fatalf("pass without a mailer = %+v", result)
	}
	if rows := noticeRows(t, pool); len(rows) != 1 || rows[0].attempts != 0 || rows[0].sent {
		t.Fatalf("rows = %+v", rows)
	}
	mailer.unconfigured = false
	if result := sendPass(t, sender); result.Sent != 1 {
		t.Fatalf("pass with a mailer = %+v", result)
	}
}

// The notice does not depend on the signal ingest (ADR-0058 E5, which has no
// ingest in this release) nor on the security audit relay: with nothing but
// the database and the mailer, and the audit events of the elevation still
// undelivered in their outbox, the notice is sent.
func TestNoticeIsSentWithTheSignalIngestDisabled(t *testing.T) {
	pool := newNoticeDatabase(t)
	start := seededStart(t, pool)
	elevation, _ := startWithNotices(t, pool, start)
	if events := elevationEvents(t, pool, elevation.ID.String()); len(events) != 1 {
		t.Fatalf("premise: %d undelivered events of the elevation, want its `started`", len(events))
	}
	mailer := &noticeMailer{}
	if result := sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return start.At })); result.Sent != 1 {
		t.Fatalf("pass = %+v", result)
	}
}

// A recipient with no project membership gets the e-mail (asserted by
// TestActivationSendsOneEmailPerRecipient). The bell notice needs a
// notification with no project, which issue 2485 (ADR-0058 E11.5) adds; the
// acceptance of that issue names this test.
func TestNoticeReachesTheBellWithoutAProject(t *testing.T) {
	t.Skip("the bell notice waits for issue 2485 (E11.5): centry.notifications.project_id is NOT NULL")
}

// A request tells each holder of access.breakglass.approve except the
// requester, and no one else: not a reviewer who cannot approve, not an
// account with another role.
func TestRequestSendsOneEmailToEachApproverButTheRequester(t *testing.T) {
	pool := newNoticeDatabase(t)
	_, admin := seedRecipient(t, pool, adminaccess.RoleAdmin)
	_, superAdmin := seedRecipient(t, pool, adminaccess.RoleSuperAdmin)
	_, approverOnly := seedRecipient(t, pool, seedCustomRole(t, pool, adminaccess.AccessBreakglassApprove))
	seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	seedRecipient(t, pool, adminaccess.RoleEmergencyAccess)
	seedRecipient(t, pool)
	// The requester holds the permission too, and is not told.
	requester, _ := seedRecipient(t, pool, adminaccess.RoleAdmin)
	at := time.Now().UTC().Truncate(time.Microsecond)
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, path, reason_code, reason, requester_user_id, requested_at)
VALUES ($1, $2, 'approved', 'identity_outage', 'the provider is down', $1, $3)
RETURNING id`, requester, seedLiveSession(t, pool, requester, at), at).Scan(&id); err != nil {
		t.Fatalf("insert a request: %v", err)
	}
	request := readElevation(t, pool, id)
	if written := writeNotices(t, pool, adminaccess.ElevationNotice{
		Kind: adminaccess.NoticeRequest, Elevation: request, At: at,
	}); written != 3 {
		t.Fatalf("wrote %d request notices, want 3", written)
	}
	mailer := &noticeMailer{}
	sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return at }))
	if got, want := mailer.recipients(), sorted(admin, superAdmin, approverOnly); !slices.Equal(got, want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for _, notice := range mailer.sent {
		if notice.ApprovalURL != "https://elitea.example/admin/app/break-glass/requests/"+id.String() ||
			notice.Reason != "the provider is down" || notice.ReasonCode != adminaccess.ElevationReasonIdentityOutage {
			t.Errorf("request notice = %+v", notice)
		}
	}
}

// The move to unreviewed_no_reviewer sends one notice per recipient.
func TestUnreviewedNoReviewerSendsOneNoticePerRecipient(t *testing.T) {
	pool := newNoticeDatabase(t)
	_, admin := seedRecipient(t, pool, adminaccess.RoleAdmin)
	_, auditor := seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	start := seededStart(t, pool)
	elevation := startElevation(t, pool, start)
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: start.At.Add(10 * time.Minute),
	})
	written := writeNotices(t, pool, adminaccess.ElevationNotice{
		Kind: adminaccess.NoticeUnreviewedNoReviewer, Elevation: ended, At: start.At.Add(time.Hour),
	})
	var approver string
	if err := pool.QueryRow(t.Context(), `SELECT email FROM public.auth_core__user WHERE id = $1`,
		start.ApproverUserID).Scan(&approver); err != nil {
		t.Fatal(err)
	}
	want := sorted(admin, auditor, approver)
	if written != len(want) {
		t.Fatalf("wrote %d, want %d", written, len(want))
	}
	mailer := &noticeMailer{}
	sendPass(t, newNoticeSender(t, pool, mailer, func() time.Time { return start.At.Add(time.Hour) }))
	if got := mailer.recipients(); !slices.Equal(got, want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for _, notice := range mailer.sent {
		if notice.Kind != adminaccess.NoticeUnreviewedNoReviewer {
			t.Errorf("kind = %s", notice.Kind)
		}
	}
}

// The writer refuses what it cannot write before the database.
func TestWriteElevationNoticesRefusesBadInput(t *testing.T) {
	pool := newNoticeDatabase(t)
	elevation := startElevation(t, pool, seededStart(t, pool))
	for name, notice := range map[string]adminaccess.ElevationNotice{
		"unknown kind":          {Kind: "other", Elevation: elevation, At: elevation.StartedAt},
		"inherit is not one":    {Kind: adminaccess.NoticeInheritToken, Elevation: elevation, At: elevation.StartedAt},
		"no elevation":          {Kind: adminaccess.NoticeActivation, At: elevation.StartedAt},
		"no time":               {Kind: adminaccess.NoticeActivation, Elevation: elevation},
		"critical with no name": {Kind: adminaccess.NoticeCriticalChange, Elevation: elevation, At: elevation.StartedAt},
	} {
		err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			_, err := adminaccess.WriteElevationNotices(t.Context(), tx, notice)
			return err
		})
		if !errors.Is(err, adminaccess.ErrNoticeInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Two senders running together deliver each row once.
func TestTwoSendersDeliverEachRowOnce(t *testing.T) {
	pool := newNoticeDatabase(t)
	for range 4 {
		seedRecipient(t, pool, adminaccess.RoleAdmin)
	}
	var at time.Time
	total := 0
	for range 5 {
		start := seededStart(t, pool)
		at = start.At
		_, written := startWithNotices(t, pool, start)
		total += written
	}
	mailer := &noticeMailer{}
	now := func() time.Time { return at.Add(time.Minute) }
	senders := []*adminaccess.NoticeSender{newNoticeSender(t, pool, mailer, now), newNoticeSender(t, pool, mailer, now)}
	var wg sync.WaitGroup
	results := make([]adminaccess.NoticeSendResult, len(senders))
	errs := make([]error, len(senders))
	for i, sender := range senders {
		wg.Go(func() { results[i], errs[i] = sender.Pass(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sender %d: %v", i, err)
		}
	}
	if results[0].Sent+results[1].Sent != total {
		t.Fatalf("sent %d + %d, want %d", results[0].Sent, results[1].Sent, total)
	}
	seen := map[uuid.UUID]bool{}
	for _, notice := range mailer.sent {
		if seen[notice.ID] {
			t.Fatalf("row %s delivered twice", notice.ID)
		}
		seen[notice.ID] = true
	}
	if len(seen) != total {
		t.Fatalf("delivered %d distinct rows, want %d", len(seen), total)
	}
}

// The `inherit` token notice (R3.15b): queued once per owner and date, its
// delivery stamps inherit_notice_delivered_at on each listed token; a failed
// send stamps nothing and the retry stamps it.
func TestInheritTokenNoticeStampsTheDelivery(t *testing.T) {
	pool := newNoticeDatabase(t)
	owner, email := seedRecipient(t, pool)
	var tokens []adminaccess.NoticeToken
	for _, name := range []string{"ci", "backup"} {
		var id int64
		if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access) VALUES ($1, $2, $3, 'inherit') RETURNING id`,
			uuid.NewString(), owner, name).Scan(&id); err != nil {
			t.Fatalf("seed a token: %v", err)
		}
		tokens = append(tokens, adminaccess.NoticeToken{ID: id, Name: name})
	}
	at := time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)
	candidate := adminaccess.InheritTokenOwner{UserID: owner, Email: email, Tokens: tokens}
	for i, want := range []bool{true, false} {
		queued, err := adminaccess.QueueInheritTokenNotice(t.Context(), pool, candidate, "2027-01-31", at)
		if err != nil || queued != want {
			t.Fatalf("queue %d = %v, %v; want %v", i, queued, err, want)
		}
	}
	delivered := func() []*time.Time {
		t.Helper()
		var out []*time.Time
		for _, token := range tokens {
			var at *time.Time
			err := pool.QueryRow(t.Context(), `
SELECT inherit_notice_delivered_at FROM elitea_identity.token_lifecycle WHERE token_id = $1`, token.ID).Scan(&at)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal(err)
			}
			out = append(out, at)
		}
		return out
	}
	clock := at
	mailer := &noticeMailer{failing: true}
	sender := newNoticeSender(t, pool, mailer, func() time.Time { return clock })
	if result := sendPass(t, sender); result.Failed != 1 {
		t.Fatalf("failing pass = %+v", result)
	}
	for _, stamp := range delivered() {
		if stamp != nil {
			t.Fatalf("a failed send stamped %v", stamp)
		}
	}
	mailer.fail(false)
	clock = at.Add(adminaccess.NoticeRetryDelay(1) + time.Second)
	if result := sendPass(t, sender); result.Sent != 1 {
		t.Fatalf("retry pass = %+v", result)
	}
	for _, stamp := range delivered() {
		if stamp == nil || !stamp.Equal(clock) {
			t.Fatalf("delivery stamp = %v, want %v", stamp, clock)
		}
	}
	notice := mailer.sent[0]
	// Delivered 2027-01-10: 30 days later is after the sunset date.
	if notice.Kind != adminaccess.NoticeInheritToken || notice.EndsOn != "2027-02-09" || len(notice.Tokens) != 2 ||
		notice.RecipientEmail != email {
		t.Fatalf("notice = %+v", notice)
	}
}
