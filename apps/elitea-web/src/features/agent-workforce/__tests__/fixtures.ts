import type { AgentScorecard, AgentWorkforceAgent, AgentWorkforceProposal } from '@/shared/api/generated/model';

export function workforceAgent(overrides: Partial<AgentWorkforceAgent> = {}): AgentWorkforceAgent {
  return {
    project_id: 1,
    application_id: 41,
    application_name: 'Triage',
    risk_tier: 'low',
    business_owner_id: 5,
    technical_owner_id: 5,
    under_review: false,
    under_review_since: null,
    lifecycle_state: '',
    retired_at: null,
    retired_by: null,
    reinstate_until: null,
    purge_after: null,
    suspension_id: 0,
    suspension_source: '',
    legal_hold: false,
    is_hr_agent: false,
    scorecard: null,
    can_manage: true,
    is_owner: false,
    ...overrides,
  };
}

export function scorecard(overrides: Partial<AgentScorecard> = {}): AgentScorecard {
  return {
    day: '2026-11-09',
    runs_30d: 40,
    failed_30d: 10,
    cap_reached_30d: 1,
    failure_rate_30d: 0.25,
    breaker_trips_7d: 0,
    approval_rejections_30d: 2,
    guardrail_hits_30d: 0,
    spend_30d_usd: 12.5,
    project_median_spend_usd: 4,
    thumbs_up_30d: 3,
    thumbs_down_30d: 6,
    last_used_at: '2026-11-08T10:00:00Z',
    unused_days: 1,
    certification_state: 'certified',
    computed_at: '2026-11-09T03:00:00Z',
    ...overrides,
  };
}

export function workforceProposal(overrides: Partial<AgentWorkforceProposal> = {}): AgentWorkforceProposal {
  return {
    id: 7,
    project_id: 1,
    application_id: 41,
    application_name: 'Triage',
    kind: 'dismissal',
    stage: 'retire',
    source: 'hr_agent',
    proposed_by_user_id: null,
    proposer_application_id: 50,
    execution_id: 'exec-1',
    rationale: 'Failed a quarter of its runs.',
    narrowing: {},
    evidence: null,
    evidence_digest: 'd',
    status: 'proposed',
    owner_comment: null,
    decided_by_user_id: null,
    decided_at: null,
    decision_note: null,
    applied: null,
    appeal_state: null,
    appeal_reason: null,
    appealed_by_user_id: null,
    appealed_at: null,
    appeal_decided_by_user_id: null,
    appeal_decided_at: null,
    appeal_note: null,
    created_at: '2026-11-09T04:00:00Z',
    ...overrides,
  };
}
