import { describe, expect, it } from 'vitest';

import { AgentSoulV1 } from '@/shared/api/generated/model/agentSoulV1.zod';

import {
  EMPTY_SOUL_DRAFT,
  SOUL_MAX_TEXT_BYTES,
  areSoulDraftsEqual,
  readSoul,
  soulDraftProblems,
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

describe('readSoul', () => {
  it('accepts a well-formed soul and rejects any other shape', () => {
    const good = { schema: 'elitea.soul/v1', role: 'Lead', voice: ['calm'], relationships: [{ agent: 'a', stance: 'b' }] };
    expect(readSoul(good)).toBe(good);
    for (const bad of [null, 'x', [], { voice: 'calm' }, { role: 7 }, { relationships: [null] }, { relationships: [{ agent: 'a' }] }]) {
      expect(readSoul(bad)).toBeUndefined();
    }
  });
});

describe('soulDraftProblems', () => {
  it('passes a draft Main would admit and names each field it would refuse', () => {
    expect(soulDraftProblems(soulToDraft({ schema: 'elitea.soul/v1', voice: ['dry'] })).size).toBe(0);

    const problems = soulDraftProblems({
      ...EMPTY_SOUL_DRAFT,
      voice: Array.from({ length: 17 }, (_, index) => `v${index}`).join('\n'),
      values: 'x'.repeat(257),
      relationships: 'planner has no stance',
      body: 'y'.repeat(SOUL_MAX_TEXT_BYTES),
    });

    expect([...problems].sort()).toEqual(['relationships', 'size', 'values', 'voice']);
  });
});
