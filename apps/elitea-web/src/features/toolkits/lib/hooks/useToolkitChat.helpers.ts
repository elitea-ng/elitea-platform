import type { ToolkitChatIndexLike } from './useToolkitChat.types';

/** `run()`'s "which input variables apply" resolution, split out of the hook body to stay under the §3.5 complexity budget. */
export function resolveRunInputVariables(
  toolInputVariables: Readonly<Record<string, unknown>> | undefined,
  index: ToolkitChatIndexLike | undefined,
  isCreateIndexMode: boolean,
  indexing: boolean,
): Readonly<Record<string, unknown>> {
  if (!isCreateIndexMode && indexing && index) {
    return index.metadata.index_configuration ?? {};
  }
  return toolInputVariables ?? {};
}

/** Whether an in-progress index's conversation should be recovered on mount — split out of the hook body to stay under the §3.5 complexity budget. */
export function computeShouldRecoverHistory(isCreateIndexMode: boolean, isIndexing: boolean, index: ToolkitChatIndexLike | undefined): boolean {
  return !isCreateIndexMode && isIndexing && Boolean(index?.metadata.conversation_id);
}
