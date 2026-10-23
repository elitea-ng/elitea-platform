/**
 * The project's own skills, for the "~" menu's "Project skills" section: every
 * skill of the conversation's project (`listSkills`, page by page), read once
 * the menu is first opened and cached 30 s. The chat request names a picked
 * one by id; elitea-main reads it again itself under the caller's permissions,
 * so a skill listed here and gone (or hidden) by the send is refused there
 * (`project_skill_unknown`), never applied from this list's text.
 */
import { useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { getListSkillsQueryKey, listSkills } from '@/shared/api/generated/skills/skills';
import { unwrapListPage } from '@/shared/api/unwrap';

interface SkillRow {
  id?: unknown;
  name?: unknown;
  description?: unknown;
}

/** One project skill the menu offers. */
export interface ProjectSkillEntry {
  readonly id: string;
  readonly name: string;
  readonly description?: string | undefined;
}

/** One page of the project's skills (the server's largest). */
const PAGE_SIZE = 100;
/** The most of the project's skills read, in pages: past it the rest are not offered. */
const MAX_SKILLS = 1000;

async function readProjectSkills(projectId: string): Promise<SkillRow[]> {
  const rows: SkillRow[] = [];
  for (let page = 1; rows.length < MAX_SKILLS; page += 1) {
    const read = unwrapListPage<SkillRow>(await listSkills(projectId, { page, page_size: PAGE_SIZE }), 'chatInput.projectSkills');
    rows.push(...read.rows);
    if (read.rows.length < PAGE_SIZE || rows.length >= read.total) break;
  }
  return rows.slice(0, MAX_SKILLS);
}

/** `[]` until `enabled` has been true once, while it loads and when the read fails. */
export function useProjectSkillList(projectId: string | undefined, enabled: boolean): readonly ProjectSkillEntry[] {
  const usable = projectId !== undefined && projectId !== '';
  const query = useQuery({
    queryKey: [...getListSkillsQueryKey(projectId ?? ''), 'chatInput.all'] as const,
    queryFn: () => readProjectSkills(projectId ?? ''),
    enabled: usable && enabled,
    staleTime: 30_000,
  });
  return useMemo(
    () =>
      usable
        ? (query.data ?? []).flatMap((row): ProjectSkillEntry[] =>
            typeof row.name === 'string' && row.name.trim() !== '' && (typeof row.id === 'number' || typeof row.id === 'string')
              ? [{ id: String(row.id), name: row.name, ...(typeof row.description === 'string' && row.description !== '' ? { description: row.description } : {}) }]
              : [],
          )
        : [],
    [usable, query.data],
  );
}
