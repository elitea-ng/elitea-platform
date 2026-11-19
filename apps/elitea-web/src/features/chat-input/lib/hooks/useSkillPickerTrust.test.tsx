import { describe, expect, it } from 'vitest';

import type { ConversationSkillPin } from '@/entities/conversation-skill';

import type { ProjectSkillEntry } from './useProjectSkillList';
import { untrustedSkillIds } from './useSkillPickerTrust';

function pin(skillId: number, state: 'trusted' | 'untrusted'): ConversationSkillPin {
  return {
    id: skillId,
    kind: 'project',
    skill_id: skillId,
    name: `skill ${String(skillId)}`,
    description: '',
    trust: { state },
    available: true,
    pinned_by: 1,
    pinned_at: '2026-11-18T00:00:00Z',
  };
}

function entry(id: number, untrusted?: boolean): ProjectSkillEntry {
  return { id: String(id), name: `skill ${String(id)}`, ...(untrusted === undefined ? {} : { untrusted }) };
}

describe('untrustedSkillIds', () => {
  it("takes a pinned skill's state from its pin and the others from their listing rows", () => {
    const ids = untrustedSkillIds([pin(1, 'untrusted'), pin(3, 'trusted')], [entry(1), entry(2, true), entry(3, true), entry(4, false), entry(5)]);
    // 3 is pinned as trusted: the pin answers, not the row. 4 and 5 are not marked.
    expect([...ids].sort()).toEqual(['1', '2']);
  });

  it('marks nothing for rows that carry no trust state (a server before 1.63)', () => {
    expect(untrustedSkillIds([], [entry(7), entry(8)]).size).toBe(0);
  });

  it('ignores a pin of a Marketplace listing, which names no project skill', () => {
    const { skill_id: _named, ...listing } = pin(9, 'untrusted');
    expect(untrustedSkillIds([listing], []).size).toBe(0);
  });
});
