import { describe, expect, it } from 'vitest';

import { sortSkills } from './Skills';

const SKILLS = [
  { name: 'Beta', created_at: '2026-02-01T00:00:00Z' },
  { name: 'Alpha', created_at: '2026-03-01T00:00:00Z' },
  { name: 'Gamma', created_at: '2026-01-01T00:00:00Z' },
];

describe('sortSkills', () => {
  it('keeps the list endpoint order (newest first) when nothing was chosen', () => {
    expect(sortSkills(SKILLS, undefined, false).map((skill) => skill.name)).toEqual(['Beta', 'Alpha', 'Gamma']);
  });

  it('sorts by created date, either way', () => {
    expect(sortSkills(SKILLS, 'createdAt', true).map((skill) => skill.name)).toEqual(['Gamma', 'Beta', 'Alpha']);
    expect(sortSkills(SKILLS, 'createdAt', false).map((skill) => skill.name)).toEqual(['Alpha', 'Beta', 'Gamma']);
  });

  it('sorts by name, either way', () => {
    expect(sortSkills(SKILLS, 'name', true).map((skill) => skill.name)).toEqual(['Alpha', 'Beta', 'Gamma']);
    expect(sortSkills(SKILLS, 'name', false).map((skill) => skill.name)).toEqual(['Gamma', 'Beta', 'Alpha']);
  });

  it('does not change the list it is given', () => {
    const before = SKILLS.map((skill) => skill.name);
    sortSkills(SKILLS, 'name', true);
    expect(SKILLS.map((skill) => skill.name)).toEqual(before);
  });
});
