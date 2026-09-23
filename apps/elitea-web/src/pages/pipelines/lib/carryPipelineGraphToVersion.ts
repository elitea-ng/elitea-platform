import type { QueryClient } from '@tanstack/react-query';

import type { PipelineGraphDraft } from '@/features/pipelines';
import { getUpdateApplicationVersionQueryOptions } from '@/shared/api/generated/applications/applications';

export interface CarryPipelineGraphArgs {
  readonly projectId: string;
  readonly applicationId: number;
  /** The version the "Save As Version" POST just minted. */
  readonly versionId: number;
  /** The LIVE flow-editor graph, read at click time via `usePipelineGraphDraft`. */
  readonly graph: PipelineGraphDraft;
}

/**
 * Copy the live flow graph onto a version that was just created by
 * `POST /elitea_core/versions/prompt_lib/{projectId}/{applicationId}`.
 *
 * Main now persists `pipeline_settings` during creation. This caller still
 * creates from stored instructions, then reads the live canvas on success.
 * This PUT therefore remains necessary to preserve unsaved nodes and positions.
 * Removing it requires passing an admitted live graph into the original POST.
 * The pipeline versioning journey tests both Main's creation contract and
 * this editor's unsaved-canvas behavior separately.
 *
 * No route is invented here: this is the same PUT the Save button already
 * uses, aimed at the id the POST just returned. It is deliberately NOT folded
 * into `entities/application-form`'s `useSaveApplicationVersion` — that hook
 * binds its version id at render time, and the id this call needs does not
 * exist until the POST resolves.
 *
 * `staleTime: 0` is load-bearing for the same reason
 * `features/agents/model/useSetDefaultVersion.ts` documents it: orval models
 * this PUT as a QUERY, so it goes through `fetchQuery` against a client whose
 * default `staleTime` is 30s (`app/providers/queryClient.ts`). Two
 * save-as-versions of an unchanged graph inside that window would otherwise
 * replay the cache entry and send no second request while reporting success.
 *
 * Throws on failure — `eliteaFetch` rejects with `EliteaApiError`. The caller
 * surfaces it; the created version is real either way, it just holds the
 * previously stored graph.
 */
export async function carryPipelineGraphToVersion(
  queryClient: QueryClient,
  { projectId, applicationId, versionId, graph }: CarryPipelineGraphArgs,
): Promise<void> {
  const options = getUpdateApplicationVersionQueryOptions(projectId, applicationId, versionId, {
    instructions: graph.instructions,
    pipeline_settings: { ...graph.pipelineSettings },
  });
  await queryClient.query({ ...options, staleTime: 0 });
}
