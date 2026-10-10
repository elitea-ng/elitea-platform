/**
 * The selected agent version's skills, for the composer's "/" menu: the
 * same read the web chat's "~" menu makes (`application_skills`, the
 * skills attached to that version, under the person's own permissions).
 * The host resolves a pick against the version it runs, so a skill listed
 * here and gone by the send is refused there (`skill_unknown`).
 */
import { useMemo } from 'react';

import { composer } from '@/features/workspace';
import { useListApplicationSkills } from '@/shared/api/generated/skills/skills';
import { unwrapList } from '@/shared/api/unwrap';

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
