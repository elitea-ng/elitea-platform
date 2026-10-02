/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props
 * budgets (§3.5) — real internal-tools-config persistence.
 * `processes/chat/model/useInternalToolsConfig.ts` cannot be imported here
 * — `widgets/` may not import `processes/` (`no-upward-from-widgets`,
 * `.dependency-cruiser.cjs`) — so its small optimistic-update-then-PUT
 * logic is reproduced locally against the same `conversationApi.useEdit()`
 * mutation it itself uses, rather than duplicating a whole new endpoint.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { conversationApi } from '@/entities/conversation';
import { agentEditorHooks } from '@/features/agents';


export interface UseChatBoxInternalToolsParams {
  readonly conversationId: string | number | undefined;
  readonly conversationMeta: Readonly<Record<string, unknown>> | undefined;
  readonly projectId: string | number | undefined;
  readonly isAgentsPage: boolean | undefined;
}

export interface UseChatBoxInternalToolsResult {
  readonly internalToolsButtonTools: readonly { readonly key: string; readonly label: string; readonly enabled: boolean }[];
  readonly handleInternalToolChange: (toolKey: string, enabled: boolean) => void;
  readonly isUpdatingInternalToolsConfig: boolean;
  readonly getInternalToolsForSend: () => Promise<readonly string[]>;
}

export function useChatBoxInternalTools({
  conversationId,
  conversationMeta,
  projectId,
  isAgentsPage,
}: UseChatBoxInternalToolsParams): UseChatBoxInternalToolsResult {
  const { mutateAsync: editConversation } = conversationApi.useEdit();
  const scope = `${String(projectId)}:${String(conversationId)}`;
  const state = useRef({ scope, meta: conversationMeta ?? {}, persistedMeta: conversationMeta ?? {}, pending: Promise.resolve(), revision: 0 });
  const [meta, setMeta] = useState(conversationMeta ?? {});
  const [pendingCount, setPendingCount] = useState(0);
  useEffect(() => {
    state.current = { scope, meta: conversationMeta ?? {}, persistedMeta: conversationMeta ?? {}, pending: Promise.resolve(), revision: 0 };
    setMeta(conversationMeta ?? {});
    setPendingCount(0);
  }, [scope, conversationMeta]);

  const handleInternalToolChange = useCallback((key: string, enabled: boolean) => {
    const current = state.current;
    const previous = current.meta;
    const tools = Array.isArray(previous['internal_tools']) ? previous['internal_tools'] as string[] : [];
    const next = { ...previous, internal_tools: enabled ? [...new Set([...tools, key])] : tools.filter((tool) => tool !== key) };
    current.meta = next;
    const revision = ++current.revision;
    setMeta(next);
    if (conversationId === undefined || projectId === undefined) return;
    setPendingCount((count) => count + 1);
    current.pending = current.pending.catch(() => undefined).then(async () => {
      await editConversation({ projectId, id: conversationId, meta: next });
      current.persistedMeta = next;
    }).catch((error: unknown) => {
      if (state.current === current && current.revision === revision) {
        current.meta = current.persistedMeta;
        setMeta(current.persistedMeta);
      }
      throw error;
    }).finally(() => {
      if (state.current === current) setPendingCount((count) => count - 1);
    });
    // Keep failures observable by the send barrier without an unhandled rejection.
    void current.pending.catch(() => undefined);
  }, [conversationId, projectId, editConversation]);

  const getInternalToolsForSend = useCallback(async () => {
    const current = state.current;
    let pending;
    do {
      pending = current.pending;
      await pending;
    } while (pending !== current.pending);
    if (state.current !== current) throw new Error('Conversation changed before sending');
    return Array.isArray(current.meta['internal_tools']) ? current.meta['internal_tools'] as string[] : [];
  }, []);

  const availableInternalTools = agentEditorHooks.useAvailableInternalTools({ includeAgentOnly: !!isAgentsPage });
  const internalToolsButtonTools = useMemo(
    () =>
      availableInternalTools.map((tool) => ({
        key: tool.name,
        label: tool.title,
        enabled: (Array.isArray(meta['internal_tools']) ? meta['internal_tools'] : []).includes(tool.name),
      })),
    [availableInternalTools, meta],
  );

  return { internalToolsButtonTools, handleInternalToolChange, isUpdatingInternalToolsConfig: pendingCount > 0, getInternalToolsForSend };
}
