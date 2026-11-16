import { describe, expect, it } from 'vitest';

import { detectMention, withProjectSkills } from './useChatSkillMention';

/**
 * ADR-0050 amendment 1, A1.5: `/skill` opens the skill list `~` opens, and a pick writes `~name`. `~` stays as an
 * alias, so a message never carries the text `/skill`.
 */
describe('detectMention', () => {
  it('keeps the ~ trigger exactly as before', () => {
    expect(detectMention('use ~Tri', 8)).toEqual({ anchor: 4, query: 'Tri' });
    expect(detectMention('~', 1)).toEqual({ anchor: 0, query: '' });
    expect(detectMention('a~b', 3)).toBeNull();
    expect(detectMention('~Triage done', 12)).toBeNull();
  });

  it('opens the same list for /skill, with or without a name typed', () => {
    expect(detectMention('/skill', 6)).toEqual({ anchor: 0, query: '' });
    expect(detectMention('/skill ', 7)).toEqual({ anchor: 0, query: '' });
    expect(detectMention('/skill Tri', 10)).toEqual({ anchor: 0, query: 'Tri' });
    expect(detectMention('line one\n/skill rel', 19)).toEqual({ anchor: 9, query: 'rel' });
    expect(detectMention('/SKILL rel', 10)).toEqual({ anchor: 0, query: 'rel' });
  });

  it('does not take /skills, a path, or a finished command for a trigger', () => {
    expect(detectMention('/skills', 7)).toBeNull();
    expect(detectMention('/skillset/tool', 14)).toBeNull();
    expect(detectMention('a/skill', 7)).toBeNull();
    // Prose that mentions the command mid-sentence does not open the picker.
    expect(detectMention('please /skill rel', 17)).toBeNull();
    expect(detectMention('/skill Triage and more', 22)).toBeNull();
    // Only the text up to the cursor counts: the cursor before the command is not in it.
    expect(detectMention('hello /skill', 5)).toBeNull();
  });
});

describe('withProjectSkills', () => {
  const project = [
    { id: '3', name: 'Changelog' },
    { id: '1', name: 'Zeta' },
    { id: '2', name: 'Alpha' },
  ];

  it('lists the conversation-pinned skills first, then the rest alphabetically', () => {
    const items = withProjectSkills([], project, new Set(['1']));
    expect(items.map((item) => item.name)).toEqual(['Zeta', 'Alpha', 'Changelog']);
  });

  it('is plain alphabetical when nothing is pinned', () => {
    expect(withProjectSkills([], project).map((item) => item.name)).toEqual(['Alpha', 'Changelog', 'Zeta']);
  });
});
