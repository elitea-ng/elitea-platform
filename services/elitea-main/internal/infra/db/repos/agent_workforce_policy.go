package repos

// The workforce policy (shared 0237, ADR-0050 decisions 5c and 5k, issue
// 1617): a project's automatic hiring and automatic dismissal, OFF by
// default. The pass (WorkforcePolicyPass) runs from the
// main.agent_workforce_policy routine; a person sets the policy and acts on
// what it did (undo or keep a hire, object to a retire notice).
//
// NO PARALLEL PATH. Every stage the policy takes is a row of
// agent_workforce_proposals (source policy) applied by the ladder of
// agent_workforce_ladder.go (applyStage), in one transaction with its audit
// event (actor policy:<rule>), exactly as a person's approval applies it.
// Demote and delete are never automatic (CHECK agent_workforce_proposals_
// policy_stage and agentworkforce.AutomaticStage); a critical-tier agent or
// one under legal hold is skipped (agentworkforce.ExemptFromAutomatic).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentsoul"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// WorkforcePolicy is a project's policy as the settings page reads it.
type WorkforcePolicy struct {
	agentworkforce.Policy
	ProjectID int64 `json:"project_id"`
	// Saved: an agent manager saved a policy for the project (false: the
	// defaults, everything off).
	Saved     bool       `json:"saved"`
	UpdatedBy *int64     `json:"updated_by"`
	UpdatedAt *time.Time `json:"updated_at"`
	// DefaultRules are the platform's starting rules.
	DefaultRules []agentworkforce.Rule `json:"default_rules"`
	// OpenAutoHired counts the automatically hired agents nobody kept yet
	// (max_auto_hired bounds it).
	OpenAutoHired int `json:"open_auto_hired"`
}

func readPolicy(ctx context.Context, q lifecycleQuerier, projectID int64) (WorkforcePolicy, error) {
	policy := WorkforcePolicy{Policy: agentworkforce.DefaultPolicy(), ProjectID: projectID, DefaultRules: agentworkforce.DefaultRules()}
	var rules []byte
	err := q.QueryRow(ctx, `
SELECT auto_hire_enabled, min_successful_runs, max_auto_hired, auto_dismiss_enabled, auto_dismiss_rules, grace_days,
       updated_by, updated_at
FROM elitea_lifecycle.agent_workforce_policies WHERE project_id = $1`, projectID).Scan(
		&policy.AutoHireEnabled, &policy.MinSuccessfulRuns, &policy.MaxAutoHired, &policy.AutoDismissEnabled, &rules,
		&policy.GraceDays, &policy.UpdatedBy, &policy.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return WorkforcePolicy{}, fmt.Errorf("agent workforce: read the policy: %w", err)
	default:
		policy.Saved = true
		policy.Rules = []agentworkforce.Rule{}
		if err := json.Unmarshal(rules, &policy.Rules); err != nil {
			return WorkforcePolicy{}, fmt.Errorf("agent workforce: decode the policy rules: %w", err)
		}
	}
	if err := q.QueryRow(ctx, openAutoHiredSQL, projectID).Scan(&policy.OpenAutoHired); err != nil {
		return WorkforcePolicy{}, fmt.Errorf("agent workforce: count the automatic hires: %w", err)
	}
	return policy, nil
}

// openAutoHiredSQL counts a project's automatically hired agents nobody has
// kept (or undone) yet.
const openAutoHiredSQL = `
SELECT count(*) FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND auto_hired_at IS NOT NULL AND probation_reviewed_at IS NULL
  AND lifecycle_state = 'active'`

// Policy answers the project's policy, the defaults when none was saved.
func (s *AgentWorkforce) Policy(ctx context.Context, projectID int64) (WorkforcePolicy, error) {
	return readPolicy(ctx, s.pool, projectID)
}

// SetPolicy saves the project's policy. Only an agent manager may. Turning
// automatic dismissal off cancels its pending retire notices and orphan
// flags (and closes their notices), so nothing waits to apply under a policy
// that is off.
func (s *AgentWorkforce) SetPolicy(ctx context.Context, projectID int64, actor WorkforceActor, input agentworkforce.Policy) (WorkforcePolicy, error) {
	if !actor.Manager || actor.UserID <= 0 {
		return WorkforcePolicy{}, agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager sets the workforce policy.")
	}
	policy, err := agentworkforce.CheckPolicy(input)
	if err != nil {
		return WorkforcePolicy{}, err
	}
	rules, err := json.Marshal(policy.Rules)
	if err != nil {
		return WorkforcePolicy{}, fmt.Errorf("agent workforce: encode the rules: %w", err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicyProject(ctx, tx, projectID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_workforce_policies
    (project_id, auto_hire_enabled, min_successful_runs, max_auto_hired, auto_dismiss_enabled, auto_dismiss_rules,
     grace_days, updated_by)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
ON CONFLICT (project_id) DO UPDATE SET
    auto_hire_enabled = EXCLUDED.auto_hire_enabled, min_successful_runs = EXCLUDED.min_successful_runs,
    max_auto_hired = EXCLUDED.max_auto_hired, auto_dismiss_enabled = EXCLUDED.auto_dismiss_enabled,
    auto_dismiss_rules = EXCLUDED.auto_dismiss_rules, grace_days = EXCLUDED.grace_days,
    updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp()`,
			projectID, policy.AutoHireEnabled, policy.MinSuccessfulRuns, policy.MaxAutoHired, policy.AutoDismissEnabled,
			string(rules), policy.GraceDays, actor.UserID); err != nil {
			return fmt.Errorf("agent workforce: save the policy: %w", err)
		}
		if !policy.AutoDismissEnabled {
			if err := s.cancelOpenActions(ctx, tx, projectID, "Automatic dismissal was turned off."); err != nil {
				return err
			}
		}
		return s.append(ctx, tx, projectID, "agent.workforce.policy_changed", personRef(actor.UserID), strconv.FormatInt(projectID, 10), "",
			fmt.Sprintf("auto_hire:%t", policy.AutoHireEnabled), fmt.Sprintf("auto_dismiss:%t", policy.AutoDismissEnabled),
			fmt.Sprintf("rules:%d", len(policy.Rules)), fmt.Sprintf("grace_days:%d", policy.GraceDays),
			fmt.Sprintf("max_auto_hired:%d", policy.MaxAutoHired), fmt.Sprintf("min_successful_runs:%d", policy.MinSuccessfulRuns))
	})
	if err != nil {
		return WorkforcePolicy{}, err
	}
	return s.Policy(ctx, projectID)
}

// cancelOpenActions cancels the project's open actions (pending, flagged and
// held), closes their open policy notices (rejected, decided by nobody: the
// policy that raised them is off) and clears the orphan flag of the agents
// whose orphan action it cancels, so a later pass can flag them again.
func (s *AgentWorkforce) cancelOpenActions(ctx context.Context, tx pgx.Tx, projectID int64, note string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET status = 'rejected', decided_at = clock_timestamp(), decision_note = $2, updated_at = clock_timestamp()
FROM elitea_lifecycle.agent_automatic_actions AS a
WHERE a.project_id = $1 AND a.status IN ('pending', 'flagged', 'held') AND a.kind <> 'auto_hire'
  AND p.project_id = a.project_id AND p.id = a.proposal_id AND p.source = 'policy'
  AND p.status IN ('proposed', 'asked_owner')`, projectID, note); err != nil {
		return fmt.Errorf("agent workforce: close the policy notices: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry AS r
SET orphaned_since = NULL, updated_at = clock_timestamp()
FROM elitea_lifecycle.agent_automatic_actions AS a
WHERE a.project_id = $1 AND a.kind = 'orphan' AND a.status IN ('pending', 'flagged', 'held')
  AND r.tenant_project_id = a.project_id AND r.application_id = a.application_id`, projectID); err != nil {
		return fmt.Errorf("agent workforce: clear the orphan flags: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions
SET status = 'cancelled', note = $2, updated_at = clock_timestamp()
WHERE project_id = $1 AND status IN ('pending', 'flagged', 'held')`, projectID, note); err != nil {
		return fmt.Errorf("agent workforce: cancel the open automatic actions: %w", err)
	}
	return nil
}

func lockPolicyProject(ctx context.Context, tx pgx.Tx, projectID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`,
		fmt.Sprintf("elitea_workforce_policy:%d", projectID))
	return err
}

/* ── automatic actions ─────────────────────────────────────────────────── */

// AutomaticAction is one thing the policy did or noticed.
type AutomaticAction struct {
	ID              int64          `json:"id"`
	ProjectID       int64          `json:"project_id"`
	ApplicationID   int64          `json:"application_id"`
	ApplicationName string         `json:"application_name"`
	Kind            string         `json:"kind"`
	Rule            string         `json:"rule"`
	Stage           *string        `json:"stage"`
	ProposalID      *int64         `json:"proposal_id"`
	Status          string         `json:"status"`
	DueAt           *time.Time     `json:"due_at"`
	Evidence        map[string]any `json:"evidence"`
	EvidenceDigest  string         `json:"evidence_digest"`
	Note            *string        `json:"note"`
	DecidedByUserID *int64         `json:"decided_by_user_id"`
	DecidedAt       *time.Time     `json:"decided_at"`
	AppliedAt       *time.Time     `json:"applied_at"`
	CreatedAt       time.Time      `json:"created_at"`
	// CanUndo: an automatic hire still in its 14-day undo window.
	CanUndo bool `json:"can_undo"`
	// CanObject: a retire notice still in its grace period.
	CanObject bool `json:"can_object"`
}

const actionColumns = `
a.id, a.project_id, a.application_id, a.kind, a.rule, a.stage, a.proposal_id, a.status, a.due_at, a.evidence,
a.evidence_digest, a.note, a.decided_by_user_id, a.decided_at, a.applied_at, a.created_at`

func scanAction(row pgx.Row, now time.Time) (AutomaticAction, error) {
	var (
		action   AutomaticAction
		evidence []byte
	)
	if err := row.Scan(&action.ID, &action.ProjectID, &action.ApplicationID, &action.Kind, &action.Rule, &action.Stage,
		&action.ProposalID, &action.Status, &action.DueAt, &evidence, &action.EvidenceDigest, &action.Note,
		&action.DecidedByUserID, &action.DecidedAt, &action.AppliedAt, &action.CreatedAt); err != nil {
		return AutomaticAction{}, err
	}
	if err := json.Unmarshal(evidence, &action.Evidence); err != nil {
		return AutomaticAction{}, fmt.Errorf("agent workforce: decode the action evidence: %w", err)
	}
	inWindow := action.DueAt != nil && now.Before(*action.DueAt)
	action.CanUndo = action.Kind == agentworkforce.ActionAutoHire && action.Status == agentworkforce.ActionApplied && inWindow
	action.CanObject = action.Kind == agentworkforce.ActionAutoDismiss && action.Status == agentworkforce.ActionPending
	return action, nil
}

// ActionFilter narrows the list.
type ActionFilter struct {
	// Status: "" is every status; open is pending and flagged.
	Status        string
	ApplicationID int64
	Limit, Offset int
}

// ListAutomaticActions answers the project's automatic actions, newest
// first, and the total.
func (s *AgentWorkforce) ListAutomaticActions(ctx context.Context, projectID int64, filter ActionFilter) ([]AutomaticAction, int, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+actionColumns+`, count(*) OVER ()
FROM elitea_lifecycle.agent_automatic_actions AS a
WHERE a.project_id = $1 AND ($2::bigint = 0 OR a.application_id = $2)
  AND ($3 = '' OR ($3 = 'open' AND a.status IN ('pending', 'flagged', 'held')) OR a.status = $3)
ORDER BY a.created_at DESC, a.id DESC
LIMIT $4 OFFSET $5`, projectID, filter.ApplicationID, filter.Status, limit, max(filter.Offset, 0))
	if err != nil {
		return nil, 0, fmt.Errorf("agent workforce: list the automatic actions: %w", err)
	}
	defer rows.Close()
	now := s.now().UTC()
	out := []AutomaticAction{}
	total := 0
	for rows.Next() {
		action, err := scanAction(countedRow{row: rows, count: &total}, now)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, action)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("agent workforce: list the automatic actions: %w", err)
	}
	if len(out) > 0 {
		ids := make([]int64, 0, len(out))
		for _, action := range out {
			ids = append(ids, action.ApplicationID)
		}
		names := applicationNames(ctx, s.pool, projectID, ids)
		for i := range out {
			out[i].ApplicationName = names[out[i].ApplicationID]
		}
	}
	return out, total, nil
}

func (s *AgentWorkforce) readAction(ctx context.Context, q lifecycleQuerier, projectID, actionID int64, lock bool) (AutomaticAction, error) {
	statement := `SELECT ` + actionColumns + ` FROM elitea_lifecycle.agent_automatic_actions AS a WHERE a.project_id = $1 AND a.id = $2`
	if lock {
		statement += ` FOR UPDATE`
	}
	action, err := scanAction(q.QueryRow(ctx, statement, projectID, actionID), s.now().UTC())
	if errors.Is(err, pgx.ErrNoRows) {
		return AutomaticAction{}, agentworkforce.Refuse(agentworkforce.NotFoundCode, "No such automatic action in this project.")
	}
	if err != nil {
		return AutomaticAction{}, fmt.Errorf("agent workforce: read the automatic action: %w", err)
	}
	action.ApplicationName = applicationNames(ctx, q, projectID, []int64{action.ApplicationID})[action.ApplicationID]
	return action, nil
}

// GetAutomaticAction answers one action.
func (s *AgentWorkforce) GetAutomaticAction(ctx context.Context, projectID, actionID int64) (AutomaticAction, error) {
	return s.readAction(ctx, s.pool, projectID, actionID, false)
}

func (s *AgentWorkforce) closeAction(ctx context.Context, tx pgx.Tx, action AutomaticAction, status string, userID int64, note string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions
SET status = $3, decided_by_user_id = $4, decided_at = clock_timestamp(), note = NULLIF($5, ''), updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID, status, nullablePositive(userID), note); err != nil {
		return fmt.Errorf("agent workforce: close the automatic action: %w", err)
	}
	return nil
}

// UndoAutoHire undoes an automatic hire within its 14 days: the agent is
// retired by the ladder (so it can still be reinstated for 30 days and
// nothing about it is deleted), its probation ends, and the same spec is not
// hired automatically again for 30 days. Only an agent manager may.
func (s *AgentWorkforce) UndoAutoHire(ctx context.Context, projectID, actionID int64, actor WorkforceActor, note string, confirmNested bool) (AutomaticAction, error) {
	return s.decideHire(ctx, projectID, actionID, actor, note, true, confirmNested)
}

// KeepAutoHire ends an automatic hire's probation: an agent manager reviewed
// it and keeps it. It stops counting against max_auto_hired.
func (s *AgentWorkforce) KeepAutoHire(ctx context.Context, projectID, actionID int64, actor WorkforceActor, note string) (AutomaticAction, error) {
	return s.decideHire(ctx, projectID, actionID, actor, note, false, false)
}

func (s *AgentWorkforce) decideHire(ctx context.Context, projectID, actionID int64, actor WorkforceActor, note string, undo, confirmNested bool) (AutomaticAction, error) {
	if !actor.Manager || actor.UserID <= 0 {
		return AutomaticAction{}, agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager undoes or keeps an automatic hire.")
	}
	note, err := agentworkforce.CheckNote(note, agentworkforce.MaxNoteBytes)
	if err != nil {
		return AutomaticAction{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		action, err := s.readAction(ctx, tx, projectID, actionID, true)
		if err != nil {
			return err
		}
		if action.Kind != agentworkforce.ActionAutoHire || action.Status != agentworkforce.ActionApplied {
			return agentworkforce.Refuse(agentworkforce.ActionClosedCode, "Only an automatic hire nobody has reviewed can be undone or kept.")
		}
		if undo && !action.CanUndo {
			return agentworkforce.Refuse(agentworkforce.ActionClosedCode, "The 14-day undo window has closed; propose retiring the agent instead.")
		}
		agent, err := readWorkforceAgent(ctx, tx, projectID, action.ApplicationID, true)
		if err != nil {
			return err
		}
		status, verb := agentworkforce.ActionKept, "kept"
		if undo {
			status, verb = agentworkforce.ActionUndone, "undone"
			if agent.State == agentworkforce.StateActive {
				// The ladder's own retire stage, as a person's approval
				// applies it: an agent others nest needs the confirmation.
				if _, _, err := s.applyStage(ctx, tx, agent, agentworkforce.StageRetire, WorkforceProposal{},
					Decision{ProjectID: projectID, Actor: actor, ConfirmNested: confirmNested}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET probation_reviewed_at = clock_timestamp(), probation_reviewed_by = $3, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, action.ApplicationID, actor.UserID); err != nil {
			return fmt.Errorf("agent workforce: end the probation: %w", err)
		}
		if err := s.closeAction(ctx, tx, action, status, actor.UserID, note); err != nil {
			return err
		}
		return s.append(ctx, tx, projectID, "agent.workforce.auto_hire_"+verb, personRef(actor.UserID), actionRef(action.ID), "",
			"agent:"+strconv.FormatInt(action.ApplicationID, 10), "evidence:"+action.EvidenceDigest)
	})
	if err != nil {
		return AutomaticAction{}, err
	}
	return s.GetAutomaticAction(ctx, projectID, actionID)
}

// ObjectToAction is the owner's or an agent manager's objection to a retire
// notice in its grace period: the notice is rejected, the agent stays, and
// the rule stays quiet for this agent for 30 days.
func (s *AgentWorkforce) ObjectToAction(ctx context.Context, projectID, actionID int64, actor WorkforceActor, note string) (AutomaticAction, error) {
	note, err := agentworkforce.CheckNote(note, agentworkforce.MaxNoteBytes)
	if err != nil {
		return AutomaticAction{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		action, err := s.readAction(ctx, tx, projectID, actionID, true)
		if err != nil {
			return err
		}
		if !action.CanObject {
			return agentworkforce.Refuse(agentworkforce.ActionClosedCode, "Only a retire notice in its grace period can be objected to.")
		}
		agent, err := readWorkforceAgent(ctx, tx, projectID, action.ApplicationID, false)
		if err != nil {
			return err
		}
		if err := mayDecide(actor, agent); err != nil {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only the agent's owner or an agent manager can object.")
		}
		if action.ProposalID != nil {
			if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals
SET status = 'rejected', decided_by_user_id = $3, decided_at = clock_timestamp(), decision_note = NULLIF($4, ''),
    updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND status IN ('proposed', 'asked_owner')`,
				projectID, *action.ProposalID, actor.UserID, note); err != nil {
				return fmt.Errorf("agent workforce: reject the notice: %w", err)
			}
		}
		if err := s.closeAction(ctx, tx, action, agentworkforce.ActionObjected, actor.UserID, note); err != nil {
			return err
		}
		return s.append(ctx, tx, projectID, "agent.workforce.policy_objected", personRef(actor.UserID), actionRef(action.ID), "",
			"rule:"+action.Rule, "agent:"+strconv.FormatInt(action.ApplicationID, 10), "evidence:"+action.EvidenceDigest)
	})
	if err != nil {
		return AutomaticAction{}, err
	}
	return s.GetAutomaticAction(ctx, projectID, actionID)
}

func actionRef(id int64) string { return "action:" + strconv.FormatInt(id, 10) }

func policyRef(rule string) string { return "policy:" + rule }

/* ── the pass ──────────────────────────────────────────────────────────── */

// PolicySummary is what one pass did.
type PolicySummary struct {
	Projects   int
	Hired      int
	Applied    int
	Noticed    int
	Retired    int
	Held       int
	Orphans    int
	Reconciled int
}

func (s PolicySummary) String() string {
	return fmt.Sprintf("projects=%d hired=%d applied=%d noticed=%d retired=%d held=%d orphans=%d reconciled=%d",
		s.Projects, s.Hired, s.Applied, s.Noticed, s.Retired, s.Held, s.Orphans, s.Reconciled)
}

// WorkforcePolicyPass evaluates every project with a policy turned on:
// automatic hires first, then the due retire notices and orphans, then the
// rules against the latest scorecards and the orphan scan. A failure in
// one project is returned after the others ran.
func (s *AgentWorkforce) WorkforcePolicyPass(ctx context.Context) (PolicySummary, error) {
	rows, err := s.pool.Query(ctx, `
SELECT project_id FROM elitea_lifecycle.agent_workforce_policies
WHERE auto_hire_enabled OR auto_dismiss_enabled ORDER BY project_id`)
	if err != nil {
		return PolicySummary{}, fmt.Errorf("agent workforce: list the policies: %w", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return PolicySummary{}, fmt.Errorf("agent workforce: list the policies: %w", err)
	}
	var (
		summary PolicySummary
		failed  []error
	)
	for _, projectID := range projects {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		summary.Projects++
		if err := s.projectPass(ctx, projectID, &summary); err != nil {
			failed = append(failed, fmt.Errorf("project %d: %w", projectID, err))
		}
	}
	return summary, errors.Join(failed...)
}

func (s *AgentWorkforce) projectPass(ctx context.Context, projectID int64, summary *PolicySummary) error {
	policy, err := readPolicy(ctx, s.pool, projectID)
	if err != nil {
		return err
	}
	if policy.AutoHireEnabled {
		if err := s.autoHirePass(ctx, policy, summary); err != nil {
			return err
		}
	}
	if !policy.AutoDismissEnabled {
		return nil
	}
	if err := s.reconcileActions(ctx, policy, summary); err != nil {
		return err
	}
	if err := s.orphanPass(ctx, policy, summary); err != nil {
		return err
	}
	return s.rulePass(ctx, policy, summary)
}

/* ── auto hire ─────────────────────────────────────────────────────────── */

type hireCandidate struct {
	id       int64
	proposer int64
	forUser  int64
	digest   string
	draft    map[string]any
}

func (s *AgentWorkforce) autoHirePass(ctx context.Context, policy WorkforcePolicy, summary *PolicySummary) error {
	rows, err := s.pool.Query(ctx, `
SELECT id, proposer_application_id, proposed_for_user_id, spec_digest, draft
FROM elitea_lifecycle.agent_proposals
WHERE project_id = $1 AND status = 'proposed'
ORDER BY created_at, id`, policy.ProjectID)
	if err != nil {
		return fmt.Errorf("agent workforce: list the open proposals: %w", err)
	}
	var candidates []hireCandidate
	for rows.Next() {
		var (
			c     hireCandidate
			draft []byte
		)
		if err := rows.Scan(&c.id, &c.proposer, &c.forUser, &c.digest, &draft); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(draft, &c.draft); err != nil {
			rows.Close()
			return fmt.Errorf("agent workforce: decode a proposal draft: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		hired, err := s.tryHire(ctx, policy, candidate)
		if err != nil {
			return err
		}
		if hired {
			summary.Hired++
		}
	}
	return nil
}

// draftCategories answers the ADR-0041 category of each proposed tool,
// computed again from the tool keys (a wildcard or an undeclared tool is
// write), never read from what the worker reported.
func draftCategories(draft map[string]any) []string {
	tools, _ := draft["tools"].([]any)
	refs := make([]agentproposal.Tool, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		key, _ := tool["key"].(string)
		toolkitType, _ := tool["toolkit_type"].(string)
		refs = append(refs, agentproposal.Tool{Key: key, ToolkitType: toolkitType})
	}
	categories := []string{}
	for _, category := range agentproposal.Categories(refs) {
		categories = append(categories, category.Category)
	}
	return categories
}

func (s *AgentWorkforce) tryHire(ctx context.Context, policy WorkforcePolicy, candidate hireCandidate) (bool, error) {
	var (
		hired  bool
		notice AutomaticAction
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicyProject(ctx, tx, policy.ProjectID); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `
SELECT status FROM elitea_lifecycle.agent_proposals WHERE project_id = $1 AND id = $2 FOR UPDATE`,
			policy.ProjectID, candidate.id).Scan(&status); err != nil {
			return fmt.Errorf("agent workforce: lock the proposal: %w", err)
		}
		if status != string(agentproposal.StatusProposed) {
			return nil
		}
		evidence := agentworkforce.HireEvidence{ToolCategories: draftCategories(candidate.draft)}
		if err := tx.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'completed'),
       count(*) FILTER (WHERE status IN ('failed', 'budget_exhausted'))
FROM elitea_runtime.ephemeral_agents
WHERE project_id = $1 AND parent_application_id = $2 AND profile_digest = $3`,
			policy.ProjectID, candidate.proposer, candidate.digest).Scan(&evidence.Successful, &evidence.Failed); err != nil {
			return fmt.Errorf("agent workforce: count the spec's runs: %w", err)
		}
		if err := tx.QueryRow(ctx, openAutoHiredSQL, policy.ProjectID).Scan(&evidence.OpenAutoHired); err != nil {
			return fmt.Errorf("agent workforce: count the automatic hires: %w", err)
		}
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_automatic_actions
               WHERE project_id = $1 AND kind = 'auto_hire' AND status = 'undone'
                 AND evidence ->> 'spec_digest' = $2 AND decided_at > $3)`,
			policy.ProjectID, candidate.digest, s.now().UTC().Add(-agentworkforce.SuppressAfterObjection)).Scan(&evidence.Undone); err != nil {
			return fmt.Errorf("agent workforce: read the undone hires: %w", err)
		}
		if reason := agentworkforce.CheckHire(policy.Policy, evidence); reason != "" {
			return nil
		}
		applicationID, err := s.createHiredAgent(ctx, tx, policy.ProjectID, candidate)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		until := now.Add(agentworkforce.ProbationWindow)
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET risk_tier = 'low', auto_hired_at = $3, auto_hire_proposal_id = $4, probation_until = $5,
    probation_reviewed_at = NULL, probation_reviewed_by = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`,
			policy.ProjectID, applicationID, now, candidate.id, until); err != nil {
			return fmt.Errorf("agent workforce: put the hire on probation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_proposals
SET status = 'created', decided_at = $3, created_application_id = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND status = 'proposed'`, policy.ProjectID, candidate.id, now, applicationID); err != nil {
			return fmt.Errorf("agent workforce: link the proposal: %w", err)
		}
		record := map[string]any{
			"proposal_id": candidate.id, "proposer_application_id": candidate.proposer, "spec_digest": candidate.digest,
			"successful_runs": evidence.Successful, "tool_categories": evidence.ToolCategories,
			"suggested_tier": agentworkforce.SuggestedTier(evidence.ToolCategories), "owner_id": candidate.forUser,
			"probation_until": until, "unpublished": true, "tools_bound": 0,
		}
		action, err := s.insertAction(ctx, tx, policy.ProjectID, applicationID, agentworkforce.ActionAutoHire, agentworkforce.RuleAutoHire,
			"", candidate.id, agentworkforce.ActionApplied, &until, record, true)
		if err != nil {
			return err
		}
		action.ApplicationName = stringOf(candidate.draft["name"])
		if err := (&AgentProposals{audit: s.audit}).append(ctx, tx, policy.ProjectID, "agent.proposal.auto_hired",
			policyRef(agentworkforce.RuleAutoHire), strconv.FormatInt(candidate.id, 10), ""); err != nil {
			return err
		}
		if err := s.append(ctx, tx, policy.ProjectID, "agent.workforce.auto_hired", policyRef(agentworkforce.RuleAutoHire),
			actionRef(action.ID), "", "agent:"+strconv.FormatInt(applicationID, 10), "evidence:"+action.EvidenceDigest); err != nil {
			return err
		}
		hired, notice = true, action
		return nil
	})
	if err != nil || !hired {
		return false, err
	}
	s.notifyPolicy(ctx, notice, candidate.forUser,
		fmt.Sprintf("The workforce policy hired agent %q on probation. An agent manager can undo the hire for 14 days.",
			notice.ApplicationName))
	return true, nil
}

// createHiredAgent writes the agent: one DRAFT version (unpublished), no
// tool bound, memory writes held for review, the soul the proposal carried,
// and its registry row (tier low, the person the proposing run served as the
// owner), as the create route does.
func (s *AgentWorkforce) createHiredAgent(ctx context.Context, tx pgx.Tx, projectID int64, candidate hireCandidate) (int64, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return 0, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	name := strings.TrimSpace(stringOf(candidate.draft["name"]))
	if name == "" {
		name = "Proposed agent"
	}
	if runes := []rune(name); len(runes) > agentproposal.MaxNameChars {
		name = strings.TrimSpace(string(runes[:agentproposal.MaxNameChars]))
	}
	description := stringOf(candidate.draft["description"])
	if runes := []rune(description); len(runes) > 2000 {
		description = string(runes[:2000])
	}
	var soul any
	if raw, ok := candidate.draft["soul"].(map[string]any); ok {
		normalized, err := agentsoul.Normalize(raw)
		if err == nil && len(normalized) > 0 {
			soul = string(normalized)
		}
	}
	memory := agentmemory.DefaultNew
	memory.Write = agentmemory.WriteReview
	meta, err := json.Marshal(map[string]any{
		"step_limit": 25, "internal_tools": []any{}, agentmemory.MetaKey: memory.Block(),
		"workforce": map[string]any{"auto_hired": true, "proposal_id": candidate.id},
	})
	if err != nil {
		return 0, err
	}
	var applicationID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, owner_id, meta, created_at, updated_at)
VALUES ($1, $2, $3, '{}'::jsonb, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
RETURNING id`, schema), name, description, projectID).Scan(&applicationID); err != nil {
		return 0, fmt.Errorf("agent workforce: create the hired agent: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions
    (application_id, name, status, author_id, agent_type, instructions, welcome_message, llm_settings,
     conversation_starters, meta, pipeline_settings, soul, created_at)
VALUES ($1, $2, 'draft', $3, $4, $5, '', '{}'::jsonb, '[]'::jsonb, $6::jsonb, '{}'::jsonb, $7::jsonb, now() AT TIME ZONE 'UTC')`, schema),
		applicationID, defaultVersionName, candidate.forUser, defaultAgentType, stringOf(candidate.draft["instructions"]),
		string(meta), soul); err != nil {
		return 0, fmt.Errorf("agent workforce: create the hired agent's version: %w", err)
	}
	if err := EnsureRegistryTx(ctx, tx, projectID, applicationID, candidate.forUser); err != nil {
		return 0, err
	}
	return applicationID, nil
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

/* ── auto dismiss ──────────────────────────────────────────────────────── */

type policyAgent struct {
	WorkforceAgent
	card       *agentworkforce.Scorecard
	createdAt  time.Time
	openReview bool
}

// rulePass applies the rules to every active agent's latest scorecard.
func (s *AgentWorkforce) rulePass(ctx context.Context, policy WorkforcePolicy, summary *PolicySummary) error {
	if len(policy.Rules) == 0 {
		return nil
	}
	ids, err := s.activeAgents(ctx, policy.ProjectID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, applicationID := range ids {
		agent, err := s.policyAgentOf(ctx, s.pool, policy.ProjectID, applicationID, now)
		if err != nil {
			return err
		}
		if agent.card == nil || agent.openReview || agentworkforce.ExemptFromAutomatic(agent.Tier, agent.LegalHold) {
			continue
		}
		unused := 0
		if agent.card.UnusedDays != nil {
			unused = *agent.card.UnusedDays
		} else {
			unused = int(now.Sub(agent.createdAt).Hours() / 24)
		}
		onProbation := agent.OnProbation()
		facts := agentworkforce.FactsOf(*agent.card, unused, agent.AutoHiredAt != nil, onProbation && agentworkforce.ProbationFailed(*agent.card))
		// The strongest matching rule the pass may apply: one an objection
		// suppressed gives way to the next, weaker one.
		var (
			rule  agentworkforce.Rule
			found bool
		)
		for _, candidate := range agentworkforce.Matching(policy.Rules, facts) {
			if alreadyAt(agent.WorkforceAgent, candidate.Stage) {
				continue
			}
			suppressed, err := s.ruleSuppressed(ctx, policy.ProjectID, applicationID, candidate.ID, now)
			if err != nil {
				return err
			}
			if !suppressed {
				rule, found = candidate, true
				break
			}
		}
		if !found {
			continue
		}
		evidence := map[string]any{"scorecard": agent.card, "facts": facts, "rule": rule}
		if err := s.applyRule(ctx, policy, applicationID, rule.ID, rule.Stage, evidence, summary); err != nil {
			return err
		}
	}
	return nil
}

// alreadyAt reports an agent the stage would not change.
func alreadyAt(agent WorkforceAgent, stage agentworkforce.Stage) bool {
	switch stage {
	case agentworkforce.StageWatch:
		return agent.UnderReview || agent.SuspensionID > 0
	case agentworkforce.StageSuspend:
		return agent.SuspensionID > 0
	}
	return agent.State != agentworkforce.StateActive
}

func (s *AgentWorkforce) activeAgents(ctx context.Context, projectID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
SELECT application_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND lifecycle_state = 'active' ORDER BY application_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: list the active agents: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// policyAgentOf reads the agent with its latest scorecard (nil when older
// than two days: the policy never acts on a stale card) and whether a
// proposal about it is open.
func (s *AgentWorkforce) policyAgentOf(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64, now time.Time) (policyAgent, error) {
	agent, err := readWorkforceAgent(ctx, q, projectID, applicationID, false)
	if err != nil {
		return policyAgent{}, err
	}
	out := policyAgent{WorkforceAgent: agent}
	if err := q.QueryRow(ctx, `
SELECT created_at,
       EXISTS (SELECT 1 FROM elitea_lifecycle.agent_workforce_proposals
               WHERE project_id = $1 AND application_id = $2 AND status IN ('proposed', 'asked_owner'))
FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = $1 AND application_id = $2`,
		projectID, applicationID).Scan(&out.createdAt, &out.openReview); err != nil {
		return policyAgent{}, fmt.Errorf("agent workforce: read the agent: %w", err)
	}
	card, err := latestScorecard(ctx, q, projectID, applicationID, now)
	if err != nil {
		return policyAgent{}, err
	}
	if card != nil {
		if day, err := time.Parse("2006-01-02", card.Day[:min(len(card.Day), 10)]); err == nil && now.Sub(day) <= 48*time.Hour {
			out.card = card
		}
	}
	return out, nil
}

// ruleSuppressed: a person objected to, or undid, what this rule did to this
// agent in the last 30 days.
func (s *AgentWorkforce) ruleSuppressed(ctx context.Context, projectID, applicationID int64, rule string, now time.Time) (bool, error) {
	var suppressed bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_automatic_actions
               WHERE project_id = $1 AND application_id = $2 AND rule = $3
                 AND status IN ('objected', 'undone') AND decided_at > $4)`,
		projectID, applicationID, rule, now.Add(-agentworkforce.SuppressAfterObjection)).Scan(&suppressed)
	if err != nil {
		return false, fmt.Errorf("agent workforce: read the objections: %w", err)
	}
	return suppressed, nil
}

// applyRule takes one stage under one rule: watch and suspend at once,
// retire as a notice that applies after grace_days.
func (s *AgentWorkforce) applyRule(ctx context.Context, policy WorkforcePolicy, applicationID int64, rule string, stage agentworkforce.Stage, evidence map[string]any, summary *PolicySummary) error {
	if !agentworkforce.AutomaticStage(stage) {
		return agentworkforce.Refuse(agentworkforce.GuardCode, "The policy never demotes or deletes.")
	}
	var (
		action   AutomaticAction
		affected []int64
		notify   string
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicyProject(ctx, tx, policy.ProjectID); err != nil {
			return err
		}
		agent, err := readWorkforceAgent(ctx, tx, policy.ProjectID, applicationID, true)
		if err != nil {
			return err
		}
		if agent.State != agentworkforce.StateActive || alreadyAt(agent, stage) ||
			agentworkforce.ExemptFromAutomatic(agent.Tier, agent.LegalHold) {
			return nil
		}
		proposal, err := s.insertProposal(ctx, tx, agent, agentworkforce.ProposalInput{
			Stage: stage, Rationale: fmt.Sprintf("The workforce policy's rule %s matched the agent's scorecard.", rule),
		}, insertSource{policyRule: rule, principalHint: policyRef(rule)})
		if refusal, ok := agentworkforce.AsRefusal(err); ok && refusal.Code == agentworkforce.OpenProposalCode {
			return nil // a person's proposal is open: leave it to them
		}
		if err != nil {
			return err
		}
		evidence["proposal_id"] = proposal.ID
		kind := agentworkforce.ActionAutoDismiss
		if rule == agentworkforce.RuleOrphan {
			kind = agentworkforce.ActionOrphan
		}
		if stage == agentworkforce.StageRetire {
			due := s.now().UTC().Add(time.Duration(policy.GraceDays) * 24 * time.Hour)
			if action, err = s.insertAction(ctx, tx, policy.ProjectID, applicationID, kind, rule, string(stage), proposal.ID,
				agentworkforce.ActionPending, &due, evidence, false); err != nil {
				return err
			}
			summary.Noticed++
			notify = fmt.Sprintf("The workforce policy will retire agent %q on %s (rule %s). The owner or an agent manager can object until then.",
				agent.ApplicationName, due.Format("2006-01-02"), rule)
			return nil
		}
		if action, err = s.insertAction(ctx, tx, policy.ProjectID, applicationID, kind, rule, string(stage), proposal.ID,
			agentworkforce.ActionApplied, nil, evidence, true); err != nil {
			return err
		}
		affected, err = s.applyPolicyStage(ctx, tx, agent, proposal, rule)
		if err != nil {
			return err
		}
		summary.Applied++
		notify = fmt.Sprintf("The workforce policy moved agent %q to %s (rule %s).", agent.ApplicationName, stage, rule)
		return nil
	})
	if err != nil || notify == "" {
		return err
	}
	s.notify(affected...)
	s.notifyPolicy(ctx, action, 0, notify)
	return nil
}

// applyPolicyStage applies a policy proposal's stage through the ladder and
// records the approval: decided by nobody (the platform under the policy a
// person set), audited as policy:<rule>.
func (s *AgentWorkforce) applyPolicyStage(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, proposal WorkforceProposal, rule string) ([]int64, error) {
	stage := agentworkforce.Stage(proposal.Stage)
	if !agentworkforce.AutomaticStage(stage) {
		return nil, agentworkforce.Refuse(agentworkforce.GuardCode, "The policy never demotes or deletes.")
	}
	applied, suspensions, err := s.applyStage(ctx, tx, agent, stage, proposal, Decision{ProjectID: agent.ProjectID, ProposalID: proposal.ID})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(applied)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: encode the applied record: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals
SET status = 'approved', decided_at = clock_timestamp(), decision_note = $3, applied = $4::jsonb, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, agent.ProjectID, proposal.ID, "Applied by the workforce policy, rule "+rule+".", string(encoded)); err != nil {
		return nil, fmt.Errorf("agent workforce: record the policy decision: %w", err)
	}
	if err := s.append(ctx, tx, agent.ProjectID, "agent.workforce.policy_applied", policyRef(rule), proposalRef(proposal.ID), "",
		"stage:"+proposal.Stage, "evidence:"+proposal.EvidenceDigest, "agent:"+strconv.FormatInt(agent.ApplicationID, 10)); err != nil {
		return nil, err
	}
	return suspensions, nil
}

func (s *AgentWorkforce) insertAction(ctx context.Context, tx pgx.Tx, projectID, applicationID int64, kind, rule, stage string, proposalID int64, status string, due *time.Time, evidence map[string]any, applied bool) (AutomaticAction, error) {
	digest := agentworkforce.EvidenceDigest(evidence)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return AutomaticAction{}, fmt.Errorf("agent workforce: encode the action evidence: %w", err)
	}
	var stageValue *string
	if stage != "" {
		stageValue = &stage
	}
	action, err := scanAction(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.agent_automatic_actions AS a
    (project_id, application_id, kind, rule, stage, proposal_id, status, due_at, evidence, evidence_digest, applied_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, CASE WHEN $11 THEN clock_timestamp() END)
RETURNING `+actionColumns, projectID, applicationID, kind, rule, stageValue, nullablePositive(proposalID), status, due,
		string(encoded), digest, applied), s.now().UTC())
	if err != nil {
		return AutomaticAction{}, fmt.Errorf("agent workforce: record the automatic action: %w", err)
	}
	return action, nil
}

// reconcileActions moves the open actions on: a retire notice past its
// grace period is applied (or held when the agent became exempt or others
// nest it); a notice a person decided in the inbox follows that decision;
// an orphan whose owner is back is resolved, one flagged for 30 days moves
// to watch. A held action waits for a person: it only follows the inbox's
// decision (or, for an orphan, an owner coming back).
func (s *AgentWorkforce) reconcileActions(ctx context.Context, policy WorkforcePolicy, summary *PolicySummary) error {
	rows, err := s.pool.Query(ctx, `
SELECT id FROM elitea_lifecycle.agent_automatic_actions
WHERE project_id = $1 AND status IN ('pending', 'flagged', 'held') ORDER BY id`, policy.ProjectID)
	if err != nil {
		return fmt.Errorf("agent workforce: list the open actions: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.reconcileAction(ctx, policy, id, summary); err != nil {
			return err
		}
	}
	return nil
}

func (s *AgentWorkforce) reconcileAction(ctx context.Context, policy WorkforcePolicy, actionID int64, summary *PolicySummary) error {
	var (
		affected []int64
		notify   string
		action   AutomaticAction
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicyProject(ctx, tx, policy.ProjectID); err != nil {
			return err
		}
		var err error
		action, err = s.readAction(ctx, tx, policy.ProjectID, actionID, true)
		if err != nil {
			return err
		}
		if action.Status != agentworkforce.ActionPending && action.Status != agentworkforce.ActionFlagged &&
			action.Status != agentworkforce.ActionHeld {
			return nil
		}
		agent, err := readWorkforceAgent(ctx, tx, policy.ProjectID, action.ApplicationID, true)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if action.Kind == agentworkforce.ActionOrphan {
			return s.reconcileOrphan(ctx, tx, policy, action, agent, now, summary, &notify)
		}
		// A retire notice.
		var proposal WorkforceProposal
		if action.ProposalID != nil {
			if proposal, err = readWorkforceProposal(ctx, tx, policy.ProjectID, *action.ProposalID, true); err != nil {
				return err
			}
		}
		switch proposal.Status {
		case agentworkforce.StatusRejected:
			summary.Reconciled++
			by := int64(0)
			if proposal.DecidedByUserID != nil {
				by = *proposal.DecidedByUserID
			}
			if by > 0 {
				return s.closeAction(ctx, tx, action, agentworkforce.ActionObjected, by, "Rejected in the workforce inbox.")
			}
			return s.closeAction(ctx, tx, action, agentworkforce.ActionCancelled, 0, "The notice was closed.")
		case agentworkforce.StatusApproved:
			summary.Reconciled++
			_, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions SET status = 'applied', applied_at = clock_timestamp(),
    note = 'Approved in the workforce inbox.', updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID)
			return err
		}
		if action.Status == agentworkforce.ActionHeld {
			return nil // left to a person: the inbox decides
		}
		if action.DueAt == nil || now.Before(*action.DueAt) {
			return nil
		}
		if agent.State != agentworkforce.StateActive {
			summary.Reconciled++
			return s.closeAction(ctx, tx, action, agentworkforce.ActionCancelled, 0, "The agent is no longer active.")
		}
		if agentworkforce.ExemptFromAutomatic(agent.Tier, agent.LegalHold) {
			summary.Held++
			return s.holdAction(ctx, tx, action, "The agent became exempt (critical tier or legal hold); the notice waits for a person.")
		}
		impact, err := impactOf(ctx, tx, agent.ProjectID, agent.ApplicationID)
		if err != nil {
			return err
		}
		if len(impact.NestedBy) > 0 {
			summary.Held++
			notify = fmt.Sprintf("The workforce policy did not retire agent %q: other agents use it as a sub-agent. Decide the notice in the workforce inbox.",
				agent.ApplicationName)
			return s.holdAction(ctx, tx, action, "Other agents nest this agent; retiring it is left to a person.")
		}
		if affected, err = s.applyPolicyStage(ctx, tx, agent, proposal, action.Rule); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions SET status = 'applied', applied_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID); err != nil {
			return fmt.Errorf("agent workforce: record the retirement: %w", err)
		}
		summary.Retired++
		notify = fmt.Sprintf("The workforce policy retired agent %q after the notice (rule %s). An agent manager can reinstate it for 30 days.",
			agent.ApplicationName, action.Rule)
		return nil
	})
	if err != nil || notify == "" {
		return err
	}
	s.notify(affected...)
	s.notifyPolicy(ctx, action, 0, notify)
	return nil
}

// holdAction leaves an action to a person: the notice stays open in the
// workforce inbox.
func (s *AgentWorkforce) holdAction(ctx context.Context, tx pgx.Tx, action AutomaticAction, note string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions SET status = 'held', note = $3, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID, note); err != nil {
		return fmt.Errorf("agent workforce: hold the action: %w", err)
	}
	return s.append(ctx, tx, action.ProjectID, "agent.workforce.policy_held", policyRef(action.Rule), actionRef(action.ID), "",
		"agent:"+strconv.FormatInt(action.ApplicationID, 10))
}

/* ── orphans ───────────────────────────────────────────────────────────── */

// orphanPass flags every active agent whose owners are no longer members of
// the project (or that has none), once.
func (s *AgentWorkforce) orphanPass(ctx context.Context, policy WorkforcePolicy, summary *PolicySummary) error {
	present, err := relationExists(ctx, s.pool, "public.auth_core__project_user_role")
	if err != nil || !present {
		return err
	}
	// An agent whose owner is back and that has no open orphan action loses
	// its flag, so it can be flagged again if its owners leave later.
	if _, err := s.pool.Exec(ctx, unflagOrphansSQL, policy.ProjectID); err != nil {
		return fmt.Errorf("agent workforce: clear the orphan flags: %w", err)
	}
	rows, err := s.pool.Query(ctx, orphansSQL, policy.ProjectID)
	if err != nil {
		return fmt.Errorf("agent workforce: find the orphaned agents: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, applicationID := range ids {
		var action AutomaticAction
		flagged := false
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			agent, err := readWorkforceAgent(ctx, tx, policy.ProjectID, applicationID, true)
			if err != nil {
				return err
			}
			if agent.State != agentworkforce.StateActive || agentworkforce.ExemptFromAutomatic(agent.Tier, agent.LegalHold) {
				return nil
			}
			now := s.now().UTC()
			due := now.Add(agentworkforce.OrphanWatchAfter)
			if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry SET orphaned_since = COALESCE(orphaned_since, $3), updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, policy.ProjectID, applicationID, now); err != nil {
				return fmt.Errorf("agent workforce: flag the orphan: %w", err)
			}
			evidence := map[string]any{
				"business_owner_id": agent.BusinessOwnerID, "technical_owner_id": agent.TechnicalOwnerID, "watch_on": due,
			}
			if action, err = s.insertAction(ctx, tx, policy.ProjectID, applicationID, agentworkforce.ActionOrphan, agentworkforce.RuleOrphan,
				string(agentworkforce.StageWatch), 0, agentworkforce.ActionFlagged, &due, evidence, false); err != nil {
				return err
			}
			flagged = true
			return s.append(ctx, tx, policy.ProjectID, "agent.workforce.orphan_flagged", policyRef(agentworkforce.RuleOrphan),
				actionRef(action.ID), "", "agent:"+strconv.FormatInt(applicationID, 10), "evidence:"+action.EvidenceDigest)
		})
		if err != nil {
			return err
		}
		if flagged {
			summary.Orphans++
			s.notifyPolicy(ctx, action, 0, fmt.Sprintf(
				"Agent %q has no owner in this project any more. Reassign it in the agent's registry; otherwise it moves to watch in 30 days.",
				action.ApplicationName))
		}
	}
	return nil
}

// orphansSQL: the project's active agents, not yet flagged, whose business
// and technical owners are both absent or no longer members.
const orphansSQL = `
SELECT r.application_id FROM elitea_lifecycle.agent_registry AS r
WHERE r.tenant_project_id = $1 AND r.lifecycle_state = 'active' AND r.orphaned_since IS NULL
  AND NOT EXISTS (SELECT 1 FROM public.auth_core__project_user_role AS m
                  WHERE m.project_id = r.tenant_project_id
                    AND m.user_id IN (r.business_owner_id, r.technical_owner_id))
  AND NOT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_automatic_actions AS a
                  WHERE a.project_id = r.tenant_project_id AND a.application_id = r.application_id
                    AND a.kind = 'orphan' AND a.status IN ('flagged', 'pending', 'held'))
ORDER BY r.application_id`

// unflagOrphansSQL clears orphaned_since of the agents that have an owner in
// the project again and no open orphan action.
const unflagOrphansSQL = `
UPDATE elitea_lifecycle.agent_registry AS r
SET orphaned_since = NULL, updated_at = clock_timestamp()
WHERE r.tenant_project_id = $1 AND r.orphaned_since IS NOT NULL
  AND EXISTS (SELECT 1 FROM public.auth_core__project_user_role AS m
              WHERE m.project_id = r.tenant_project_id
                AND m.user_id IN (r.business_owner_id, r.technical_owner_id))
  AND NOT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_automatic_actions AS a
                  WHERE a.project_id = r.tenant_project_id AND a.application_id = r.application_id
                    AND a.kind = 'orphan' AND a.status IN ('flagged', 'pending', 'held'))`

func (s *AgentWorkforce) ownerIsMember(ctx context.Context, tx pgx.Tx, agent WorkforceAgent) (bool, error) {
	var member bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role
               WHERE project_id = $1 AND user_id IN ($2, $3))`, agent.ProjectID, agent.BusinessOwnerID, agent.TechnicalOwnerID).Scan(&member)
	if err != nil {
		return false, fmt.Errorf("agent workforce: read the owners' membership: %w", err)
	}
	return member, nil
}

func (s *AgentWorkforce) reconcileOrphan(ctx context.Context, tx pgx.Tx, policy WorkforcePolicy, action AutomaticAction, agent WorkforceAgent, now time.Time, summary *PolicySummary, notify *string) error {
	member, err := s.ownerIsMember(ctx, tx, agent)
	if err != nil {
		return err
	}
	if member || agent.State != agentworkforce.StateActive {
		summary.Reconciled++
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry SET orphaned_since = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID); err != nil {
			return err
		}
		return s.closeAction(ctx, tx, action, agentworkforce.ActionResolved, 0, "The agent has an owner again.")
	}
	if action.Status == agentworkforce.ActionHeld || action.DueAt == nil || now.Before(*action.DueAt) {
		return nil
	}
	if agentworkforce.ExemptFromAutomatic(agent.Tier, agent.LegalHold) {
		summary.Held++
		return s.holdAction(ctx, tx, action, "The agent is exempt (critical tier or legal hold); reassigning it is left to a person.")
	}
	if alreadyAt(agent, agentworkforce.StageWatch) {
		summary.Reconciled++
		_, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions SET status = 'applied', applied_at = clock_timestamp(),
    note = 'The agent was already under watch.', updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID)
		return err
	}
	proposal, err := s.insertProposal(ctx, tx, agent, agentworkforce.ProposalInput{
		Stage: agentworkforce.StageWatch, Rationale: "The agent has had no owner in this project for 30 days.",
	}, insertSource{policyRule: agentworkforce.RuleOrphan, principalHint: policyRef(agentworkforce.RuleOrphan)})
	if refusal, ok := agentworkforce.AsRefusal(err); ok && refusal.Code == agentworkforce.OpenProposalCode {
		summary.Held++
		return s.holdAction(ctx, tx, action, "A proposal about the agent is open; it is left to a person.")
	}
	if err != nil {
		return err
	}
	if _, err := s.applyPolicyStage(ctx, tx, agent, proposal, agentworkforce.RuleOrphan); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_automatic_actions SET status = 'applied', applied_at = clock_timestamp(), proposal_id = $3,
    updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`, action.ProjectID, action.ID, proposal.ID); err != nil {
		return err
	}
	summary.Applied++
	*notify = fmt.Sprintf("Agent %q has had no owner for 30 days and is now under watch. Reassign it in the agent's registry.", agent.ApplicationName)
	return nil
}

/* ── notices ───────────────────────────────────────────────────────────── */

// notifyPolicy tells the agent managers (the project admins), the agent's
// owners and, for a hire, the person it was hired for. Best effort.
//
// The key and the event carry the status the action ended in, read after the
// commit: a caller may hold the action as it was before its UPDATE (still
// pending or flagged), which would reuse the notice's key and the insert's ON
// CONFLICT would drop the later message.
func (s *AgentWorkforce) notifyPolicy(ctx context.Context, action AutomaticAction, extra int64, message string) {
	if current, err := s.readAction(ctx, s.pool, action.ProjectID, action.ID, false); err == nil {
		action.Status = current.Status
	}
	recipients := map[int64]bool{}
	if admins, err := projectAdminIDs(ctx, s.pool, action.ProjectID); err == nil {
		for _, id := range admins {
			recipients[id] = true
		}
	}
	if agent, err := readWorkforceAgent(ctx, s.pool, action.ProjectID, action.ApplicationID, false); err == nil {
		recipients[agent.BusinessOwnerID] = true
		recipients[agent.TechnicalOwnerID] = true
	}
	recipients[extra] = true
	for id := range recipients {
		if id <= 0 {
			continue
		}
		meta := map[string]any{
			"message": message, "project_id": action.ProjectID, "application_id": action.ApplicationID,
			"automatic_action_id": action.ID, "kind": action.Kind, "rule": action.Rule, "event": "policy_" + action.Status,
		}
		key := fmt.Sprintf("workforce-policy-%d-%s-%d", action.ID, action.Status, id)
		_ = insertReviewNotification(ctx, s.pool, key, action.ProjectID, id, WorkforceNotificationEventType, meta)
	}
}
