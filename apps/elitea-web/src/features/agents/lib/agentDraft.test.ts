import { describe, expect, it } from 'vitest';

import { EMPTY_AGENT_DRAFT, filterEmptyStrings, mapApplicationDraft } from './agentDraft';

describe('filterEmptyStrings', () => {
  it('drops blank and whitespace-only entries', () => {
    expect(filterEmptyStrings(['hello', '', '  ', 'world'])).toEqual(['hello', 'world']);
  });

  it('returns an empty array when every entry is blank', () => {
    expect(filterEmptyStrings(['', '   '])).toEqual([]);
  });

  it('returns a new array, not the input reference', () => {
    const input = ['a'];
    expect(filterEmptyStrings(input)).not.toBe(input);
  });
});

describe('mapApplicationDraft', () => {
  const served = {
    name: 'Incident Triager',
    description: 'Triages incoming incidents',
    instructions: 'Sort by severity, then page the owner.',
    welcome_message: 'What broke?',
    conversation_starters: ['Triage this page', 'Summarise the last hour'],
  };

  it('carries every served field into the draft the review form edits', () => {
    expect(mapApplicationDraft(served)).toEqual({
      ...EMPTY_AGENT_DRAFT,
      name: 'Incident Triager',
      description: 'Triages incoming incidents',
      instructions: 'Sort by severity, then page the owner.',
      welcome_message: 'What broke?',
      conversation_starters: ['Triage this page', 'Summarise the last hour'],
    });
  });

  it('drops a blank conversation starter rather than rendering an empty chip', () => {
    const draft = mapApplicationDraft({ ...served, conversation_starters: ['Keep me', '   ', ''] });
    expect(draft.conversation_starters).toEqual(['Keep me']);
  });

  it('never fabricates suggested resources — the endpoint carries none', () => {
    const draft = mapApplicationDraft(served);
    expect(draft.suggested_toolkits).toEqual([]);
    expect(draft.suggested_mcp).toEqual([]);
    expect(draft.suggested_pipelines).toEqual([]);
    expect(draft.suggested_agents).toEqual([]);
    expect(draft.suggested_skills).toEqual([]);
  });
});
