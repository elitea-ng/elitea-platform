import { describe, expect, it } from 'vitest';

import { proposalFixture } from '../__tests__/fixtures';
import { agentsMarkdownOf, createRequestOf, fileStemOf, reviewDraftOf, soulMarkdownOf } from './proposalDraft';
import { proposalIdOf, proposeActions } from './proposedAgents';

describe('proposalIdOf', () => {
  it('reads a new or repeated proposal, in any of the stored encodings', () => {
    expect(proposalIdOf({ status: 'proposed', proposal_id: '12', message: '' })).toBe(12);
    expect(proposalIdOf(JSON.stringify({ status: 'already_proposed', proposal_id: 12 }))).toBe(12);
    expect(proposalIdOf(JSON.stringify(JSON.stringify({ status: 'proposed', proposal_id: '7' })))).toBe(7);
  });

  it('has no proposal for a refusal or an unreadable id', () => {
    expect(proposalIdOf({ status: 'proposal_limit', message: 'three open' })).toBeUndefined();
    expect(proposalIdOf({ status: 'proposal_suppressed' })).toBeUndefined();
    expect(proposalIdOf({ status: 'proposed', proposal_id: '0' })).toBeUndefined();
    expect(proposalIdOf({ status: 'proposed', proposal_id: '1e3' })).toBeUndefined();
    expect(proposalIdOf('not json')).toBeUndefined();
  });
});

describe('proposeActions', () => {
  it('keeps only completed propose_agent calls', () => {
    const actions = [
      { name: 'propose_agent', status: 'complete' },
      { name: 'propose_agent', status: 'processing' },
      { name: 'remember', status: 'complete' },
    ];
    expect(proposeActions(actions)).toEqual([actions[0]]);
    expect(proposeActions(undefined)).toEqual([]);
  });
});

describe('the review draft', () => {
  it('reads the server-built draft and tolerates missing fields', () => {
    const review = reviewDraftOf(proposalFixture());
    expect(review).toMatchObject({ name: 'Ada', instructions: 'Review pull requests.', displayName: 'Ada', soul: 'Precise and brief.' });
    expect(reviewDraftOf({ draft: { name: 3, soul: 'x' } })).toMatchObject({ name: '', soul: '', conversationStarters: [] });
  });

  it('creates a fresh agent with the soul and without tools or a tier', () => {
    const request = createRequestOf(reviewDraftOf(proposalFixture()));
    expect(request.name).toBe('Ada');
    const version = request.versions?.[0];
    expect(version?.soul).toEqual({ schema: 'elitea.soul/v1', display_name: 'Ada', body: 'Precise and brief.' });
    expect(version?.meta).toEqual({ step_limit: 25, internal_tools: [] });
    expect(JSON.stringify(request)).not.toContain('risk_tier');
    expect(JSON.stringify(request)).not.toContain('github/read_file');
  });

  it('sends no soul when the sub-agent had none', () => {
    const review = { ...reviewDraftOf(proposalFixture()), soul: ' ', displayName: '' };
    expect(createRequestOf(review).versions?.[0]).not.toHaveProperty('soul');
  });

  it('writes AGENTS.md and SOUL.md', () => {
    const proposal = proposalFixture();
    const review = reviewDraftOf(proposal);
    const agents = agentsMarkdownOf(review, proposal);
    expect(agents).toContain('# Ada');
    expect(agents).toContain('## Instructions\n\nReview pull requests.');
    expect(agents).toContain('- `github/read_file` (read)');
    expect(agents).toContain('- fact: The repo uses Go.');
    expect(soulMarkdownOf(review)).toBe('# Ada\n\nPrecise and brief.\n');
    expect(fileStemOf('  Code Reviewer!  ')).toBe('code-reviewer');
    expect(fileStemOf('???')).toBe('agent');
  });
});
