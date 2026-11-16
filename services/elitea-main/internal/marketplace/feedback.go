package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// Feedback per listing version (ADR-0051 decision 5, issue 1631): reviews from
// verified hires, workforce reports, the publisher's reply and flag, and the
// administrator's hide. The schema is elitea_marketplace.listing_feedback
// (shared migration 0252).
//
// The privacy rule holds in every read here: a feedback item never carries a
// project id or a project name. A review shows its author's name. A workforce
// report shows only that it came from a hiring project.

// Kinds of feedback.
const (
	FeedbackReview = "review"
	FeedbackReport = "workforce_report"
	FeedbackSignal = "signal"
)

// States of a feedback item.
const (
	FeedbackVisible = "visible"
	FeedbackHidden  = "hidden"
)

// Authors of feedback.
const (
	AuthorPerson = "person"
	AuthorAgent  = "agent"
	AuthorPolicy = "policy"
)

// PolicyAutoFeedback labels a report that the agents.workforce.auto_feedback
// policy published without a person's approval of that one report.
const PolicyAutoFeedback = "policy:auto_feedback"

// Outcomes of a workforce report.
const (
	OutcomeKept      = "kept"
	OutcomeDemoted   = "demoted"
	OutcomeDismissed = "dismissed"
)

// Audit actions of the feedback events.
const (
	ActionCreated = "created"
	ActionEdited  = "edited"
	ActionReply   = "reply"
	ActionFlag    = "flag"
	ActionUnflag  = "unflag"
	ActionHide    = "hide"
	ActionUnhide  = "unhide"
)

// Limits of the text a person or an agent writes, and of a page of items.
const (
	MaxReviewBody = 2000
	MaxReportBody = 500
	MaxReplyBody  = 2000
	MaxReason     = 500
	// A workforce manager reads at most 20 items (ADR-0051 decision 6).
	DefaultFeedbackLimit = 20
	MaxFeedbackLimit     = 50
)

// Errors the feedback store answers on top of the ones in types.go.
var (
	// ErrNotVerifiedHire answers a review or a report from a project that never
	// hired the listing.
	ErrNotVerifiedHire = errors.New("marketplace: only a project that hired the listing may leave feedback")
	// ErrOwnListing answers feedback on a listing its author published.
	ErrOwnListing = errors.New("marketplace: feedback on your own listing is not allowed")
	// ErrAlreadyDone answers a second reply, or a second report for the same
	// hired agent and stage.
	ErrAlreadyDone = errors.New("marketplace: already done")
	// ErrNotWorkforceManager answers a report from an agent that is not a
	// workforce manager of the project.
	ErrNotWorkforceManager = errors.New("marketplace: the agent is not a workforce manager of the project")
)

// TxDB is a DB that can open a transaction: a pgx pool.
type TxDB interface {
	DB
	Begin(ctx context.Context) (pgx.Tx, error)
}

func inTx(ctx context.Context, db TxDB, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("marketplace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketplace: commit: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// recordEvent adds a row to the audit trail of the item.
func recordEvent(ctx context.Context, db DB, feedbackID, listingID int64, action string, actor int64, reason string) error {
	if _, err := db.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_feedback_events (feedback_id, listing_id, action, actor_user_id, reason)
VALUES ($1, $2, $3, $4, $5)`, feedbackID, listingID, action, actor, clamp(strings.TrimSpace(reason), MaxReason)); err != nil {
		return fmt.Errorf("marketplace: record the feedback event: %w", err)
	}
	return nil
}

// RefreshRating recomputes rating_avg and rating_count of a listing from its
// visible reviews. A listing with none has a null average and a zero count.
func RefreshRating(ctx context.Context, db DB, listingID int64) error {
	if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listings l
SET rating_avg = r.avg, rating_count = r.n, updated_at = clock_timestamp()
FROM (SELECT round(avg(f.rating)::numeric, 2) AS avg, count(*)::int AS n
      FROM elitea_marketplace.listing_feedback f
      WHERE f.listing_id = $1 AND f.kind = 'review' AND f.state = 'visible') r
WHERE l.id = $1 AND (l.rating_avg IS DISTINCT FROM r.avg OR l.rating_count <> r.n)`, listingID); err != nil {
		return fmt.Errorf("marketplace: refresh the rating: %w", err)
	}
	return nil
}

// RefreshAllRatings recomputes every rating. The signals routine runs it, so a
// purge of a snapshot cannot leave a stale average.
func RefreshAllRatings(ctx context.Context, db DB) error {
	if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listings l
SET rating_avg = r.avg, rating_count = r.n, updated_at = clock_timestamp()
FROM (SELECT l2.id,
             round(avg(f.rating) FILTER (WHERE f.kind = 'review' AND f.state = 'visible')::numeric, 2) AS avg,
             (count(f.id) FILTER (WHERE f.kind = 'review' AND f.state = 'visible'))::int AS n
      FROM elitea_marketplace.listings l2
      LEFT JOIN elitea_marketplace.listing_feedback f ON f.listing_id = l2.id
      GROUP BY l2.id) r
WHERE l.id = r.id AND (l.rating_avg IS DISTINCT FROM r.avg OR l.rating_count <> r.n)`); err != nil {
		return fmt.Errorf("marketplace: refresh the ratings: %w", err)
	}
	return nil
}

// hiredVersions lists the versions of the listing the project hired, newest
// first. A hire that was later dismissed still counts: the person may review
// what they used.
func hiredVersions(ctx context.Context, db DB, projectID, listingID int64) ([]int64, error) {
	rows, err := db.Query(ctx, `
SELECT DISTINCT version_id FROM elitea_marketplace.installs
WHERE project_id = $1 AND listing_id = $2 ORDER BY version_id DESC`, projectID, listingID)
	if err != nil {
		return nil, fmt.Errorf("marketplace: read the hired versions: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func containsID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// listingOrigin reads who published the listing and from which project.
func listingOrigin(ctx context.Context, db DB, listingID int64) (*int64, int64, error) {
	var publisher *int64
	var source int64
	err := db.QueryRow(ctx, `SELECT publisher_id, source_project_id FROM elitea_marketplace.listings WHERE id = $1`, listingID).
		Scan(&publisher, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("marketplace: read the listing: %w", err)
	}
	return publisher, source, nil
}

// ReviewInput is a person's review.
type ReviewInput struct {
	ListingID int64
	// VersionID is the hired version being reviewed. Zero means the version the
	// project hired most recently.
	VersionID int64
	// ProjectID is the project the person reviews from. It must have hired the
	// listing.
	ProjectID int64
	UserID    int64
	Rating    int
	Body      string
}

// SaveReview records or edits the person's review of the listing. It answers
// the id of the item and whether it is new. A person has one review per
// listing: a second call edits it and keeps the publisher's reply.
func SaveReview(ctx context.Context, db TxDB, in ReviewInput) (int64, bool, error) {
	if in.Rating < 1 || in.Rating > 5 {
		return 0, false, &ParamError{Param: "rating", Message: "must be from 1 to 5"}
	}
	body := strings.TrimSpace(in.Body)
	if len([]rune(body)) > MaxReviewBody {
		return 0, false, &ParamError{Param: "body", Message: fmt.Sprintf("must be at most %d characters", MaxReviewBody)}
	}
	if in.UserID <= 0 || in.ProjectID <= 0 || in.ListingID <= 0 {
		return 0, false, ErrInvalidRequest
	}
	var id int64
	var created bool
	err := inTx(ctx, db, func(tx pgx.Tx) error {
		publisher, _, err := listingOrigin(ctx, tx, in.ListingID)
		if err != nil {
			return err
		}
		if publisher != nil && *publisher == in.UserID {
			return ErrOwnListing
		}
		versions, err := hiredVersions(ctx, tx, in.ProjectID, in.ListingID)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			return ErrNotVerifiedHire
		}
		version := in.VersionID
		if version == 0 {
			version = versions[0]
		} else if !containsID(versions, version) {
			return ErrNotVerifiedHire
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listing_feedback
    (listing_id, version_id, kind, rating, body, author_kind, author_user_id, project_id)
VALUES ($1, $2, 'review', $3, $4, 'person', $5, $6)
ON CONFLICT (listing_id, author_user_id) WHERE kind = 'review'
DO UPDATE SET version_id = EXCLUDED.version_id, rating = EXCLUDED.rating, body = EXCLUDED.body,
              project_id = EXCLUDED.project_id, updated_at = clock_timestamp()
RETURNING id, (xmax = 0)`, in.ListingID, version, in.Rating, body, in.UserID, in.ProjectID).Scan(&id, &created); err != nil {
			return fmt.Errorf("marketplace: save the review: %w", err)
		}
		action := ActionEdited
		if created {
			action = ActionCreated
		}
		if err := recordEvent(ctx, tx, id, in.ListingID, action, in.UserID, ""); err != nil {
			return err
		}
		return RefreshRating(ctx, tx, in.ListingID)
	})
	return id, created, err
}

// scorecardKeys are the only facts a workforce report's scorecard summary may
// hold: platform-computed numbers, never text.
var scorecardKeys = map[string]bool{
	"failure_rate": true, "breaker_trips": true, "cost_vs_median": true, "active_days": true,
}

// CleanScorecard keeps the numeric values under the four scorecard keys and
// drops everything else, so a report cannot carry a transcript, a prompt, tool
// arguments or memory.
func CleanScorecard(raw json.RawMessage) json.RawMessage {
	out := map[string]float64{}
	var generic map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &generic) == nil {
		for key, value := range generic {
			if number, ok := value.(float64); ok && scorecardKeys[key] {
				out[key] = number
			}
		}
	}
	encoded, _ := json.Marshal(out)
	return encoded
}

// ReportInput is a workforce report: an agent's account of a dismissal or a
// demotion of a hired agent.
type ReportInput struct {
	ListingID int64
	ProjectID int64
	// HiredEntityID is the hired agent the report is about. Its install gives
	// the version the report belongs to.
	HiredEntityID int64
	// AgentID is the workforce manager that wrote the report.
	AgentID int64
	// ApprovedBy is the person who approved the report. Under the
	// auto_feedback policy it is the person who set the policy, and Policy is
	// true.
	ApprovedBy int64
	Policy     bool
	Outcome    string
	Stage      string
	ReasonCode string
	Body       string
	// Scorecard is the platform-computed summary; CleanScorecard drops
	// anything that is not one of its numbers.
	Scorecard json.RawMessage
}

// SaveWorkforceReport records a workforce report on the version the hired agent
// was installed from. The guards of ADR-0051 decision 6 hold here: the agent is
// a workforce manager of the project, the project hired the listing, the
// project did not publish it, and there is one report per hired agent and
// stage. It answers the id of the item.
func SaveWorkforceReport(ctx context.Context, db TxDB, in ReportInput) (int64, error) {
	switch in.Outcome {
	case OutcomeKept, OutcomeDemoted, OutcomeDismissed:
	default:
		return 0, &ParamError{Param: "outcome", Message: "must be kept, demoted or dismissed"}
	}
	body := strings.TrimSpace(in.Body)
	if len([]rune(body)) > MaxReportBody {
		return 0, &ParamError{Param: "text", Message: fmt.Sprintf("must be at most %d characters", MaxReportBody)}
	}
	stage := strings.TrimSpace(in.Stage)
	if stage == "" || len(stage) > 64 {
		return 0, &ParamError{Param: "stage", Message: "is required and at most 64 characters"}
	}
	reason := strings.TrimSpace(in.ReasonCode)
	if len(reason) > 64 {
		return 0, &ParamError{Param: "reason_code", Message: "must be at most 64 characters"}
	}
	if in.ListingID <= 0 || in.ProjectID <= 0 || in.HiredEntityID <= 0 || in.AgentID <= 0 || in.ApprovedBy <= 0 {
		return 0, ErrInvalidRequest
	}
	authorKind, label := AuthorAgent, ""
	if in.Policy {
		authorKind, label = AuthorPolicy, PolicyAutoFeedback
	}
	var id int64
	err := inTx(ctx, db, func(tx pgx.Tx) error {
		_, source, err := listingOrigin(ctx, tx, in.ListingID)
		if err != nil {
			return err
		}
		if source == in.ProjectID {
			return ErrOwnListing
		}
		var version int64
		if err := tx.QueryRow(ctx, `
SELECT version_id FROM elitea_marketplace.installs
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2 AND listing_id = $3`,
			in.ProjectID, in.HiredEntityID, in.ListingID).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotVerifiedHire
		} else if err != nil {
			return fmt.Errorf("marketplace: read the hire: %w", err)
		}
		manager, err := isWorkforceManager(ctx, tx, in.ProjectID, in.AgentID)
		if err != nil {
			return err
		}
		if !manager {
			return ErrNotWorkforceManager
		}
		scorecard := CleanScorecard(in.Scorecard)
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listing_feedback
    (listing_id, version_id, kind, body, outcome, stage, reason_code, scorecard,
     author_kind, author_user_id, author_agent_id, author_label, project_id, hired_entity_id)
VALUES ($1, $2, 'workforce_report', $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13)
RETURNING id`, in.ListingID, version, body, in.Outcome, stage, reason, string(scorecard),
			authorKind, in.ApprovedBy, in.AgentID, label, in.ProjectID, in.HiredEntityID).Scan(&id); err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyDone
			}
			return fmt.Errorf("marketplace: save the report: %w", err)
		}
		return recordEvent(ctx, tx, id, in.ListingID, ActionCreated, in.ApprovedBy, "")
	})
	return id, err
}

// isWorkforceManager reports whether a published version of the agent sets
// capabilities.can_review_agents in the project.
func isWorkforceManager(ctx context.Context, db DB, projectID, agentID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, ErrInvalidRequest
	}
	var manager bool
	if err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (SELECT 1 FROM %s.application_versions
               WHERE application_id = $1 AND status IN ('published', 'embedded')
                 AND meta -> 'capabilities' -> 'can_review_agents' = 'true'::jsonb)`, schema), agentID).Scan(&manager); err != nil {
		return false, fmt.Errorf("marketplace: read the agent's capabilities: %w", err)
	}
	return manager, nil
}

// MarkDismissed records that a hired agent or skill was removed from its
// project. It changes nothing for an entity that was not hired from the
// Marketplace. by is the person acting, or zero.
func MarkDismissed(ctx context.Context, db DB, projectID int64, entityKind string, entityID, by int64) error {
	var who any
	if by > 0 {
		who = by
	}
	if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.installs SET dismissed_at = clock_timestamp(), dismissed_by = $4::bigint, updated_at = clock_timestamp()
WHERE project_id = $1 AND entity_kind = $2 AND entity_id = $3 AND dismissed_at IS NULL`,
		projectID, entityKind, entityID, who); err != nil {
		return fmt.Errorf("marketplace: record the dismissal: %w", err)
	}
	return nil
}

func itemExists(ctx context.Context, db DB, listingID, feedbackID int64) error {
	var one int
	err := db.QueryRow(ctx, `SELECT 1 FROM elitea_marketplace.listing_feedback WHERE id = $1 AND listing_id = $2`,
		feedbackID, listingID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("marketplace: read the feedback item: %w", err)
	}
	return nil
}

// Reply records the publisher's answer to an item: one per item, and only to a
// visible one. The caller has checked that who is the publisher.
func Reply(ctx context.Context, db TxDB, listingID, feedbackID int64, text string, who int64) error {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > MaxReplyBody {
		return &ParamError{Param: "text", Message: fmt.Sprintf("is required and at most %d characters", MaxReplyBody)}
	}
	return inTx(ctx, db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE elitea_marketplace.listing_feedback SET reply_body = $3, replied_at = clock_timestamp(), replied_by = $4,
    updated_at = clock_timestamp()
WHERE id = $1 AND listing_id = $2 AND reply_body IS NULL AND state = 'visible'`, feedbackID, listingID, text, who)
		if err != nil {
			return fmt.Errorf("marketplace: save the reply: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var replied bool
			err := tx.QueryRow(ctx, `SELECT reply_body IS NOT NULL FROM elitea_marketplace.listing_feedback
WHERE id = $1 AND listing_id = $2 AND state = 'visible'`, feedbackID, listingID).Scan(&replied)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			return ErrAlreadyDone
		}
		return recordEvent(ctx, tx, feedbackID, listingID, ActionReply, who, "")
	})
}

// Flag marks an item for an administrator, or clears the mark. A second flag on
// a flagged item changes nothing.
func Flag(ctx context.Context, db TxDB, listingID, feedbackID int64, flagged bool, reason string, who int64) error {
	reason = clamp(strings.TrimSpace(reason), MaxReason)
	return inTx(ctx, db, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		action := ActionFlag
		if flagged {
			tag, err = tx.Exec(ctx, `
UPDATE elitea_marketplace.listing_feedback SET flagged_at = clock_timestamp(), flagged_by = $3, flag_reason = $4,
    updated_at = clock_timestamp()
WHERE id = $1 AND listing_id = $2 AND flagged_at IS NULL`, feedbackID, listingID, who, reason)
		} else {
			action = ActionUnflag
			tag, err = tx.Exec(ctx, `
UPDATE elitea_marketplace.listing_feedback SET flagged_at = NULL, flagged_by = NULL, flag_reason = '',
    updated_at = clock_timestamp()
WHERE id = $1 AND listing_id = $2 AND flagged_at IS NOT NULL`, feedbackID, listingID)
		}
		if err != nil {
			return fmt.Errorf("marketplace: flag the item: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return itemExists(ctx, tx, listingID, feedbackID)
		}
		return recordEvent(ctx, tx, feedbackID, listingID, action, who, reason)
	})
}

// SetHidden hides an item from everyone but its author and the moderators, or
// shows it again. A hidden review leaves the rating. The caller has checked
// that who is a platform administrator. Every call that changes the item writes
// an audit event in the same transaction.
func SetHidden(ctx context.Context, db TxDB, listingID, feedbackID int64, hidden bool, reason string, who int64) error {
	reason = clamp(strings.TrimSpace(reason), MaxReason)
	return inTx(ctx, db, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		action := ActionHide
		if hidden {
			tag, err = tx.Exec(ctx, `
UPDATE elitea_marketplace.listing_feedback SET state = 'hidden', hidden_at = clock_timestamp(), hidden_by = $3,
    hide_reason = $4, updated_at = clock_timestamp()
WHERE id = $1 AND listing_id = $2 AND state = 'visible'`, feedbackID, listingID, who, reason)
		} else {
			action = ActionUnhide
			tag, err = tx.Exec(ctx, `
UPDATE elitea_marketplace.listing_feedback SET state = 'visible', hidden_at = NULL, hidden_by = NULL,
    hide_reason = '', updated_at = clock_timestamp()
WHERE id = $1 AND listing_id = $2 AND state = 'hidden'`, feedbackID, listingID)
		}
		if err != nil {
			return fmt.Errorf("marketplace: hide the item: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return itemExists(ctx, tx, listingID, feedbackID)
		}
		if err := recordEvent(ctx, tx, feedbackID, listingID, action, who, reason); err != nil {
			return err
		}
		return RefreshRating(ctx, tx, listingID)
	})
}

// FeedbackAuthor is who an item is from, as the reader may see it.
type FeedbackAuthor struct {
	Kind string `json:"kind"`
	// Name is a person's name for a review. For a workforce report it is the
	// label "Workforce report from a hiring project". It is never a project's
	// name.
	Name string `json:"name"`
}

// FeedbackReply is the publisher's answer.
type FeedbackReply struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// FeedbackItem is one item as the API returns it. It has no project field.
type FeedbackItem struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	VersionID   string          `json:"version_id"`
	VersionName string          `json:"version_name"`
	Rating      *int            `json:"rating"`
	Body        string          `json:"body"`
	Outcome     string          `json:"outcome,omitempty"`
	Stage       string          `json:"stage,omitempty"`
	ReasonCode  string          `json:"reason_code,omitempty"`
	Scorecard   json.RawMessage `json:"scorecard,omitempty"`
	Author      FeedbackAuthor  `json:"author"`
	State       string          `json:"state"`
	Flagged     bool            `json:"flagged"`
	FlagReason  string          `json:"flag_reason,omitempty"`
	HideReason  string          `json:"hide_reason,omitempty"`
	Reply       *FeedbackReply  `json:"reply"`
	// Mine is true for a review the caller wrote and for a report their
	// project wrote.
	Mine      bool      `json:"mine"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkforceReportLabel is how a report names its source.
const WorkforceReportLabel = "Workforce report from a hiring project"

// RatingBucket is the rating distribution and the report count of one version.
type RatingBucket struct {
	VersionID   string   `json:"version_id"`
	VersionName string   `json:"version_name"`
	RatingAvg   *float64 `json:"rating_avg"`
	RatingCount int      `json:"rating_count"`
	// Distribution holds the number of reviews with one star, two, up to five.
	Distribution [5]int `json:"distribution"`
	ReportCount  int    `json:"report_count"`
}

// FeedbackQuery selects a page of items.
type FeedbackQuery struct {
	ListingID int64
	// VersionID filters to one version; zero is every version.
	VersionID int64
	// Kind filters to review, workforce_report or signal; empty is all.
	Kind   string
	Limit  int
	Offset int
	// UserID and ProjectID are the viewer's. They decide which items are the
	// viewer's own, and a hidden item shows to its own author.
	UserID    int64
	ProjectID int64
	// Moderator sees hidden items and the reasons; Publisher sees the flag
	// reasons.
	Moderator bool
	Publisher bool
	// AuthorUserID narrows the items to the ones that person wrote; zero is
	// everyone. The caller's own review is read this way, past the first page.
	AuthorUserID int64
}

// FeedbackPage is a page of items with the rating summary of the listing.
type FeedbackPage struct {
	Items    []FeedbackItem `json:"items"`
	Total    int            `json:"total"`
	Versions []RatingBucket `json:"versions"`
	// Overall holds the same figures for every version together.
	Overall RatingBucket `json:"overall"`
}

const feedbackFilterSQL = `
WHERE f.listing_id = $1 AND ($2::bigint = 0 OR f.version_id = $2) AND ($3::text = '' OR f.kind = $3)
  AND (f.state = 'visible' OR $4::boolean
       OR (f.kind = 'review' AND f.author_user_id = $5) OR (f.kind <> 'review' AND f.project_id = $6))
  AND ($7::bigint = 0 OR f.author_user_id = $7)`

// ListFeedback reads a page of the listing's items, newest first, with the
// rating distribution of each version. Hidden items show only to a moderator
// and to their author. No row carries a project id or name.
func ListFeedback(ctx context.Context, db DB, q FeedbackQuery) (FeedbackPage, error) {
	switch q.Kind {
	case "", FeedbackReview, FeedbackReport, FeedbackSignal:
	default:
		return FeedbackPage{}, &ParamError{Param: "kind", Message: "must be review, workforce_report or signal"}
	}
	if q.Limit <= 0 {
		q.Limit = DefaultFeedbackLimit
	}
	if q.Limit > MaxFeedbackLimit {
		q.Limit = MaxFeedbackLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	page := FeedbackPage{Items: []FeedbackItem{}, Versions: []RatingBucket{}}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM elitea_marketplace.listing_feedback f`+feedbackFilterSQL,
		q.ListingID, q.VersionID, q.Kind, q.Moderator, q.UserID, q.ProjectID, q.AuthorUserID).Scan(&page.Total); err != nil {
		return FeedbackPage{}, fmt.Errorf("marketplace: count the feedback: %w", err)
	}
	rows, err := db.Query(ctx, `
SELECT f.id, f.kind, f.version_id, v.name, f.rating, f.body, COALESCE(f.outcome, ''), f.stage, f.reason_code, f.scorecard,
       f.author_kind, COALESCE(NULLIF(u.name, ''), 'A person'), f.state, f.flagged_at IS NOT NULL, f.flag_reason, f.hide_reason,
       f.reply_body, f.replied_at,
       (f.kind = 'review' AND f.author_user_id = $5) OR (f.kind <> 'review' AND f.project_id = $6),
       f.created_at, f.updated_at
FROM elitea_marketplace.listing_feedback f
JOIN elitea_marketplace.listing_versions v ON v.listing_id = f.listing_id AND v.version_id = f.version_id
LEFT JOIN auth_core__user u ON u.id = f.author_user_id AND f.kind = 'review'`+feedbackFilterSQL+`
ORDER BY f.created_at DESC, f.id DESC LIMIT $8 OFFSET $9`,
		q.ListingID, q.VersionID, q.Kind, q.Moderator, q.UserID, q.ProjectID, q.AuthorUserID, q.Limit, q.Offset)
	if err != nil {
		return FeedbackPage{}, fmt.Errorf("marketplace: read the feedback: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanFeedbackItem(rows, q)
		if err != nil {
			return FeedbackPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return FeedbackPage{}, err
	}
	page.Versions, page.Overall, err = ratingBuckets(ctx, db, q.ListingID)
	return page, err
}

func scanFeedbackItem(rows pgx.Rows, q FeedbackQuery) (FeedbackItem, error) {
	var (
		item       FeedbackItem
		id, ver    int64
		rating     *int16
		scorecard  []byte
		authorKind string
		name       string
		flagged    bool
		reply      *string
		repliedAt  *time.Time
		flagReason string
		hideReason string
	)
	if err := rows.Scan(&id, &item.Kind, &ver, &item.VersionName, &rating, &item.Body, &item.Outcome, &item.Stage,
		&item.ReasonCode, &scorecard, &authorKind, &name, &item.State, &flagged, &flagReason, &hideReason,
		&reply, &repliedAt, &item.Mine, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return FeedbackItem{}, err
	}
	item.ID = fmt.Sprint(id)
	item.VersionID = fmt.Sprint(ver)
	if rating != nil {
		value := int(*rating)
		item.Rating = &value
	}
	item.Flagged = flagged
	if q.Moderator || q.Publisher {
		item.FlagReason = flagReason
	}
	if q.Moderator {
		item.HideReason = hideReason
	}
	if item.Kind == FeedbackReview {
		item.Author = FeedbackAuthor{Kind: AuthorPerson, Name: name}
	} else {
		item.Author = FeedbackAuthor{Kind: authorKind, Name: WorkforceReportLabel}
		item.Scorecard = scorecard
	}
	if reply != nil && repliedAt != nil {
		item.Reply = &FeedbackReply{Body: *reply, CreatedAt: *repliedAt}
	}
	return item, nil
}

// ratingBuckets reads the visible reviews and reports of each version, newest
// version first, and the figures of all of them together.
func ratingBuckets(ctx context.Context, db DB, listingID int64) ([]RatingBucket, RatingBucket, error) {
	rows, err := db.Query(ctx, `
SELECT v.version_id, v.name,
       (count(*) FILTER (WHERE f.kind = 'review' AND f.state = 'visible' AND f.rating = 1))::int,
       (count(*) FILTER (WHERE f.kind = 'review' AND f.state = 'visible' AND f.rating = 2))::int,
       (count(*) FILTER (WHERE f.kind = 'review' AND f.state = 'visible' AND f.rating = 3))::int,
       (count(*) FILTER (WHERE f.kind = 'review' AND f.state = 'visible' AND f.rating = 4))::int,
       (count(*) FILTER (WHERE f.kind = 'review' AND f.state = 'visible' AND f.rating = 5))::int,
       (count(*) FILTER (WHERE f.kind = 'workforce_report' AND f.state = 'visible'))::int
FROM elitea_marketplace.listing_versions v
LEFT JOIN elitea_marketplace.listing_feedback f ON f.listing_id = v.listing_id AND f.version_id = v.version_id
WHERE v.listing_id = $1
GROUP BY v.version_id, v.name, v.published_at
ORDER BY v.published_at DESC, v.version_id DESC`, listingID)
	if err != nil {
		return nil, RatingBucket{}, fmt.Errorf("marketplace: read the rating distribution: %w", err)
	}
	defer rows.Close()
	buckets := []RatingBucket{}
	overall := RatingBucket{VersionID: "0"}
	for rows.Next() {
		var bucket RatingBucket
		var id int64
		if err := rows.Scan(&id, &bucket.VersionName, &bucket.Distribution[0], &bucket.Distribution[1], &bucket.Distribution[2],
			&bucket.Distribution[3], &bucket.Distribution[4], &bucket.ReportCount); err != nil {
			return nil, RatingBucket{}, err
		}
		bucket.VersionID = fmt.Sprint(id)
		bucket.finish()
		for i, n := range bucket.Distribution {
			overall.Distribution[i] += n
		}
		overall.ReportCount += bucket.ReportCount
		buckets = append(buckets, bucket)
	}
	overall.finish()
	return buckets, overall, rows.Err()
}

// finish fills the count and the average from the distribution.
func (b *RatingBucket) finish() {
	sum := 0
	b.RatingCount = 0
	for i, n := range b.Distribution {
		b.RatingCount += n
		sum += n * (i + 1)
	}
	if b.RatingCount > 0 {
		avg := float64(sum) / float64(b.RatingCount)
		avg = float64(int(avg*100+0.5)) / 100
		b.RatingAvg = &avg
	}
}

// MyReview reads the person's own review of the listing, wherever it falls in
// the list, or nil when they wrote none. A hidden review is still theirs.
func MyReview(ctx context.Context, db DB, listingID, userID, projectID int64) (*FeedbackItem, error) {
	if userID <= 0 {
		return nil, nil
	}
	page, err := ListFeedback(ctx, db, FeedbackQuery{
		ListingID: listingID, Kind: FeedbackReview, Limit: 1, UserID: userID, ProjectID: projectID, AuthorUserID: userID,
	})
	if err != nil || len(page.Items) == 0 {
		return nil, err
	}
	return &page.Items[0], nil
}

// ReviewVersion is a version the project hired and its people may review.
type ReviewVersion struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReviewableVersions lists the versions of the listing the project hired,
// newest first, including a hire that was later dismissed.
func ReviewableVersions(ctx context.Context, db DB, projectID, listingID int64) ([]ReviewVersion, error) {
	rows, err := db.Query(ctx, `
SELECT DISTINCT v.version_id, v.name, v.published_at
FROM elitea_marketplace.installs i
JOIN elitea_marketplace.listing_versions v ON v.listing_id = i.listing_id AND v.version_id = i.version_id
WHERE i.project_id = $1 AND i.listing_id = $2
ORDER BY v.published_at DESC, v.version_id DESC`, projectID, listingID)
	if err != nil {
		return nil, fmt.Errorf("marketplace: read the reviewable versions: %w", err)
	}
	defer rows.Close()
	out := []ReviewVersion{}
	for rows.Next() {
		var id int64
		var name string
		var published time.Time
		if err := rows.Scan(&id, &name, &published); err != nil {
			return nil, err
		}
		out = append(out, ReviewVersion{ID: fmt.Sprint(id), Name: name})
	}
	return out, rows.Err()
}
