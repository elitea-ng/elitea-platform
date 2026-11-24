import { describe, expect, it } from 'vitest';

import { load as loadYaml } from 'js-yaml';
import JSZip from 'jszip';

import type { AgentTeam } from '@/shared/api/generated/model';

import { buildTeamZip, memberPaths, teamZipName } from './teamExport';

const team: AgentTeam = {
  id: 7,
  project_id: 22,
  name: 'Dev Team',
  description: 'Builds the platform',
  floor_policy: 'lead',
  lead_position: 2,
  members: [
    { application_id: 1, project_id: 22, version_id: 11, name: 'Dev Implementer' },
    { application_id: 2, project_id: 22, version_id: 12, name: 'Dev Coordinator' },
    { application_id: 3, project_id: 22, version_id: 13, name: 'Dev Implementer' },
  ],
  created_by: 1,
  created_at: '2026-11-24T00:00:00Z',
  updated_at: '2026-11-24T00:00:00Z',
};

describe('team export (issue 3010)', () => {
  it('gives every member its own file, even when two share a name', () => {
    expect(memberPaths(team)).toEqual([
      'members/dev-implementer.agent.md',
      'members/dev-coordinator.agent.md',
      'members/dev-implementer-2.agent.md',
    ]);
    expect(teamZipName(team)).toBe('dev-team.team.zip');
  });

  it('writes a team.yaml the import reads: roster order, lead and floor', async () => {
    const blob = await buildTeamZip(team, ['one', 'two', 'three']);
    const zip = await JSZip.loadAsync(await blob.arrayBuffer());
    const manifest = loadYaml((await zip.files['team.yaml']?.async('string')) ?? '') as Record<string, unknown>;
    expect(manifest).toEqual({
      kind: 'team',
      name: 'Dev Team',
      description: 'Builds the platform',
      floor_policy: 'lead',
      members: memberPaths(team),
      lead: 'members/dev-coordinator.agent.md',
    });
    expect(await zip.files['members/dev-coordinator.agent.md']?.async('string')).toBe('two');
  });

  it('leaves out the lead of an addressed team', async () => {
    const addressed = { ...team, floor_policy: 'addressed', lead_position: null };
    const zip = await JSZip.loadAsync(await (await buildTeamZip(addressed, ['a', 'b', 'c'])).arrayBuffer());
    const manifest = loadYaml((await zip.files['team.yaml']?.async('string')) ?? '') as Record<string, unknown>;
    expect(manifest['lead']).toBeUndefined();
    expect(manifest['floor_policy']).toBe('addressed');
  });
});
