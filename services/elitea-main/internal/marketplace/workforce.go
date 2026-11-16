package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Workforce managers on the Marketplace (ADR-0051 decision 6, issue 1632).
//
// A workforce manager's two proposals live in
// elitea_lifecycle.agent_workforce_proposals (shared 0253): a hire_listing
// that a person turns into a hire, and a feedback_report that a person
// publishes. This file is the Marketplace side of both: publishing a drafted
// report, and settling a hire proposal when the hire is made.
//
// A report is published only from its own proposal. The proposal names the
// hired agent, the workforce manager that drafted it (the agent that ran
// suggest_feedback), the stage and the platform-filled scorecard summary, so a
// caller cannot publish a report as an agent that did not draft one.

// ErrProposalNotOpen answers a decision on a proposal that was decided already,
// or is not of the kind the call is for.
var ErrProposalNotOpen = errors.New("marketplace: the proposal is not open")

// PublishReportInput is the decision on one drafted report.
type PublishReportInput struct {
	ProjectID  int64
	ProposalID int64
	// ListingID, when not zero, must be the listing the proposal reports on.
	ListingID int64
	// ApprovedBy is the person who approves the report, or, under the
	// auto_feedback policy, the person who turned the policy on (Policy true).
	ApprovedBy int64
	Policy     bool
	// Text, when not nil, replaces the agent's draft: the person edited it.
	Text *string
}

// PublishReportProposal publishes the workforce report of a feedback_report
// proposal to the listing version the hired agent came from, and settles the
// proposal as approved, in one transaction. The guards of SaveWorkforceReport
// hold: the drafting agent is still a workforce manager of the project, the
// project hired the listing and did not publish it, and there is one report
// per hired agent and stage. It answers the id of the feedback item.
func PublishReportProposal(ctx context.Context, db TxDB, in PublishReportInput) (int64, error) {
	if in.ProjectID <= 0 || in.ProposalID <= 0 || in.ApprovedBy <= 0 {
		return 0, ErrInvalidRequest
	}
	var feedbackID int64
	err := inTx(ctx, db, func(tx pgx.Tx) error {
		var (
			listingID, hired int64
			proposer         *int64
			stage, text      string
			status           string
			payload          []byte
		)
		err := tx.QueryRow(ctx, `
SELECT listing_id, COALESCE(application_id, 0), proposer_application_id, COALESCE(stage, ''), rationale, status, payload
FROM elitea_lifecycle.agent_workforce_proposals
WHERE project_id = $1 AND id = $2 AND kind = 'feedback_report' FOR UPDATE`, in.ProjectID, in.ProposalID).
			Scan(&listingID, &hired, &proposer, &stage, &text, &status, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("marketplace: read the report proposal: %w", err)
		}
		if status != "proposed" && status != "asked_owner" {
			return ErrProposalNotOpen
		}
		if proposer == nil || *proposer <= 0 || (in.ListingID > 0 && in.ListingID != listingID) {
			return ErrInvalidRequest
		}
		var draft struct {
			Outcome    string          `json:"outcome"`
			ReasonCode string          `json:"reason_code"`
			Scorecard  json.RawMessage `json:"scorecard"`
		}
		if err := json.Unmarshal(payload, &draft); err != nil {
			return fmt.Errorf("marketplace: read the report draft: %w", err)
		}
		edited := false
		if in.Text != nil {
			text, edited = strings.TrimSpace(*in.Text), true
		}
		feedbackID, err = SaveWorkforceReport(ctx, tx, ReportInput{
			ListingID: listingID, ProjectID: in.ProjectID, HiredEntityID: hired, AgentID: *proposer,
			ApprovedBy: in.ApprovedBy, Policy: in.Policy, Outcome: draft.Outcome, Stage: stage,
			ReasonCode: draft.ReasonCode, Body: text, Scorecard: draft.Scorecard,
		})
		if err != nil {
			return err
		}
		applied, _ := json.Marshal(map[string]any{"feedback_id": feedbackID, "policy": in.Policy, "edited": edited})
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals
SET status = 'approved', decided_by_user_id = $3, decided_at = clock_timestamp(), applied = $4::jsonb,
    rationale = CASE WHEN $5 THEN $6 ELSE rationale END, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, in.ProjectID, in.ProposalID, in.ApprovedBy, string(applied), edited, text); err != nil {
			return fmt.Errorf("marketplace: settle the report proposal: %w", err)
		}
		return nil
	})
	return feedbackID, err
}

// SettleHireProposalTx marks the open hire_listing proposal as approved by the
// person who made the hire, in the hire's own transaction. The proposal must
// be of the same project and name the same listing: a hire cannot settle
// another listing's proposal. A person's hire may pick another version than
// the one the agent named (versionID 0); the policy's hire passes the
// proposal's own version, because the conditions it checked were that version's.
// applied records the agent that was hired.
func SettleHireProposalTx(ctx context.Context, tx DB, projectID, proposalID, listingID, versionID, decidedBy int64, applied map[string]any) error {
	if projectID <= 0 || proposalID <= 0 || decidedBy <= 0 {
		return ErrInvalidRequest
	}
	var (
		listing, version int64
		status           string
	)
	err := tx.QueryRow(ctx, `
SELECT listing_id, listing_version_id, status
FROM elitea_lifecycle.agent_workforce_proposals
WHERE project_id = $1 AND id = $2 AND kind = 'hire_listing' FOR UPDATE`, projectID, proposalID).Scan(&listing, &version, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("marketplace: read the hire proposal: %w", err)
	}
	if status != "proposed" && status != "asked_owner" {
		return ErrProposalNotOpen
	}
	if listing != listingID || (versionID > 0 && version != versionID) {
		return ErrInvalidRequest
	}
	// The wizard may hire another version of the same listing than the one the
	// agent named; the proposal records what was hired.
	encoded, _ := json.Marshal(applied)
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals
SET status = 'approved', decided_by_user_id = $3, decided_at = clock_timestamp(), applied = $4::jsonb,
    updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, projectID, proposalID, decidedBy, string(encoded)); err != nil {
		return fmt.Errorf("marketplace: settle the hire proposal: %w", err)
	}
	return nil
}

// VersionReviewStats answers the number and the average rating of the visible
// reviews of one listing version: what the auto_hire rule reads.
func VersionReviewStats(ctx context.Context, db DB, listingID, versionID int64) (int, float64, error) {
	var (
		count int
		avg   *float64
	)
	if err := db.QueryRow(ctx, `
SELECT count(*)::int, avg(rating)::float8
FROM elitea_marketplace.listing_feedback
WHERE listing_id = $1 AND version_id = $2 AND kind = 'review' AND state = 'visible'`, listingID, versionID).Scan(&count, &avg); err != nil {
		return 0, 0, fmt.Errorf("marketplace: read the review figures: %w", err)
	}
	if avg == nil {
		return count, 0, nil
	}
	return count, *avg, nil
}
