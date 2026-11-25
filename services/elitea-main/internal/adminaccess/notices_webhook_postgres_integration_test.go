package adminaccess_test

// The out-of-band webhook of the break-glass notices on the real schema
// (ADR-0059 decision 5, "Loud"; story R2.5b, issue 2598): the webhook row is
// written with the e-mail rows in the transaction of the event (shared
// 0281), and the sender delivers it to an HTTPS test server through the
// egress guard, signed, with ids, a type and a time only.

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// noticeWebhookSecret is the signing secret of the tests, random per run.
var noticeWebhookSecret = uuid.NewString()

// webhookReceiver is the HTTPS test server: it records each delivery and
// answers status.
type webhookReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	bodies [][]byte
	heads  []http.Header
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	receiver := &webhookReceiver{status: http.StatusNoContent}
	receiver.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receiver.mu.Lock()
		defer receiver.mu.Unlock()
		receiver.bodies = append(receiver.bodies, body)
		receiver.heads = append(receiver.heads, r.Header.Clone())
		w.WriteHeader(receiver.status)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

// url is the receiver's URL with a token in its path, as a chat webhook has.
func (r *webhookReceiver) url() string { return r.server.URL + "/hooks/T0KEN-in-the-path" }

func (r *webhookReceiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *webhookReceiver) deliveries() ([][]byte, []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...), append([]http.Header(nil), r.heads...)
}

// newWebhook is the production webhook pointed at the receiver: the guard
// permits the loopback receiver by its allowlist entry, and the receiver's
// certificate is the only root.
func newWebhook(t *testing.T, receiver *webhookReceiver) *adminaccess.NoticeWebhook {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(receiver.server.Certificate())
	webhook, err := adminaccess.NewNoticeWebhook(adminaccess.NoticeWebhookConfig{
		URL: receiver.url(), Secret: noticeWebhookSecret, Allowlist: "127.0.0.1/32", RootCAs: roots,
	}, nil)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	return webhook
}

// enableNoticeWebhook turns the webhook rows on for one test. The tests of
// this package that read the outbox run one at a time (none is parallel), so
// none sees the switch of another.
func enableNoticeWebhook(t *testing.T) {
	t.Helper()
	adminaccess.SetNoticeWebhookEnabled(true)
	t.Cleanup(func() { adminaccess.SetNoticeWebhookEnabled(false) })
}

// fixtureIdentities are every name and address of the database's accounts.
func fixtureIdentities(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT value FROM public.auth_core__user account,
     LATERAL (VALUES (btrim(account.email)), (btrim(COALESCE(account.name, '')))) AS v(value)
WHERE length(value) > 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("premise: the fixture has no names or addresses")
	}
	return out
}

type webhookRow struct {
	kind      string
	attempts  int
	lastError string
	sent      bool
}

func webhookRows(t *testing.T, pool *pgxpool.Pool) []webhookRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT kind, attempts, COALESCE(last_error, ''), sent_at IS NOT NULL
FROM elitea_auth.admin_access_notices WHERE channel = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []webhookRow
	for rows.Next() {
		var row webhookRow
		if err := rows.Scan(&row.kind, &row.attempts, &row.lastError, &row.sent); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// checkDelivery asserts one delivery: the payload's fields and type, its
// signature, and that it names nobody of the fixture.
func checkDelivery(t *testing.T, body []byte, head http.Header, kind adminaccess.NoticeKind,
	elevation adminaccess.ElevationRecord, identities []string,
) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload %s: %v", body, err)
	}
	want := []string{"elevation_id", "level", "notice_id", "path", "time", "type"}
	if len(payload) != len(want) {
		t.Fatalf("payload fields = %v, want exactly %v", payload, want)
	}
	for _, key := range want {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload lacks %q: %s", key, body)
		}
	}
	if payload["type"] != string(kind) || payload["elevation_id"] != elevation.ID.String() ||
		payload["level"] != elevation.Level || payload["path"] != elevation.Path {
		t.Fatalf("payload = %s, want type %s of elevation %s", body, kind, elevation.ID)
	}
	if _, err := time.Parse(time.RFC3339, payload["time"].(string)); err != nil {
		t.Fatalf("time %v: %v", payload["time"], err)
	}
	for _, identity := range identities {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(identity)) {
			t.Fatalf("the payload names %q: %s", identity, body)
		}
	}
	if got, want := head.Get(adminaccess.NoticeWebhookSignatureHeader),
		adminaccess.SignNoticeWebhookBody([]byte(noticeWebhookSecret), body); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if head.Get(adminaccess.NoticeWebhookEventHeader) != string(kind) {
		t.Fatalf("event header = %q", head.Get(adminaccess.NoticeWebhookEventHeader))
	}
}

// An activation makes one webhook call, beside the e-mails, and its payload
// has the type and no person.
func TestActivationMakesOneWebhookCallNamingNobody(t *testing.T) {
	enableNoticeWebhook(t)
	pool := newNoticeDatabase(t)
	seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	start := seededStart(t, pool)
	elevation, written := startWithNotices(t, pool, start)
	// The auditor, the approver and the webhook.
	if written != 3 {
		t.Fatalf("wrote %d notices, want 3", written)
	}
	receiver := newWebhookReceiver(t)
	mailer := &noticeMailer{}
	sender := newNoticeSender(t, pool, mailer, func() time.Time { return start.At }).WithWebhook(newWebhook(t, receiver))
	result := sendPass(t, sender)
	if result.Sent != 2 || result.WebhookSent != 1 || result.WebhookFailed != 0 || result.WebhookPending != 0 ||
		!result.WebhookConfigured {
		t.Fatalf("pass = %+v", result)
	}
	bodies, heads := receiver.deliveries()
	if len(bodies) != 1 {
		t.Fatalf("the webhook was called %d times, want 1", len(bodies))
	}
	checkDelivery(t, bodies[0], heads[0], adminaccess.NoticeActivation, elevation, fixtureIdentities(t, pool))
	// A second pass calls nothing.
	sendPass(t, sender)
	if bodies, _ := receiver.deliveries(); len(bodies) != 1 {
		t.Fatalf("a second pass called the webhook again (%d calls)", len(bodies))
	}
}

// The move to unreviewed_no_reviewer makes one webhook call with that type.
func TestUnreviewedNoReviewerMakesOneWebhookCall(t *testing.T) {
	enableNoticeWebhook(t)
	pool := newNoticeDatabase(t)
	start := seededStart(t, pool)
	elevation := startElevation(t, pool, start)
	ended, _ := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: start.At.Add(10 * time.Minute),
	})
	writeNotices(t, pool, adminaccess.ElevationNotice{
		Kind: adminaccess.NoticeUnreviewedNoReviewer, Elevation: ended, At: start.At.Add(time.Hour),
	})
	receiver := newWebhookReceiver(t)
	sender := newNoticeSender(t, pool, &noticeMailer{}, func() time.Time { return start.At.Add(time.Hour) }).
		WithWebhook(newWebhook(t, receiver))
	if result := sendPass(t, sender); result.WebhookSent != 1 {
		t.Fatalf("pass = %+v", result)
	}
	bodies, heads := receiver.deliveries()
	if len(bodies) != 1 {
		t.Fatalf("the webhook was called %d times, want 1", len(bodies))
	}
	checkDelivery(t, bodies[0], heads[0], adminaccess.NoticeUnreviewedNoReviewer, ended, fixtureIdentities(t, pool))
}

// A failing webhook leaves its row for retry, with an error that does not
// name the URL, and the e-mail rows of the same event are sent in the same
// pass. The row goes out once the receiver answers again.
func TestFailingWebhookLeavesTheRowAndNeverBlocksTheEmail(t *testing.T) {
	enableNoticeWebhook(t)
	pool := newNoticeDatabase(t)
	seedRecipient(t, pool, adminaccess.RoleSecurityAuditor)
	start := seededStart(t, pool)
	startWithNotices(t, pool, start)
	receiver := newWebhookReceiver(t)
	receiver.answer(http.StatusInternalServerError)
	clock := start.At
	mailer := &noticeMailer{}
	sender := newNoticeSender(t, pool, mailer, func() time.Time { return clock }).WithWebhook(newWebhook(t, receiver))

	result := sendPass(t, sender)
	if result.Sent != 2 || result.Pending != 0 || result.WebhookSent != 0 || result.WebhookFailed != 1 ||
		result.WebhookPending != 1 {
		t.Fatalf("pass with a failing webhook = %+v", result)
	}
	if got := mailer.recipients(); len(got) != 2 {
		t.Fatalf("e-mails = %v, want 2", got)
	}
	rows := webhookRows(t, pool)
	if len(rows) != 1 || rows[0].sent || rows[0].attempts != 1 || rows[0].lastError == "" {
		t.Fatalf("webhook row after a failure = %+v", rows)
	}
	if strings.Contains(rows[0].lastError, "T0KEN") || strings.Contains(rows[0].lastError, "127.0.0.1") {
		t.Fatalf("the stored error names the URL: %q", rows[0].lastError)
	}
	// A sender whose webhook was taken away still counts the row, so the
	// routine can say it waits, and sends nothing.
	unconfigured := newNoticeSender(t, pool, &noticeMailer{}, func() time.Time { return clock })
	if result := sendPass(t, unconfigured); result.WebhookConfigured || result.WebhookPending != 1 || result.WebhookSent != 0 {
		t.Fatalf("pass with no webhook = %+v", result)
	}

	receiver.answer(http.StatusOK)
	clock = start.At.Add(adminaccess.NoticeRetryDelay(1) + time.Second)
	if result := sendPass(t, sender); result.WebhookSent != 1 || result.WebhookPending != 0 {
		t.Fatalf("retry pass = %+v", result)
	}
	if rows := webhookRows(t, pool); len(rows) != 1 || !rows[0].sent || rows[0].attempts != 2 || rows[0].lastError != "" {
		t.Fatalf("webhook row after the retry = %+v", rows)
	}
}

// A request is for the approvers: it writes no webhook row. With the webhook
// off, no event writes one.
func TestWebhookRowsOnlyForTheReportedEventsAndOnlyWhenConfigured(t *testing.T) {
	pool := newNoticeDatabase(t)
	start := seededStart(t, pool)
	startWithNotices(t, pool, start)
	if rows := webhookRows(t, pool); len(rows) != 0 {
		t.Fatalf("with the webhook off, an activation wrote %d webhook rows", len(rows))
	}

	enableNoticeWebhook(t)
	elevation := startElevation(t, pool, seededStart(t, pool))
	writeNotices(t, pool, adminaccess.ElevationNotice{
		Kind: adminaccess.NoticeRequest, Elevation: elevation, At: start.At,
	})
	if rows := webhookRows(t, pool); len(rows) != 0 {
		t.Fatalf("a request wrote %d webhook rows", len(rows))
	}
	writeNotices(t, pool, adminaccess.ElevationNotice{
		Kind: adminaccess.NoticeCriticalChange, Elevation: elevation, Action: "roles.write", At: start.At,
	})
	if rows := webhookRows(t, pool); len(rows) != 1 || rows[0].kind != string(adminaccess.NoticeCriticalChange) {
		t.Fatalf("a critical change wrote %+v", rows)
	}
}
