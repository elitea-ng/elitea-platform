/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/
 * useInternalToolsConfig.hooks.js` — toggles one entry in a conversation's
 * `meta.internal_tools` list, optimistic-updates the caller's local
 * conversation state first, then persists via `PUT .../conversation/...`,
 * reverting the local state if the persist fails.
 *
 * Uses `entities/conversation`'s already-landed `conversationApi.useEdit`
 * (`useConversationEditMutation`) rather than a new REST endpoint — this is
 * exactly `conversationEdit`, already ported by the `entities/conversation`
 * cluster this same run.
 *
 * **DEVIATION (disclosed):** `toastError`/`toastSuccess` -> `onError`/
 * `onSuccess` injected callbacks, the same established "caller's seam for
 * toast" substitution as `useChatCopyToClipboard.ts`
 * (no shared toast infra exists in this app yet).
 */
import { useCallback } from 'react';

import { conversationApi } from '@/entities/conversation';

/** Loose — matches `useChatCopyToClipboard.ts`'s `CopyableChatMessage` convention (no wire schema for raw client-held conversation state). */
export interface InternalToolsConversation {
  readonly id: string | number;
  readonly meta?: Readonly<Record<string, unknown>> & { readonly internal_tools?: readonly string[] };
}

export interface UseInternalToolsConfigParams {
  readonly projectId: string | number | undefined;
  readonly activeConversation: InternalToolsConversation | undefined;
  readonly setActiveConversation: (updater: (prev: InternalToolsConversation) => InternalToolsConversation) => void;
  readonly onSuccess?: () => void;
  readonly onError?: (error: unknown) => void;
}

interface OnInternalToolsConfigChangeInput {
  readonly key: string;
  readonly value: boolean;
}

export interface UseInternalToolsConfigResult {
  readonly onInternalToolsConfigChange: (input: OnInternalToolsConfigChangeInput) => Promise<void>;
  readonly isUpdatingInternalToolsConfig: boolean;
}

export function useInternalToolsConfig(params: UseInternalToolsConfigParams): UseInternalToolsConfigResult {
  const { projectId, activeConversation, setActiveConversation, onSuccess, onError } = params;
  const editMutation = conversationApi.useEdit();

  const onInternalToolsConfigChange = useCallback(
    async ({ key, value }: OnInternalToolsConfigChangeInput): Promise<void> => {
      if (!activeConversation || projectId === undefined) return;
      const previousTools = activeConversation.meta?.internal_tools ?? [];
      const newTools = value ? [...previousTools, key] : previousTools.filter((tool) => tool !== key);
      const newMeta = { ...activeConversation.meta, internal_tools: newTools };

      // Optimistic local update first, matching the baseline's own ordering.
      setActiveConversation((prev) => ({ ...prev, meta: newMeta }));

      try {
        await editMutation.mutateAsync({ projectId, id: activeConversation.id, meta: newMeta });
        onSuccess?.();
      } catch (error) {
        // Revert to the ORIGINAL meta — matching `useInternalToolsConfig.hooks.js:39-45`'s own revert shape (the original `meta` object, `internal_tools` included as-is).
        setActiveConversation((prev) => ({ ...prev, meta: activeConversation.meta ?? {} }));
        onError?.(error);
      }
    },
    [activeConversation, editMutation, onError, onSuccess, projectId, setActiveConversation],
  );

  return { onInternalToolsConfigChange, isUpdatingInternalToolsConfig: editMutation.isPending };
}
