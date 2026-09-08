/**
 * Saving toolkit settings, and deleting a wiki's stored objects.
 *
 * BOTH ROUTES ARE SERVED, which is worth saying because the artifact READ paths
 * are not (parity/notes/deepwiki-artifact-store.md, issue #665):
 *
 *   PUT  /elitea_core/tool/prompt_lib/{project_id}/{tool_id}   — v2.yaml
 *   POST /artifacts/objects/{projectID}/{bucket}:batchDelete    — v2.yaml
 *
 * batchDelete rather than a per-object DELETE loop. The legacy screen deleted a
 * wiki one artifact at a time and counted successes into a message
 * ("Deleted 3/7 artifacts for wiki X"), so a partial failure left a half-deleted
 * wiki that still listed. One request either deletes the set or reports which
 * keys it could not.
 */
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { ToolkitSettings } from '@/entities/wiki';

const WIKI_BUCKET = 'wiki-artifacts';

export interface SaveSettingsInput {
  readonly projectId: string | number;
  readonly toolkitId: string | number;
  /** The toolkit as last read, so the PUT carries every field it already had. */
  readonly toolkit: Record<string, unknown>;
  readonly settings: ToolkitSettings;
}

/**
 * Save the toolkit with new settings.
 *
 * The whole toolkit is sent, not a settings patch: the route is a PUT and
 * replaces the resource, so posting only `settings` would clear every other
 * field the toolkit carries. The legacy code spread the toolkit for the same
 * reason, and it is preserved rather than tidied into a PATCH the API does not
 * serve.
 */
export function useSaveWikiSettings(): UseMutationResult<unknown, Error, SaveSettingsInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SaveSettingsInput) => {
      const path = `/elitea_core/tool/prompt_lib/${String(input.projectId)}/${String(input.toolkitId)}`;
      return eliteaFetch<unknown>(path, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...input.toolkit, settings: input.settings }),
      });
    },
    onSuccess: () => {
      // The wiki list is derived from the configured repository, so a settings
      // change can make a different set of wikis visible. Not invalidating
      // leaves the browser showing the previous repository's wikis.
      void queryClient.invalidateQueries({ queryKey: ['deepwiki'] });
    },
  });
}

export interface DeleteWikiInput {
  readonly projectId: string | number;
  /** Every object key belonging to the wiki, manifest included. */
  readonly keys: readonly string[];
  readonly bucket?: string;
}

export interface DeleteWikiResult {
  readonly deleted: number;
  /** Keys the server could not delete, so a partial result is not silent. */
  readonly failed: readonly string[];
}

/**
 * What `:batchDelete` answers: `{deleted: string[], failed: [{key, code,
 * message}]}` — see BatchDeleteObjects in elitea-main
 * internal/api/v2/artifacts/objects.go. The field is `failed`. An earlier
 * reading of this envelope looked for `errors`, which the server never
 * writes, so a half-deleted wiki reported as a clean delete and the
 * "these remain" warning could not render; its test served the same wrong
 * shape, which is how it survived. `errors` is still accepted in case an
 * older envelope form ever answers.
 */
interface BatchDeleteFailure {
  key?: string;
}
interface BatchDeleteBody {
  deleted?: string[];
  failed?: BatchDeleteFailure[];
  errors?: BatchDeleteFailure[];
}
interface BatchDeleteEnvelope extends BatchDeleteBody {
  data?: BatchDeleteBody;
}

/** The keys the server reports it could NOT delete, whichever field names them. */
function failedKeysFrom(body: BatchDeleteBody): string[] {
  return [...(body.failed ?? []), ...(body.errors ?? [])]
    .map((e) => e.key)
    .filter((key): key is string => typeof key === 'string');
}

/** Delete a wiki's objects in ONE request. */
export function useDeleteWiki(): UseMutationResult<DeleteWikiResult, Error, DeleteWikiInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: DeleteWikiInput): Promise<DeleteWikiResult> => {
      if (input.keys.length === 0) {
        // Refused rather than sent. An empty batch is a request that reports
        // success having deleted nothing, which reads as a deleted wiki.
        throw new Error('No objects were named for deletion.');
      }
      const bucket = input.bucket ?? WIKI_BUCKET;
      const path =
        `/artifacts/objects/${encodeURIComponent(String(input.projectId))}` +
        `/${encodeURIComponent(bucket)}:batchDelete`;
      const response = await eliteaFetch<BatchDeleteEnvelope>(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ keys: input.keys }),
      });
      const body = response.data ?? response;
      return { deleted: (body.deleted ?? []).length, failed: failedKeysFrom(body) };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['deepwiki'] });
    },
  });
}
