import { describe, expect, it } from 'vitest';

import type { MarketplaceTeamDetail, MarketplaceTeamMember } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import { applyChoice, DISABLED_VALUE } from './hireModel';
import {
  membersStillUnbound,
  reusesMember,
  teamHireErrorMessage,
  teamHireRequest,
  unavailableMembers,
} from './teamHireModel';

const GITHUB = {
  items: [{ key: 'toolkit:github:Repo', kind: 'toolkit', type: 'github', name: 'Repo', tools: [], has_write_tools: false }],
  model_class: 'tool_calling',
  tier: 'low',
  has_write_tools: false,
};

function member(position: number, overrides: Partial<MarketplaceTeamMember> = {}): MarketplaceTeamMember {
  return {
    position,
    listing_id: String(position),
    version_id: String(position * 10),
    name: `Agent ${position}`,
    description: '',
    icon_url: null,
    version_name: 'v1',
    hireable: true,
    requirements: GITHUB,
    hired_application_id: null,
    ...overrides,
  };
}

const TEAM: MarketplaceTeamDetail = {
  version_id: '3',
  floor_policy: 'addressed',
  lead_position: 0,
  members: [member(1), member(2, { hired_application_id: '77' })],
};

function refusal(body: object): EliteaApiError {
  return new EliteaApiError({ kind: 'http', status: 409, body } as never);
}

describe('team hire model', () => {
  it('reuses an installed member under Install only', () => {
    expect(reusesMember(TEAM.members[1]!, 'install')).toBe(true);
    expect(reusesMember(TEAM.members[1]!, 'fork')).toBe(false);
    expect(reusesMember(TEAM.members[0]!, 'install')).toBe(false);
  });

  it('lists the members whose required toolkits are still unbound', () => {
    expect(membersStillUnbound(TEAM, 'install', {})).toEqual([1]);
    expect(membersStillUnbound(TEAM, 'fork', {})).toEqual([1, 2]);
    const bound = { 1: applyChoice({}, 'toolkit:github:Repo', '12') };
    expect(membersStillUnbound(TEAM, 'install', bound)).toEqual([]);
    const disabled = { ...bound, 2: applyChoice({}, 'toolkit:github:Repo', DISABLED_VALUE) };
    expect(membersStillUnbound(TEAM, 'fork', disabled)).toEqual([]);
  });

  it('builds the request with the bindings of the members the hire makes', () => {
    const choices = { 1: applyChoice({}, 'toolkit:github:Repo', '12') };
    expect(teamHireRequest('9', TEAM, 'install', '  Crew  ', choices)).toEqual({
      listing_id: '9',
      version_id: '3',
      mode: 'install',
      name: 'Crew',
      members: [{ position: 1, bindings: [{ requirement_key: 'toolkit:github:Repo', toolkit_id: '12' }] }],
    });
    expect(teamHireRequest('9', TEAM, 'fork', '', {}).members).toEqual([
      { position: 1, bindings: [] },
      { position: 2, bindings: [] },
    ]);
    expect(teamHireRequest('9', TEAM, 'fork', '', {})).not.toHaveProperty('name');
  });

  it('flags a member whose version cannot be hired', () => {
    expect(unavailableMembers({ ...TEAM, members: [member(1, { hireable: false })] })).toHaveLength(1);
    expect(unavailableMembers(TEAM)).toEqual([]);
  });

  it('says which member a refusal is about, and when the name is taken', () => {
    expect(teamHireErrorMessage(refusal({ error: 'binding_missing', member: 'Agent 1' }))).toBe(
      'Agent 1: Bind every required toolkit, or choose Disable for now.',
    );
    expect(teamHireErrorMessage(refusal({ error: 'team_name_taken' }))).toMatch(/already exists/);
    expect(teamHireErrorMessage(new Error('network'))).toBe('The hire failed. Try again.');
  });
});
