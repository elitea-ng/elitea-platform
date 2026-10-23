/**
 * The project's own skills, for the "~" menu's "Project skills" section: every
 * skill of the conversation's project (`listSkills`, page by page), read when
 * an agent's composer mounts and cached 30 s. Read up front, not when the menu
 * opens: a `~Name` typed or pasted without opening the menu must still resolve
 * to its id at send. The chat request names a picked
 * one by id; elitea-main reads it again itself under the caller's permissions,
 * so a skill listed here and gone (or hidden) by the send is refused there
 * (`project_skill_unknown`), never applied from this list's text.
 */
import { useCallback, useMemo } from 'react';

import { useQuery, useQueryClient } from '@tanstack/react-query';

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

function toEntries(rows: readonly SkillRow[]): readonly ProjectSkillEntry[] {
  return rows.flatMap((row): ProjectSkillEntry[] =>
    typeof row.name === 'string' && row.name.trim() !== '' && (typeof row.id === 'number' || typeof row.id === 'string')
      ? [{ id: String(row.id), name: row.name, ...(typeof row.description === 'string' && row.description !== '' ? { description: row.description } : {}) }]
      : [],
  );
}

export interface ProjectSkillList {
  /** `[]` while `enabled` is false, while the read is in flight and when it fails. */
  readonly entries: readonly ProjectSkillEntry[];
  /** Whether `entries` is the project's list (always true without a project: there is nothing to read). */
  readonly ready: boolean;
  /** The list, read now if it is not cached; rejects when the read fails. */
  readonly resolve: () => Promise<readonly ProjectSkillEntry[]>;
}

export function useProjectSkillList(projectId: string | undefined, enabled: boolean): ProjectSkillList {
  const usable = projectId !== undefined && projectId !== '';
  const queryClient = useQueryClient();
  const options = useMemo(
    () => ({
      queryKey: [...getListSkillsQueryKey(projectId ?? ''), 'chatInput.all'] as const,
      queryFn: () => readProjectSkills(projectId ?? ''),
      staleTime: 30_000,
    }),
    [projectId],
  );
  const query = useQuery({ ...options, enabled: usable && enabled });
  const entries = useMemo(() => (usable ? toEntries(query.data ?? []) : []), [usable, query.data]);
  const resolve = useCallback(async () => (usable ? toEntries(await queryClient.query(options)) : []), [usable, queryClient, options]);
  return { entries, ready: !usable || query.isSuccess, resolve };
}
