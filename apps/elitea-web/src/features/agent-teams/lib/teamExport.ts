/**
 * Export of a saved team (issue 3010): a zip of `team.yaml` and one
 * `members/<slug>.agent.md` per member, each the member's markdown export at
 * the version the team pins. Workshop › Agents › Import reads the same zip
 * back into the agents and the team, so a team round-trips.
 *
 * `team.yaml` (`elitea.team/v1`, issue 3137):
 *
 *     schema: elitea.team/v1
 *     kind: team
 *     name: Dev team
 *     description: …            # optional
 *     floor_policy: lead        # addressed | lead
 *     members:                  # roster order, paths inside the zip
 *       - members/coordinator.agent.md
 *     lead: members/coordinator.agent.md   # with floor_policy lead
 *
 * A member with uploaded avatar media exports as an `.agent.zip` (its `.md`
 * plus `avatars/<slug>/<slot>.<ext>`); its files are merged into the team zip
 * under the member's own slug, `avatars/<member>/…`, and the `avatar:` paths
 * of its front matter are rewritten to match.
 */
import { dump as dumpYaml } from 'js-yaml';
import JSZip from 'jszip';

import type { AgentTeam } from '@/shared/api/generated/model';

/** One member's markdown export: the `.agent.md` text, or the bytes of its `.agent.zip`. */
export type MemberExport = string | Uint8Array;

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
    schema: 'elitea.team/v1',
    kind: 'team',
    name: team.name,
    ...(team.description === '' ? {} : { description: team.description }),
    floor_policy: team.floor_policy,
    members: [...paths],
    ...(lead === undefined ? {} : { lead }),
  });
}

const FRONT_MATTER_END = /\r?\n---[ \t]*(?:\r?\n|$)/;

/** `text` with each old path of `renames` replaced in its front matter only (the body is the agent's instructions). */
function renameInFrontMatter(text: string, renames: ReadonlyMap<string, string>): string {
  const end = FRONT_MATTER_END.exec(text.slice(3));
  if (!text.startsWith('---') || end === null) return text;
  const split = 3 + end.index;
  let front = text.slice(0, split);
  for (const [from, to] of renames) front = front.split(from).join(to);
  return front + text.slice(split);
}

/** Merges a member's `.agent.zip` into the team zip: its markdown at `path`, its avatars under `avatars/<member slug>/`. */
async function addMemberZip(zip: JSZip, path: string, bytes: Uint8Array): Promise<void> {
  const member = await JSZip.loadAsync(bytes);
  const files = Object.keys(member.files).filter((name) => !(member.files[name]?.dir ?? true) && !name.startsWith('__MACOSX/'));
  const markdown = files.filter((name) => /\.md$/i.test(name)).sort()[0];
  const slug = (path.split('/').pop() ?? path).replace(/\.agent\.md$/, '');
  const renames = new Map<string, string>();
  for (const name of files) {
    if (name === markdown || !name.startsWith('avatars/')) continue;
    const target = `avatars/${slug}/${name.split('/').pop() ?? name}`;
    renames.set(name, target);
    zip.file(target, (await member.files[name]?.async('uint8array')) ?? new Uint8Array());
  }
  const text = markdown === undefined ? '' : ((await member.files[markdown]?.async('string')) ?? '');
  zip.file(path, renameInFrontMatter(text, renames));
}

/** The zip of a team: `team.yaml` plus each member's export, given in roster order. */
export async function buildTeamZip(team: AgentTeam, memberExports: readonly MemberExport[]): Promise<Blob> {
  const paths = memberPaths(team);
  const zip = new JSZip();
  zip.file('team.yaml', teamManifestYaml(team, paths));
  for (const [index, path] of paths.entries()) {
    const exported = memberExports[index] ?? '';
    if (typeof exported === 'string') zip.file(path, exported);
    else await addMemberZip(zip, path, exported);
  }
  return zip.generateAsync({ type: 'blob' });
}

/** The zip's file name. */
export function teamZipName(team: AgentTeam): string {
  return `${slugOf(team.name)}.team.zip`;
}
