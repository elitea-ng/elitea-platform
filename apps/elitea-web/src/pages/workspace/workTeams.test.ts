import { describe, expect, it } from 'vitest';

import type { AgentTeam, Application } from '@/shared/api/generated/model';

import { teamChoice } from './workTeams';

const agents = [
  { id: '5', name: 'Coder' },
  { id: '6', name: 'Reviewer' },
  { id: '7', name: 'Release' },
] as unknown as Application[];

function team(extra: Partial<AgentTeam>): AgentTeam {
  return {
    id: 3,
    project_id: 42,
    name: 'Review crew',
    description: '',
    floor_policy: 'lead',
    lead_position: 2,
    members: [
      { application_id: 5, project_id: 42, version_id: 9, name: 'Coder' },
      { application_id: 6, project_id: 42, version_id: 10, name: 'Reviewer' },
    ],
    created_by: 3,
    created_at: '2026-11-20T08:00:00Z',
    updated_at: '2026-11-20T08:00:00Z',
    ...extra,
  };
}

describe('teamChoice (#3053)', () => {
  it('runs a lead team with the lead as the composer agent and the others as its companions, on the pinned versions', () => {
    expect(teamChoice(team({}), 42, agents, new Set())).toEqual({
      id: 3,
      name: 'Review crew',
      team: {
        id: 3,
        name: 'Review crew',
        lead: true,
        primary: { applicationId: 6, versionId: 10, name: 'Reviewer' },
        others: [{ applicationId: 5, versionId: 9, name: 'Coder' }],
      },
    });
  });

  it('runs an addressed team with its first member as the composer agent and no lead', () => {
    const choice = teamChoice(team({ floor_policy: 'addressed', lead_position: null }), 42, agents, new Set());
    expect(choice).toMatchObject({ team: { lead: false, primary: { applicationId: 5 }, others: [{ applicationId: 6 }] } });
  });

  it('keeps the order of the other members and names a member saved without a name by its agent', () => {
    const members = [
      { application_id: 7, project_id: 42, version_id: 11, name: '' },
      { application_id: 5, project_id: 42, version_id: 9, name: 'Coder' },
      { application_id: 6, project_id: 42, version_id: 10, name: 'Reviewer' },
    ];
    const choice = teamChoice(team({ members, lead_position: 3 }), 42, agents, new Set());
    expect(choice).toMatchObject({ team: { primary: { applicationId: 6 }, others: [{ applicationId: 7, name: 'Release' }, { applicationId: 5 }] } });
  });

  it('a lead position out of the roster is no lead', () => {
    expect(teamChoice(team({ lead_position: 9 }), 42, agents, new Set())).toMatchObject({ team: { lead: false, primary: { applicationId: 5 } } });
  });

  it('does not run a team with an agent of another project, or one of this project the picker does not list', () => {
    const publicMember = team({ members: [{ application_id: 5, project_id: 42, version_id: 9, name: 'Coder' }, { application_id: 900, project_id: 1, version_id: 901, name: 'Helper' }] });
    expect(teamChoice(publicMember, 42, agents, new Set())).toEqual({ id: 3, name: 'Review crew', unavailable: 'other_project' });
    const gone = team({ members: [{ application_id: 5, project_id: 42, version_id: 9, name: 'Coder' }, { application_id: 99, project_id: 42, version_id: 1, name: 'Gone' }] });
    expect(teamChoice(gone, 42, agents, new Set())).toMatchObject({ unavailable: 'not_listed' });
  });

  it('does not run a team with a pipeline (a pipeline never runs on this computer), or with no members', () => {
    expect(teamChoice(team({}), 42, agents, new Set(['6']))).toMatchObject({ unavailable: 'pipeline' });
    expect(teamChoice(team({ members: [], lead_position: null }), 42, agents, new Set())).toMatchObject({ unavailable: 'no_members' });
  });
});
