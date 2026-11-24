/**
 * Export of a saved team (issue 3010): a zip of `team.yaml` and one
 * `members/<slug>.agent.md` per member, each the member's markdown export at
 * the version the team pins. Workshop › Agents › Import reads the same zip
 * back into the agents and the team, so a team round-trips.
 *
 * `team.yaml`:
 *
 *     kind: team
 *     name: Dev team
 *     description: …            # optional
 *     floor_policy: lead        # addressed | lead
 *     members:                  # roster order, paths inside the zip
 *       - members/dev-coordinator.agent.md
 *     lead: members/dev-coordinator.agent.md   # with floor_policy lead
 */
import { dump as dumpYaml } from 'js-yaml';
import JSZip from 'jszip';

import type { AgentTeam } from '@/shared/api/generated/model';

/** A file-name slug of an agent name; non-Latin names fall back to `agent`. */
function slugOf(name: string): string {
  const slug = name
    .toLowerCase()
    .normalize('NFKD')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return slug === '' ? 'agent' : slug;
}

/** One distinct `members/<slug>.agent.md` path per member, in roster order. */
export function memberPaths(team: AgentTeam): string[] {
  const used = new Set<string>();
  return team.members.map((member, index) => {
    const base = slugOf(member.name === '' ? `agent-${String(index + 1)}` : member.name);
    let path = `members/${base}.agent.md`;
    for (let n = 2; used.has(path); n += 1) path = `members/${base}-${String(n)}.agent.md`;
    used.add(path);
    return path;
  });
}

/** The `team.yaml` of a team's export. */
function teamManifestYaml(team: AgentTeam, paths: readonly string[]): string {
  const lead = team.lead_position === null || team.lead_position === undefined ? undefined : paths[team.lead_position - 1];
  return dumpYaml({
    kind: 'team',
    name: team.name,
    ...(team.description === '' ? {} : { description: team.description }),
    floor_policy: team.floor_policy,
    members: [...paths],
    ...(lead === undefined ? {} : { lead }),
  });
}

/** The zip of a team: `team.yaml` plus each member's markdown, given in roster order. */
export async function buildTeamZip(team: AgentTeam, memberMarkdown: readonly string[]): Promise<Blob> {
  const paths = memberPaths(team);
  const zip = new JSZip();
  zip.file('team.yaml', teamManifestYaml(team, paths));
  paths.forEach((path, index) => zip.file(path, memberMarkdown[index] ?? ''));
  return zip.generateAsync({ type: 'blob' });
}

/** The zip's file name. */
export function teamZipName(team: AgentTeam): string {
  return `${slugOf(team.name)}.team.zip`;
}
