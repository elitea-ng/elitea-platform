import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { t } from '@/shared/i18n';

import { deleteSkill, exportSkill, fetchSkill, updateSkill } from '../api/skillsApi';

import { skillQueryKeys } from './useSkills';

/**
 * The actions of the Workshop's selection section (ADR-0051) for skills: Add
 * tag, Export and Delete over every selected row. Each row is its own request;
 * a refusal on one does not stop the rest, and the caller is told how many
 * failed so the selection can keep exactly those.
 */
interface SkillBulkResult {
  readonly failed: readonly string[];
}

interface SkillBulkActions {
  readonly addTag: (ids: readonly string[], tag: string) => Promise<SkillBulkResult>;
  readonly exportMany: (ids: readonly string[]) => Promise<SkillBulkResult>;
  readonly deleteMany: (ids: readonly string[]) => Promise<SkillBulkResult>;
  readonly busy: boolean;
  /** The last action's refusal as a sentence; undefined when it succeeded. */
  readonly error: string | undefined;
}

async function eachId(ids: readonly string[], run: (id: string) => Promise<void>): Promise<SkillBulkResult> {
  const failed: string[] = [];
  for (const id of ids) {
    try {
      await run(id);
    } catch {
      failed.push(id);
    }
  }
  return { failed };
}

function downloadMarkdown(content: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([content], { type: 'text/markdown' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}

/** Adds `tag` to a skill's base version, keeping its name, description, instructions and the tags it has. */
async function addTagTo(projectId: string, id: string, tag: string): Promise<void> {
  const skill = await fetchSkill(projectId, id);
  const tags = skill.version_details?.tags ?? skill.tags ?? [];
  if (tags.includes(tag)) return;
  await updateSkill(projectId, id, {
    name: skill.name,
    description: skill.description ?? '',
    instructions: skill.version_details?.instructions ?? '',
    tags: [...tags, tag],
  });
}

export function useSkillBulkActions(projectId: string | undefined): SkillBulkActions {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const run = useCallback(
    async (action: () => Promise<SkillBulkResult>, failure: (count: number) => string, refresh: boolean): Promise<SkillBulkResult> => {
      setBusy(true);
      setError(undefined);
      try {
        const result = await action();
        if (result.failed.length > 0) setError(failure(result.failed.length));
        if (refresh && projectId !== undefined) await queryClient.invalidateQueries({ queryKey: skillQueryKeys.all(projectId) });
        return result;
      } finally {
        setBusy(false);
      }
    },
    [queryClient, projectId],
  );

  return {
    busy,
    error,
    addTag: (ids, tag) =>
      run(
        () => eachId(ids, (id) => addTagTo(projectId ?? '', id, tag)),
        (count) => t('features.skills.bulk.tagFailed', 'The tag could not be added to {{count}} skills.', { count }),
        true,
      ),
    exportMany: (ids) =>
      run(
        () =>
          eachId(ids, async (id) => {
            downloadMarkdown(await exportSkill(projectId ?? '', id), `skill-${id}.md`);
          }),
        (count) => t('features.skills.bulk.exportFailed', '{{count}} skills could not be exported.', { count }),
        false,
      ),
    deleteMany: (ids) =>
      run(
        () =>
          eachId(ids, async (id) => {
            await deleteSkill(projectId ?? '', id);
          }),
        (count) => t('features.skills.bulk.deleteFailed', '{{count}} skills could not be deleted.', { count }),
        true,
      ),
  };
}
