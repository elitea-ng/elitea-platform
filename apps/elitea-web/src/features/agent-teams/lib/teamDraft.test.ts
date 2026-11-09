import { describe, expect, it } from 'vitest';

import type { AgentTeam, AgentTeamMember } from '@/shared/api/generated/model';

import { MAX_TEAM_MEMBERS, defaultVersionOf, draftOf, draftProblem, withMember, withoutMember, writeOf, type TeamDraft } from './teamDraft';

const member = (id: number, project = 1): AgentTeamMember => ({ application_id: id, project_id: project, version_id: id * 10, name: `Agent ${id}` });

const team: AgentTeam = {
  id: 3,
  project_id: 1,
  name: 'Release crew',
  description: '',
  floor_policy: 'lead',
  lead_position: 2,
  members: [member(1), member(2)],
  created_by: 7,
  created_at: '2026-11-09T10:00:00Z',
  updated_at: '2026-11-09T10:00:00Z',
};

describe('teamDraft', () => {
  it('round-trips a team with a lead to the lead policy', () => {
    const draft = draftOf(team);
    expect(draft.lead).toBe(2);
    expect(writeOf(draft)).toEqual({
      name: 'Release crew',
      description: '',
      floor_policy: 'lead',
      lead_position: 2,
      members: [member(1), member(2)],
    });
  });

  it('writes addressed with no lead', () => {
    const write = writeOf({ name: ' Solo ', description: '', members: [member(1)], lead: 0 });
    expect(write.floor_policy).toBe('addressed');
    expect(write.lead_position).toBeNull();
    expect(write.name).toBe('Solo');
  });

  it('moves the lead with its member and clears it when the lead is removed', () => {
    const draft = { name: 'x', description: '', members: [member(1), member(2), member(3)], lead: 3 };
    expect(withoutMember(draft, 0).lead).toBe(2);
    expect(withoutMember(draft, 2).lead).toBe(0);
    expect(withoutMember(draft, 1).lead).toBe(2);
  });

  it('adds an agent once, keeps the same agent of another project, and stops at the limit', () => {
    let draft: TeamDraft = { name: 'x', description: '', members: [member(1)], lead: 0 };
    expect(withMember(draft, member(1)).members).toHaveLength(1);
    expect(withMember(draft, member(1, 2)).members).toHaveLength(2);
    for (let id = 2; id <= MAX_TEAM_MEMBERS + 2; id += 1) draft = withMember(draft, member(id));
    expect(draft.members).toHaveLength(MAX_TEAM_MEMBERS);
  });

  it('names the first broken rule', () => {
    expect(draftProblem({ name: ' ', description: '', members: [member(1)], lead: 0 })).toBe('name');
    expect(draftProblem({ name: 'x', description: '', members: [], lead: 0 })).toBe('members');
    expect(draftProblem({ name: 'a'.repeat(129), description: '', members: [member(1)], lead: 0 })).toBe('nameTooLong');
    expect(draftProblem({ name: 'x', description: '', members: [member(1)], lead: 0 })).toBeUndefined();
  });

  it('picks the default version, else base, else the first', () => {
    const version = (id: string, name: string, isDefault = false) => ({ id, name, status: 'draft', agent_type: 'classic', created_at: '', is_default: isDefault });
    expect(defaultVersionOf([version('1', 'base'), version('2', 'v2', true)])).toBe(2);
    expect(defaultVersionOf([version('5', 'v5'), version('1', 'base')])).toBe(1);
    expect(defaultVersionOf([version('5', 'v5')])).toBe(5);
    expect(defaultVersionOf([])).toBeUndefined();
  });
});
