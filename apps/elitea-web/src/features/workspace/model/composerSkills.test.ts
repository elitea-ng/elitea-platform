import { describe, expect, it } from 'vitest';

import { invokedSkill, matchingSkills, skillToken, type ComposerSkill } from './composerSkills';

const SKILLS: ComposerSkill[] = [
  { id: '1', name: 'review', description: 'Review a change' },
  { id: '2', name: 'code-review' },
  { id: '3', name: 'Release notes' },
  { id: '4', name: 'style' },
];

describe('matchingSkills', () => {
  it('lists every skill for an empty query, alphabetically', () => {
    expect(matchingSkills(SKILLS, '').map((s) => s.name)).toEqual(['code-review', 'Release notes', 'review', 'style']);
  });

  it('puts names starting with the query first, then names containing it, ignoring case', () => {
    expect(matchingSkills(SKILLS, 'RE').map((s) => s.name)).toEqual(['Release notes', 'review', 'code-review']);
    expect(matchingSkills(SKILLS, 'view').map((s) => s.name)).toEqual(['code-review', 'review']);
    expect(matchingSkills(SKILLS, 'nothing')).toEqual([]);
  });

  it('lists at most 50', () => {
    const many = Array.from({ length: 60 }, (_, i) => ({ id: String(i), name: `s${String(i).padStart(2, '0')}` }));
    expect(matchingSkills(many, 's')).toHaveLength(50);
  });
});

describe('invokedSkill', () => {
  it('is the skill the first word names, followed by a blank or the end', () => {
    expect(invokedSkill('/style fix it', SKILLS)?.id).toBe('4');
    expect(invokedSkill('  /STYLE', SKILLS)?.id).toBe('4');
    expect(invokedSkill('/style\nfix it', SKILLS)?.id).toBe('4');
  });

  it('takes the longest name, so a name with a space is whole', () => {
    expect(invokedSkill('/Release notes for 1.2', [...SKILLS, { id: '5', name: 'Release' }])?.id).toBe('3');
  });

  it('is none for a longer word, a later "/" or no skill at all', () => {
    expect(invokedSkill('/styles fix it', SKILLS)).toBeNull();
    expect(invokedSkill('use /style', SKILLS)).toBeNull();
    expect(invokedSkill('/style', [])).toBeNull();
  });

  it('inserts the name after a "/"', () => {
    expect(skillToken({ id: '3', name: 'Release notes' })).toBe('/Release notes');
  });
});
