import { describe, expect, it } from 'vitest';

import { AgentSoulV1 } from '@/shared/api/generated/model/agentSoulV1.zod';

import {
  EMPTY_SOUL_DRAFT,
  SOUL_MAX_TEXT_BYTES,
  areSoulDraftsEqual,
  soulFromDraft,
  soulTextBytes,
  soulToDraft,
} from './agentSoulDraft';

describe('agentSoulDraft', () => {
  const soul = {
    schema: 'elitea.soul/v1' as const,
    display_name: 'Ada',
    pronouns: 'she/her',
    role: 'Release reviewer',
    voice: ['dry', 'brief'],
    values: ['correctness first'],
    boundaries: ['never approves its own change'],
    relationships: [{ agent: 'planner', stance: 'asks before: changing scope' }],
    proactive_goals: ['check the release notes'],
    body: '# Notes\n\nKeep it short.',
  };

  it('round-trips every field through the draft', () => {
    const back = soulFromDraft(soulToDraft(soul));

    expect(back).toEqual(soul);
    expect(AgentSoulV1.parse(back)).toEqual(soul);
  });

  it('maps an empty draft to null and null to the empty draft', () => {
    expect(soulFromDraft(EMPTY_SOUL_DRAFT)).toBeNull();
    expect(areSoulDraftsEqual(soulToDraft(null), EMPTY_SOUL_DRAFT)).toBe(true);
  });

  it('drops blank lines and relationships with no stance, which Main would refuse', () => {
    const draft = { ...EMPTY_SOUL_DRAFT, voice: 'dry\n\n  \nbrief', relationships: 'planner\nreviewer: \nqa: trusts' };

    expect(soulFromDraft(draft)).toEqual({
      schema: 'elitea.soul/v1',
      voice: ['dry', 'brief'],
      relationships: [{ agent: 'qa', stance: 'trusts' }],
    });
  });

  it('counts UTF-8 bytes of every string value, schema included', () => {
    expect(soulTextBytes(null)).toBe(0);
    expect(soulTextBytes({ schema: 'elitea.soul/v1', body: 'é' })).toBe('elitea.soul/v1'.length + 2);
    expect(soulTextBytes({ schema: 'elitea.soul/v1', body: 'x'.repeat(SOUL_MAX_TEXT_BYTES) })).toBeGreaterThan(
      SOUL_MAX_TEXT_BYTES,
    );
  });
});
