import type { AgentProposal } from '@/shared/api/generated/model';

export function proposalFixture(overrides: Partial<AgentProposal> = {}): AgentProposal {
  return {
    id: 5,
    project_id: 1,
    proposer_application_id: 41,
    proposer_name: 'Mira',
    proposed_for_user_id: 4,
    execution_id: 'exec-1',
    conversation_uuid: '6b8f2a1c-0d3e-4f5a-9b7c-1e2d3f4a5b6c',
    ephemeral_agent_id: 'a_00000001',
    spec_digest: 'e2b787c2',
    rationale: 'It found the bug fast.',
    draft: {
      name: 'Ada',
      description: 'It found the bug fast.',
      instructions: 'Review pull requests.',
      welcome_message: '',
      conversation_starters: [],
      risk_tier: 'low',
      soul: { schema: 'elitea.soul/v1', display_name: 'Ada', body: 'Precise and brief.' },
      tools: [{ key: 'github/read_file', toolkit_type: '', category: 'read' }],
    },
    memory_seeds: [
      { kind: 'fact', content: 'The repo uses Go.' },
      { kind: 'preference', content: 'Short answers.' },
    ],
    tool_categories: [{ key: 'github/read_file', category: 'read' }],
    status: 'proposed',
    decided_by_user_id: null,
    decided_at: null,
    created_application_id: null,
    created_at: '2026-11-09T10:00:00Z',
    evidence: null,
    ...overrides,
  };
}
