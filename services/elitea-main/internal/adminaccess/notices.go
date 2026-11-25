package adminaccess

// The access notices (ADR-0059 decision 5, "Loud", "Path 1" step 2 and "The
// mailer is down"; story R2.5a, issue 2519), and the notice to the owners of
// `inherit` tokens (decision 2, "Token owners get 30 days from delivery";
// story R3.15b, issue 2619).
//
// A request that nobody sees expires unseen, and an activation that nobody
// hears of is not loud. The signal ledger and its notification route come
// later (ADR-0058 E5.6), so break-glass carries its own notices: rows of the
// outbox elitea_auth.admin_access_notices (shared 0277), written by
// WriteElevationNotices IN THE TRANSACTION OF THE EVENT, and delivered by
// NoticeSender afterwards.
//
// The writer resolves the recipients when it writes, from STANDING roles,
// one row per recipient and channel, and copies the address: a person whose
// role is taken away afterwards still gets the notice of the event that
// happened. The callers are the ledger stories (R2.2b the request, R2.2c and
// R2.7b the activation, R2.8b a critical change, R2.9a an `unauthorized`
// outcome, R2.9b the overdue review and the move to unreviewed_no_reviewer).
//
// The sender is the only part that talks to the mailer, and a mailer failure
// never reaches the event: the row stays unsent, with its attempts and last
// error, and is retried with a growing delay. A deployment with no mailer
// keeps every row pending and the pass reports how many; nothing drops one.
// Delivery is at least once (a sender that dies after the mailer accepted a
// message and before it marked the row sends it again), and every message is
// idempotent in meaning.
//
// Everything is in-process in elitea-main on PostgreSQL: no internal HTTP
// hop, no Redis key, no NATS subject.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// NoticeKind is one kind of access notice. The table's CHECK constraint holds
// the same list.
type NoticeKind string

// The notice kinds.
const (
	// NoticeRequest: an elevation was requested. Told: the holders of
	// access.breakglass.approve other than the requester, with a link to the
	// approval page.
	NoticeRequest NoticeKind = "request"
	// NoticeActivation: an elevation started.
	NoticeActivation NoticeKind = "activation"
	// NoticeCriticalChange: a `critical` change was made under an elevation.
	NoticeCriticalChange NoticeKind = "critical_change"
	// NoticeReviewOverdue: the review of an ended elevation is overdue.
	NoticeReviewOverdue NoticeKind = "review_overdue"
	// NoticeUnreviewedNoReviewer: the elevation moved to
	// unreviewed_no_reviewer.
	NoticeUnreviewedNoReviewer NoticeKind = "unreviewed_no_reviewer"
	// NoticeReviewUnauthorized: a reviewer recorded the outcome
	// `unauthorized`.
	NoticeReviewUnauthorized NoticeKind = "review_unauthorized"
	// NoticeInheritToken: the administration access of the owner's `inherit`
	// tokens ends (R3.15b). Not about an elevation.
	NoticeInheritToken NoticeKind = "inherit_token"
)

// ElevationNoticeKinds are the kinds WriteElevationNotices writes.
var ElevationNoticeKinds = []NoticeKind{
	NoticeRequest, NoticeActivation, NoticeCriticalChange,
	NoticeReviewOverdue, NoticeUnreviewedNoReviewer, NoticeReviewUnauthorized,
}

// NoticeChannelEmail is the only channel a writer uses. The table also
// accepts `bell`, the in-application notice that waits for issue 2485 (a
// notification with no project).
const NoticeChannelEmail = "email"

// ElevationApprovalPath is the console page an approver opens from a request
// notice (story R3.10 mounts it). The notice makes it absolute against the
// deployment's public origin, never against a request's Host.
func ElevationApprovalPath(id uuid.UUID) string {
	return "/admin/app/break-glass/requests/" + id.String()
}

// ErrNoticeInvalid wraps every refusal of WriteElevationNotices's input.
var ErrNoticeInvalid = errors.New("access notice refused")

// ElevationNotice is what one event writes notices for.
type ElevationNotice struct {
	// Kind is one of ElevationNoticeKinds.
	Kind NoticeKind
	// Elevation is the ledger row the event is about; its ID is required.
	// The requester is read from it (a request is not told to its own
	// requester).
	Elevation ElevationRecord
	// ActorUserID is who acted: the person behind a critical change, the
	// reviewer of an `unauthorized` outcome. 0 for the platform, and for a
	// request or an activation, whose person is the elevation's.
	ActorUserID int64
	// Action is the administration action of a critical change (its action
	// table name). Required for NoticeCriticalChange, ignored otherwise.
	Action string
	// At is the event's time, from the caller's clock. Required.
	At time.Time
}

func (n ElevationNotice) validate() error {
	if !slices.Contains(ElevationNoticeKinds, n.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrNoticeInvalid, n.Kind)
	}
	if n.Elevation.ID == uuid.Nil {
		return fmt.Errorf("%w: no elevation", ErrNoticeInvalid)
	}
	if n.At.IsZero() {
		return fmt.Errorf("%w: no time given", ErrNoticeInvalid)
	}
	if n.ActorUserID < 0 {
		return fmt.Errorf("%w: a negative actor", ErrNoticeInvalid)
	}
	if n.Kind == NoticeCriticalChange && strings.TrimSpace(n.Action) == "" {
		return fmt.Errorf("%w: a critical change names its action", ErrNoticeInvalid)
	}
	return nil
}

// noticeDetail is the `detail` column: ids, names and codes only.
type noticeDetail struct {
	Action      string        `json:"action,omitempty"`
	Tokens      []NoticeToken `json:"tokens,omitempty"`
	SunsetAfter string        `json:"sunset_after,omitempty"`
}

// NoticeToken is one token an `inherit` notice lists.
type NoticeToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Execer is the part of a transaction or a pool a write needs.
type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// standingHolder is the SQL predicate "account holds the permission $p
// through a standing administration role", for a permission parameter.
func standingHolder(permission string) string {
	return `EXISTS (
        SELECT 1
        FROM public.auth_core__user_role assignment
        JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = 'administration'
        JOIN public.auth_core__role_permission grant_row
          ON grant_row.role_id = role.id AND grant_row.permission = ` + permission + `
        WHERE assignment.user_id = account.id AND NOT ` + unassignableRole + `)`
}

// unassignableRole is the predicate of a role the built-in record marks
// unassignable (the administration `system` role of a legacy-backed
// database): its holders are platform principals, not people to tell.
const unassignableRole = `EXISTS (
           SELECT 1 FROM elitea_auth.admin_role_seeds seed
           WHERE seed.role_id = role.id AND seed.marker = 'unassignable')`

// administratorRoleHolder is the predicate "account holds an administrator
// role (admin or super_admin) as a standing administration role".
const administratorRoleHolder = `EXISTS (
        SELECT 1
        FROM public.auth_core__user_role assignment
        JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = 'administration'
        WHERE assignment.user_id = account.id AND role.name IN ('` + RoleAdmin + `', '` + RoleSuperAdmin + `'))`

// WriteElevationNotices writes the notice rows of one event on tx, the
// transaction of the event's ledger write, and answers how many it wrote.
// An error means the caller rolls back, as for any other write of the step;
// the MAILER is not called here, so its state never fails the step.
//
// Recipients, from standing roles of unsuspended accounts with an address:
//   - a request: every holder of access.breakglass.approve, except the
//     requester and the person to be elevated. Each may read the reason text
//     in the console, so the e-mail carries it;
//   - every other kind: every holder of an administrator role (admin,
//     super_admin) and every holder of access.breakglass.review. The reason
//     text is in the e-mail only for those who hold access.breakglass.approve
//     or access.breakglass.review.
//
// An event whose deployment has nobody to tell writes no row; the posture
// check of R2.14 reports a deployment with no notice channel.
func WriteElevationNotices(ctx context.Context, tx pgx.Tx, notice ElevationNotice) (int, error) {
	if err := notice.validate(); err != nil {
		return 0, err
	}
	if tx == nil {
		return 0, errors.New("write access notices: no transaction")
	}
	detail := noticeDetail{}
	if notice.Kind == NoticeCriticalChange {
		detail.Action = strings.TrimSpace(notice.Action)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return 0, fmt.Errorf("write access notices: %w", err)
	}
	excluded := []int64{}
	recipients := standingHolder("$7") + ` OR ` + administratorRoleHolder + ` OR ` + standingHolder("$8")
	if notice.Kind == NoticeRequest {
		excluded = append(excluded, notice.Elevation.RequesterUserID, notice.Elevation.UserID)
		recipients = standingHolder("$7")
	}
	at := notice.At.UTC()
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_access_notices
    (kind, channel, elevation_id, recipient_user_id, recipient_email, recipient_name,
     include_reason, actor_user_id, detail, created_at, next_attempt_at)
SELECT $1, '`+NoticeChannelEmail+`', $2, account.id, btrim(account.email), COALESCE(account.name, ''),
       `+standingHolder("$7")+` OR `+standingHolder("$8")+`,
       NULLIF($3::bigint, 0), $4::jsonb, $5, $5
FROM public.auth_core__user account
WHERE account.suspended = false
  AND account.email IS NOT NULL AND btrim(account.email) <> ''
  AND account.id <> ALL($6::bigint[])
  AND (`+recipients+`)
ORDER BY account.id`,
		string(notice.Kind), notice.Elevation.ID, notice.ActorUserID, encoded, at, excluded,
		AccessBreakglassApprove, AccessBreakglassReview)
	if err != nil {
		return 0, fmt.Errorf("write access notices: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

/* ── the inherit token notice (R3.15b) ─────────────────────────────────── */

// InheritNoticePeriod is how long an `inherit` token keeps working after its
// owner's notice was delivered, at the least (decision 2).
const InheritNoticePeriod = 30 * 24 * time.Hour

// InheritTokenEndsOn is the date an `inherit` token ends: the later of the
// sunset date and 30 days after its owner's notice was delivered. "" while
// the sunset date is not set. Before the delivery it is the sunset date, the
// earliest the token can end: a delivery can only move the date later.
func InheritTokenEndsOn(sunsetAfter string, delivered time.Time) string {
	if sunsetAfter == "" {
		return ""
	}
	if delivered.IsZero() {
		return sunsetAfter
	}
	// ISO dates compare as strings.
	if end := delivered.UTC().Add(InheritNoticePeriod).Format(time.DateOnly); end > sunsetAfter {
		return end
	}
	return sunsetAfter
}

// InheritTokenOwner is one owner of `inherit` tokens whose notice was not
// delivered yet, with those tokens.
type InheritTokenOwner struct {
	UserID int64
	Email  string
	Name   string
	Tokens []NoticeToken
}

// QueueInheritTokenNotice writes the one notice row of an owner, for the
// sunset date sunsetAfter, and reports whether it wrote one. The row is keyed
// by the owner, the date and the tokens it lists, so a pass that runs again
// before the row is delivered writes nothing.
func QueueInheritTokenNotice(ctx context.Context, q Execer, owner InheritTokenOwner, sunsetAfter string, at time.Time) (bool, error) {
	switch {
	case q == nil:
		return false, errors.New("queue the inherit token notice: no database")
	case owner.UserID <= 0 || strings.TrimSpace(owner.Email) == "":
		return false, fmt.Errorf("%w: the owner has no id or address", ErrNoticeInvalid)
	case len(owner.Tokens) == 0:
		return false, fmt.Errorf("%w: no token to list", ErrNoticeInvalid)
	case sunsetAfter == "":
		return false, fmt.Errorf("%w: no sunset date", ErrNoticeInvalid)
	case at.IsZero():
		return false, fmt.Errorf("%w: no time given", ErrNoticeInvalid)
	}
	tokens := slices.Clone(owner.Tokens)
	slices.SortFunc(tokens, func(a, b NoticeToken) int { return cmp.Compare(a.ID, b.ID) })
	ids := make([]string, 0, len(tokens))
	for _, token := range tokens {
		ids = append(ids, strconv.FormatInt(token.ID, 10))
	}
	encoded, err := json.Marshal(noticeDetail{Tokens: tokens, SunsetAfter: sunsetAfter})
	if err != nil {
		return false, fmt.Errorf("queue the inherit token notice: %w", err)
	}
	key := "inherit_token:" + strconv.FormatInt(owner.UserID, 10) + ":" + sunsetAfter + ":" + strings.Join(ids, ",")
	tag, err := q.Exec(ctx, `
INSERT INTO elitea_auth.admin_access_notices
    (kind, channel, recipient_user_id, recipient_email, recipient_name, detail, dedupe_key,
     created_at, next_attempt_at)
VALUES ($1, '`+NoticeChannelEmail+`', $2, $3, $4, $5::jsonb, $6, $7, $7)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		string(NoticeInheritToken), owner.UserID, strings.TrimSpace(owner.Email), owner.Name, encoded, key, at.UTC())
	if err != nil {
		return false, fmt.Errorf("queue the inherit token notice: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

/* ── the sender ────────────────────────────────────────────────────────── */

// AccessNoticeSenderRoutineID is the routine registry row of the sender.
const AccessNoticeSenderRoutineID = "main.access_notice_sender"

// AccessNoticeSenderDefaultSchedule is the sender's default interval.
const AccessNoticeSenderDefaultSchedule = "30s"

// The sender's bounds.
const (
	// defaultNoticeBatch bounds the rows one pass sends; the next takes the
	// rest.
	defaultNoticeBatch = 100
	// noticeLease is how long a claimed row is withheld from the other
	// senders. It is far above one SMTP session (infra/mailer DefaultTimeout,
	// 20 seconds), so a row is sent by one sender at a time.
	noticeLease = 5 * time.Minute
	// noticeFirstRetry and noticeMaxRetry bound the delay after a failed
	// send: it doubles per attempt from the first and stops at the most.
	noticeFirstRetry = time.Minute
	noticeMaxRetry   = time.Hour
	// maxNoticeError bounds the stored error text, in characters.
	maxNoticeError = 500
)

// Notice is one notice as the mailer renders it. Names are display names
// (the address when an account has no name). The reason text is set only for
// a recipient who may read it in the console, and never belongs in a subject.
type Notice struct {
	ID             uuid.UUID
	Kind           NoticeKind
	RecipientEmail string
	RecipientName  string
	// The elevation the notice is about (zero for NoticeInheritToken).
	ElevationID uuid.UUID
	// PersonName is the person elevated, or to be.
	PersonName string
	// ActorName is who acted, when that is not the person (the reviewer,
	// the person behind a critical change); "" for the platform.
	ActorName  string
	Level      string
	Path       string
	ReasonCode string
	Reason     string
	ExpiresAt  time.Time
	// Action is the action of a critical change.
	Action string
	// ApprovalURL is the absolute link to the approval page of a request,
	// built from the public origin; "" when the origin is not configured,
	// and then the mailer makes ApprovalPath absolute against its own.
	ApprovalURL  string
	ApprovalPath string
	// Tokens and EndsOn are the `inherit` notice's: the tokens and the date
	// their administration access ends if this message is delivered now.
	Tokens []NoticeToken
	EndsOn string
}

// NoApprover reports an activation that nobody approved: approval was off
// (the self path) or the sealed credential was used. The e-mail says so.
func (n Notice) NoApprover() bool {
	return n.Path == ElevationPathSelf || n.Path == ElevationPathSealed
}

// NoticeMailer is what the sender needs from the mailer
// (application/mailer.Composer).
type NoticeMailer interface {
	// Configured reports whether a message can be submitted at all.
	Configured(ctx context.Context) bool
	SendAccessNotice(ctx context.Context, notice Notice) error
}

// NoticeSendResult counts what one pass did.
type NoticeSendResult struct {
	Sent   int
	Failed int
	// Pending is the unsent rows left after the pass, due or not.
	Pending int
	// MailerConfigured is false when the pass sent nothing because no
	// mailer is configured; every row stayed pending.
	MailerConfigured bool
}

// NoticeDB is what the sender needs from the database: a *pgxpool.Pool.
type NoticeDB interface {
	Beginner
	Querier
	Execer
}

// NoticeSender delivers the outbox.
type NoticeSender struct {
	db     NoticeDB
	mailer NoticeMailer
	origin string
	now    func() time.Time
	batch  int
}

// NewNoticeSender builds the sender. origin is the deployment's public
// origin (publicorigin.Normalize of DEPLOYMENT_URL), "" when it is not set.
func NewNoticeSender(db NoticeDB, mailer NoticeMailer, origin string, now func() time.Time) (*NoticeSender, error) {
	switch {
	case db == nil:
		return nil, errors.New("access notice sender: no database")
	case mailer == nil:
		return nil, errors.New("access notice sender: no mailer")
	case now == nil:
		return nil, errors.New("access notice sender: no clock")
	}
	return &NoticeSender{db: db, mailer: mailer, origin: strings.TrimRight(origin, "/"), now: now,
		batch: defaultNoticeBatch}, nil
}

// Pass sends the due rows, one claim per row. A row whose send fails stays
// unsent with its error and a later next attempt; the error of the pass
// joins the database failures only, since a mailer failure is the row's
// state, not the pass's.
func (s *NoticeSender) Pass(ctx context.Context) (NoticeSendResult, error) {
	var result NoticeSendResult
	if !s.mailer.Configured(ctx) {
		pending, err := s.pending(ctx)
		result.Pending = pending
		return result, err
	}
	result.MailerConfigured = true
	var errs []error
	for range s.batch {
		notice, claimed, err := s.claim(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if !claimed {
			break
		}
		if sendErr := s.mailer.SendAccessNotice(ctx, notice.Notice); sendErr != nil {
			result.Failed++
			if err := s.markFailed(ctx, notice, sendErr); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := s.markSent(ctx, notice); err != nil {
			errs = append(errs, err)
			continue
		}
		result.Sent++
	}
	pending, err := s.pending(ctx)
	result.Pending = pending
	if err != nil {
		errs = append(errs, err)
	}
	return result, errors.Join(errs...)
}

// claimedNotice is a claimed row: the notice and what marking it needs.
type claimedNotice struct {
	Notice
	attempts int
	tokenIDs []int64
}

// claim takes one due row, withheld from the other senders for the lease.
func (s *NoticeSender) claim(ctx context.Context) (claimedNotice, bool, error) {
	now := s.now().UTC()
	var notice claimedNotice
	var kind, elevationID, detail string
	var expires *time.Time
	err := s.db.QueryRow(ctx, `
WITH due AS (
    SELECT id FROM elitea_auth.admin_access_notices
    WHERE sent_at IS NULL AND channel = '`+NoticeChannelEmail+`' AND next_attempt_at <= $1
    ORDER BY next_attempt_at, created_at, id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE elitea_auth.admin_access_notices notice
    SET attempts = notice.attempts + 1,
        next_attempt_at = $1::timestamptz + make_interval(secs => $2)
    FROM due WHERE notice.id = due.id
    RETURNING notice.*
)
SELECT claimed.id, claimed.kind, claimed.recipient_email, claimed.recipient_name, claimed.attempts,
       claimed.detail::text, COALESCE(claimed.elevation_id::text, ''),
       COALESCE(elevation.level, ''), COALESCE(elevation.path, ''), COALESCE(elevation.reason_code, ''),
       CASE WHEN claimed.include_reason THEN COALESCE(elevation.reason, '') ELSE '' END,
       elevation.expires_at,
       COALESCE(NULLIF(btrim(person.name), ''), person.email, ''),
       COALESCE(NULLIF(btrim(actor.name), ''), actor.email, '')
FROM claimed
LEFT JOIN elitea_auth.admin_elevations elevation ON elevation.id = claimed.elevation_id
LEFT JOIN public.auth_core__user person ON person.id = elevation.user_id
LEFT JOIN public.auth_core__user actor ON actor.id = claimed.actor_user_id`,
		now, noticeLease.Seconds()).Scan(&notice.ID, &kind, &notice.RecipientEmail, &notice.RecipientName,
		&notice.attempts, &detail, &elevationID, &notice.Level, &notice.Path, &notice.ReasonCode, &notice.Reason,
		&expires, &notice.PersonName, &notice.ActorName)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedNotice{}, false, nil
	}
	if err != nil {
		return claimedNotice{}, false, fmt.Errorf("access notice sender: claim a row: %w", err)
	}
	notice.Kind = NoticeKind(kind)
	if expires != nil {
		notice.ExpiresAt = expires.UTC()
	}
	if elevationID != "" {
		if notice.ElevationID, err = uuid.Parse(elevationID); err != nil {
			return claimedNotice{}, false, fmt.Errorf("access notice sender: row %s: %w", notice.ID, err)
		}
	}
	var parsed noticeDetail
	if err := json.Unmarshal([]byte(detail), &parsed); err != nil {
		return claimedNotice{}, false, fmt.Errorf("access notice sender: row %s detail: %w", notice.ID, err)
	}
	notice.Action = parsed.Action
	if notice.Kind == NoticeRequest && notice.ElevationID != uuid.Nil {
		notice.ApprovalPath = ElevationApprovalPath(notice.ElevationID)
		if s.origin != "" {
			notice.ApprovalURL = s.origin + notice.ApprovalPath
		}
	}
	if notice.Kind == NoticeInheritToken {
		notice.Tokens = parsed.Tokens
		notice.EndsOn = InheritTokenEndsOn(parsed.SunsetAfter, now)
		for _, token := range parsed.Tokens {
			notice.tokenIDs = append(notice.tokenIDs, token.ID)
		}
	}
	return notice, true, nil
}

// markSent records the delivery. For an `inherit` notice it also stamps the
// delivery time on each listed token that still exists, once: a later notice
// never moves it.
func (s *NoticeSender) markSent(ctx context.Context, notice claimedNotice) error {
	at := s.now().UTC()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE elitea_auth.admin_access_notices SET sent_at = $2, last_error = NULL
WHERE id = $1 AND sent_at IS NULL`, notice.ID, at); err != nil {
			return err
		}
		if notice.Kind != NoticeInheritToken || len(notice.tokenIDs) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.token_lifecycle (token_id, inherit_notice_delivered_at)
SELECT token.id, $2 FROM public.auth_core__token token WHERE token.id = ANY($1::bigint[])
ON CONFLICT (token_id) DO UPDATE
    SET inherit_notice_delivered_at = COALESCE(token_lifecycle.inherit_notice_delivered_at,
                                               EXCLUDED.inherit_notice_delivered_at)`,
			notice.tokenIDs, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("access notice sender: mark row %s sent: %w", notice.ID, err)
	}
	return nil
}

// markFailed keeps the row unsent with its error, due again after the retry
// delay of its attempt.
func (s *NoticeSender) markFailed(ctx context.Context, notice claimedNotice, sendErr error) error {
	if _, err := s.db.Exec(ctx, `
UPDATE elitea_auth.admin_access_notices SET last_error = $2, next_attempt_at = $3
WHERE id = $1 AND sent_at IS NULL`,
		notice.ID, boundedError(sendErr), s.now().UTC().Add(NoticeRetryDelay(notice.attempts))); err != nil {
		return fmt.Errorf("access notice sender: record the failure of row %s: %w", notice.ID, err)
	}
	return nil
}

// pending counts the unsent rows.
func (s *NoticeSender) pending(ctx context.Context) (int, error) {
	var pending int
	if err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM elitea_auth.admin_access_notices WHERE sent_at IS NULL`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("access notice sender: count the pending rows: %w", err)
	}
	return pending, nil
}

// NoticeRetryDelay is the delay after the attempt-th failed send: a minute,
// doubling per attempt, at most an hour.
func NoticeRetryDelay(attempt int) time.Duration {
	delay := noticeFirstRetry
	for i := 1; i < attempt && delay < noticeMaxRetry; i++ {
		delay *= 2
	}
	return min(delay, noticeMaxRetry)
}

func boundedError(err error) string {
	text := strings.ToValidUTF8(err.Error(), "?")
	if utf8.RuneCountInString(text) <= maxNoticeError {
		return text
	}
	return string([]rune(text)[:maxNoticeError])
}
