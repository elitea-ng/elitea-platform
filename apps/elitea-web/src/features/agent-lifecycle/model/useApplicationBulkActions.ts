import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { deleteApplication, getApplication, getListApplicationsQueryKey, updateApplicationVersion } from '@/shared/api/generated/applications/applications';
import { serverErrorOf } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

import { fetchExportDocument } from '../api/lifecycleApi';

/**
 * The actions of the Workshop's selection section (ADR-0051) for agents and
 * pipelines: Add tag, Export and Delete, each run over every selected row.
 *
 * Each row is its own request. A refusal on one row does not stop the rest;
 * the person is told how many rows it failed on, and the selection keeps only
 * those rows so Retry is one click.
 */
interface ApplicationBulkResult {
  /** The ids the action did not complete on. */
  readonly failed: readonly string[];
  /** The server's own sentence for the first refusal, when it sent one. */
  readonly reason?: string;
}

interface ApplicationBulkActions {
  readonly addTag: (ids: readonly string[], tag: string) => Promise<ApplicationBulkResult>;
  readonly exportMany: (ids: readonly string[]) => Promise<ApplicationBulkResult>;
  readonly deleteMany: (ids: readonly string[]) => Promise<ApplicationBulkResult>;
  readonly busy: boolean;
  /** The last action's refusal, as a sentence; undefined when it succeeded. */
  readonly error: string | undefined;
}

interface VersionDetail {
  readonly id?: number | string;
  readonly tags?: readonly { readonly id?: number; readonly name?: string | null }[];
}

/** Runs `run` over `ids` one at a time, collecting the ids it threw on. */
async function eachId(ids: readonly string[], run: (id: string) => Promise<void>): Promise<ApplicationBulkResult> {
  const failed: string[] = [];
  let reason: string | undefined;
  for (const id of ids) {
    try {
      await run(id);
    } catch (error) {
      failed.push(id);
      reason ??= serverErrorOf(error);
    }
  }
  return reason === undefined ? { failed } : { failed, reason };
}

/** One entity's export document, handed to the browser as a JSON file. */
function downloadJson(document: unknown, filename: string): void {
  const url = URL.createObjectURL(new Blob([JSON.stringify(document, null, 2)], { type: 'application/json' }));
  const link = window.document.createElement('a');
  link.href = url;
  link.download = filename;
  link.click();
  // Revoked late: some browsers start the download after the click returns.
  setTimeout(() => URL.revokeObjectURL(url), 5000);
}

/** Adds `tag` to the version the list shows for one application, keeping the tags it already has. */
async function addTagTo(projectId: string, id: string, tag: string): Promise<void> {
  const envelope = await getApplication(projectId, Number(id));
  const detail = (envelope as unknown as { readonly data: { readonly version_details?: VersionDetail } }).data.version_details;
  if (detail?.id === undefined) throw new Error('no version to tag');
  const existing = (detail.tags ?? []).flatMap((entry) => (entry.name === undefined || entry.name === null ? [] : [{ name: entry.name }]));
  if (existing.some((entry) => entry.name === tag)) return;
  await updateApplicationVersion(projectId, Number(id), Number(detail.id), { tags: [...existing, { name: tag }] });
}

export function useApplicationBulkActions(projectId: string | undefined, kind: 'agent' | 'pipeline'): ApplicationBulkActions {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const run = useCallback(
    async (action: () => Promise<ApplicationBulkResult>, failure: (count: number) => string, refresh: boolean): Promise<ApplicationBulkResult> => {
      setBusy(true);
      setError(undefined);
      try {
        const result = await action();
        // The count, then the server's reason for the first refusal: a
        // guessed cause ("must be unpublished first") sent people after the
        // wrong fix whenever the refusal was something else.
        if (result.failed.length > 0) setError([failure(result.failed.length), result.reason].filter(Boolean).join(' '));
        if (refresh) await queryClient.invalidateQueries({ queryKey: getListApplicationsQueryKey(projectId ?? '') }).catch(() => undefined);
        return result;
      } finally {
        setBusy(false);
      }
    },
    [queryClient, projectId],
  );

  const noun = kind === 'agent' ? t('features.agentLifecycle.bulk.agents', 'agents') : t('features.agentLifecycle.bulk.pipelines', 'pipelines');

  return {
    busy,
    error,
    addTag: (ids, tag) =>
      run(
        () => eachId(ids, (id) => addTagTo(projectId ?? '', id, tag)),
        (count) => t('features.agentLifecycle.bulk.tagFailed', 'The tag could not be added to {{count}} {{noun}}.', { count, noun }),
        true,
      ),
    exportMany: (ids) =>
      run(
        () =>
          eachId(ids, async (id) => {
            downloadJson(await fetchExportDocument(projectId ?? '', Number(id)), `${kind}-${id}.json`);
          }),
        (count) => t('features.agentLifecycle.bulk.exportFailed', '{{count}} {{noun}} could not be exported.', { count, noun }),
        false,
      ),
    deleteMany: (ids) =>
      run(
        () =>
          eachId(ids, async (id) => {
            await deleteApplication(projectId ?? '', Number(id));
          }),
        (count) => t('features.agentLifecycle.bulk.deleteFailedCount', '{{count}} {{noun}} could not be deleted.', { count, noun }),
        true,
      ),
  };
}
