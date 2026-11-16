package repos

// The workforce proposal table after shared/0253 (ADR-0051 decision 6, issue
// 1632) against a REAL migrated database: the Marketplace kinds beside the
// ladder's, the indexes that keep one open ladder proposal per agent, one
// report per hired agent and stage, and one open hire per listing, and the
// policy columns. It SKIPS without ELITEA_TEST_DATABASE_URL, so each case
// asserts values a skip could not have produced.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	pgCheckViolation  = "23514"
	pgUniqueViolation = "23505"
)

// insertProposal writes one proposal row as the Marketplace tools or a person
// would, and answers the database's refusal. A workforce manager's proposal
// carries its agent and run; a person's carries the person.
func insertProposal(pool *pgxpool.Pool, kind string, application, stage any, source string, listing any) error {
	var proposer, execution, person any
	if source == "workforce_manager" {
		proposer, execution = int64(9001), "exec-1"
	} else {
		person = int64(7)
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO elitea_lifecycle.agent_workforce_proposals
    (project_id, application_id, kind, stage, source, proposed_by_user_id, proposer_application_id, execution_id,
     rationale, evidence_digest, listing_id, listing_version_id)
VALUES (1, $1, $2, $3, $4, $5, $6, $7, 'because', $8, $9, $9)`,
		application, kind, stage, source, person, proposer, execution, strings.Repeat("b", 64), listing)
	return err
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func TestMarketplaceProposalKindsAndIndexes(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	// A ladder proposal keeps its rules: one open per agent, a stage, an agent.
	if err := insertProposal(pool, "dismiss", int64(7001), "watch", "person", nil); err != nil {
		t.Fatalf("the ladder proposal: %v", err)
	}
	if err := insertProposal(pool, "dismiss", int64(7001), "retire", "person", nil); !hasCode(err, pgUniqueViolation) {
		t.Fatalf("a second open ladder proposal for the agent = %v, want a unique violation", err)
	}
	if err := insertProposal(pool, "demote", int64(7002), "watch", "person", nil); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a demote kind with another stage = %v, want a check violation", err)
	}
	if err := insertProposal(pool, "dismiss", nil, "watch", "person", nil); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a ladder proposal with no agent = %v, want a check violation", err)
	}

	// feedback_report: the hired agent and a stage; one per agent and stage
	// while open or published, drafted beside the agent's open dismissal.
	if err := insertProposal(pool, "feedback_report", int64(7001), "watch", "workforce_manager", int64(55)); err != nil {
		t.Fatalf("a report beside the open dismissal: %v", err)
	}
	if err := insertProposal(pool, "feedback_report", int64(7001), "watch", "workforce_manager", int64(55)); !hasCode(err, pgUniqueViolation) {
		t.Fatalf("a second report for the hired agent and stage = %v, want a unique violation", err)
	}
	if err := insertProposal(pool, "feedback_report", int64(7001), "retire", "workforce_manager", int64(55)); err != nil {
		t.Fatalf("a report for another stage: %v", err)
	}
	if err := insertProposal(pool, "feedback_report", int64(7001), "demote", "person", int64(55)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a report from a person = %v, want a check violation: Marketplace proposals are a workforce manager's", err)
	}
	if err := insertProposal(pool, "feedback_report", int64(7001), "suspend", "workforce_manager", nil); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a report with no listing = %v, want a check violation", err)
	}
	if err := insertProposal(pool, "feedback_report", nil, "suspend", "workforce_manager", int64(55)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a report with no hired agent = %v, want a check violation", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals SET status = 'rejected', decided_at = now(), decided_by_user_id = 7
WHERE kind = 'feedback_report' AND stage = 'watch'`); err != nil {
		t.Fatal(err)
	}
	if err := insertProposal(pool, "feedback_report", int64(7001), "watch", "workforce_manager", int64(55)); err != nil {
		t.Fatalf("a report after the first was discarded: %v", err)
	}

	// hire_listing: no agent, no stage, one open per listing.
	if err := insertProposal(pool, "hire_listing", nil, nil, "workforce_manager", int64(56)); err != nil {
		t.Fatalf("a hire proposal: %v", err)
	}
	if err := insertProposal(pool, "hire_listing", nil, nil, "workforce_manager", int64(56)); !hasCode(err, pgUniqueViolation) {
		t.Fatalf("a second open hire for the listing = %v, want a unique violation", err)
	}
	if err := insertProposal(pool, "hire_listing", nil, nil, "workforce_manager", int64(57)); err != nil {
		t.Fatalf("a hire of another listing: %v", err)
	}
	if err := insertProposal(pool, "hire_listing", int64(7001), nil, "workforce_manager", int64(58)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a hire proposal that names an agent = %v, want a check violation", err)
	}
	if err := insertProposal(pool, "hire_listing", nil, "watch", "workforce_manager", int64(58)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a hire proposal with a stage = %v, want a check violation", err)
	}
	if err := insertProposal(pool, "hire_listing", nil, nil, "person", int64(58)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("a hire proposal from a person = %v, want a check violation", err)
	}
	if err := insertProposal(pool, "approve_everything", nil, nil, "workforce_manager", int64(58)); !hasCode(err, pgCheckViolation) {
		t.Fatalf("an unknown kind = %v, want a check violation", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals SET status = 'approved', decided_at = now(), decided_by_user_id = 7
WHERE kind = 'hire_listing' AND listing_id = 56`); err != nil {
		t.Fatal(err)
	}
	if err := insertProposal(pool, "hire_listing", nil, nil, "workforce_manager", int64(56)); err != nil {
		t.Fatalf("a hire of the listing after the first was settled: %v", err)
	}

	// The policy's new columns default off and to the ADR's numbers.
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_lifecycle.agent_workforce_policies (project_id) VALUES (91)`); err != nil {
		t.Fatal(err)
	}
	var (
		enabled         bool
		reviews         int
		rating, dismiss float64
	)
	if err := pool.QueryRow(ctx, `
SELECT auto_feedback_enabled, listing_min_reviews, listing_min_rating::float8, listing_max_dismissal_rate::float8
FROM elitea_lifecycle.agent_workforce_policies WHERE project_id = 91`).Scan(&enabled, &reviews, &rating, &dismiss); err != nil ||
		enabled || reviews != 3 || rating != 4.0 || dismiss != 0.2 {
		t.Fatalf("the policy defaults = %v %d %v %v (%v), want off, 3, 4.0, 0.2", enabled, reviews, rating, dismiss, err)
	}
}
