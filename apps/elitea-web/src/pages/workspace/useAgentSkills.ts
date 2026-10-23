/**
 * The selected agent version's skills, for the composer's "/" menu: the
 * same read the web chat's "~" menu makes (`application_skills`, the
 * skills attached to that version, under the person's own permissions).
 * The host resolves a pick against the version it runs, so a skill listed
 * here and gone by the send is refused there (`skill_unknown`).
 */
import { useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { composer } from '@/features/workspace';
import { getListSkillsQueryKey, listSkills, useListApplicationSkills } from '@/shared/api/generated/skills/skills';
import { unwrapList, unwrapListPage } from '@/shared/api/unwrap';

interface SkillRow {
  id?: unknown;
  name?: unknown;
  description?: unknown;
}

/** One skill the composer's "/" menu offers (the shape `composer.matchingSkills` filters). */
export type ComposerSkill = Parameters<typeof composer.matchingSkills>[0][number];

/** `[]` until an agent version is picked, while it loads and when the read fails. */
export function useAgentSkills(projectId: number, versionId: string): ComposerSkill[] {
  const version = Number(versionId);
  const enabled = versionId !== '' && Number.isInteger(version) && version > 0;
  const query = useListApplicationSkills(String(projectId), enabled ? version : 0, { query: { enabled, staleTime: 30_000 } });
  return useMemo(
    () =>
      unwrapList<SkillRow>(query.data, 'workspace.skills').flatMap((row) =>
        typeof row.name === 'string' && row.name.trim() !== ''
          ? [{ id: String(row.id), name: row.name, ...(typeof row.description === 'string' && row.description !== '' ? { description: row.description } : {}) }]
          : [],
      ),
    [query.data],
  );
}

/** One page of the project's skills (the server's largest). */
const PROJECT_SKILLS_PAGE = 100;
/** The most of the project's skills read, in pages: past it the rest are not offered. */
const PROJECT_SKILLS_MAX = 1000;

/**
 * Every skill of the project, page by page (`page`/`page_size`, the server's
 * paging), until a page comes back short, the server's `total` is reached or
 * {@link PROJECT_SKILLS_MAX} were read.
 */
async function readProjectSkills(projectId: number): Promise<SkillRow[]> {
  const rows: SkillRow[] = [];
  for (let page = 1; rows.length < PROJECT_SKILLS_MAX; page += 1) {
    const read = unwrapListPage<SkillRow>(await listSkills(String(projectId), { page, page_size: PROJECT_SKILLS_PAGE }), 'workspace.projectSkills');
    rows.push(...read.rows);
    if (read.rows.length < PROJECT_SKILLS_PAGE || rows.length >= read.total) break;
  }
  return rows.slice(0, PROJECT_SKILLS_MAX);
}

/**
 * The bound project's skills that the agent version does not carry (by
 * name, ignoring case), for the menu's "Project skills" (desktop only):
 * all of them (up to {@link PROJECT_SKILLS_MAX}), read once `enabled` (the
 * composer was focused, or `/help` is open, on a thread that runs here),
 * cached 30 s like the version's. The host reads a picked one again
 * itself, by id, so one listed here and gone by the send is refused there
 * (`skill_unknown`). `[]` while it loads and when the read fails.
 */
export function useProjectSkills(projectId: number, enabled: boolean, attached: readonly ComposerSkill[]): ComposerSkill[] {
  const on = enabled && Number.isInteger(projectId) && projectId > 0;
  const query = useQuery({
    queryKey: [...getListSkillsQueryKey(String(projectId)), 'workspace.all'] as const,
    queryFn: () => readProjectSkills(projectId),
    enabled: on,
    staleTime: 30_000,
  });
  return useMemo(() => {
    if (!on) return [];
    const taken = new Set(attached.map((skill) => skill.name.trim().toLowerCase()));
    return (query.data ?? []).flatMap((row) =>
      typeof row.name === 'string' && row.name.trim() !== '' && !taken.has(row.name.trim().toLowerCase())
        ? [
            {
              id: String(row.id),
              name: row.name,
              project: true as const,
              ...(typeof row.description === 'string' && row.description !== '' ? { description: row.description } : {}),
            },
          ]
        : [],
    );
  }, [on, query.data, attached]);
}
