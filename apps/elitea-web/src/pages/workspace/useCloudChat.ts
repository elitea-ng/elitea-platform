/**
 * A thread that runs in the cloud (ADR-0034), chatted with from the desktop:
 * the SAME server conversation, the same agent-execution route and the same
 * streaming transport the web chat page uses — never the local agent loop.
 *
 * - The conversation's messages come from the server (polled while nothing
 *   streams, so a cloud turn that ran elsewhere shows up) and are converted to
 *   the chat's own message shape, so tool steps, errors with their support
 *   details and the rest render as they do in the chat.
 * - `send` starts an execution for the conversation's agent participant
 *   (`agent.execute.application.v1`) and `useChatStreamTransport` folds its
 *   stream into the transcript; `stop` cancels it server-side.
 * - Only a thread whose executor is `cloud` may send; a `returning` thread is
 *   read-only (its transcript still follows the server).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { convertConversationToChatHistory, useChatStreamTransport, type ChatMessage, type ConversationDetails } from '@/features/chat-messages';
import { getExecutionTokens } from '@/features/mcps';
import { t } from '@/shared/i18n';
import { useActivityFeed } from '@/widgets/desktop-shell';

import { useChatPageData } from '@/pages/chat/useChatPageData';

const SERVER_POLL_MS = 3000;

export interface CloudChat {
  /** The transcript: the server's messages, with the live turn folded in. */
  history: readonly ChatMessage[];
  /** The first read of the conversation is pending. */
  loading: boolean;
  streaming: boolean;
  /** The thread's agent and conversation are known, and its executor lets a message through. */
  canSend: boolean;
  /** Why the last send did not start. */
  error: string | null;
  send: (text: string) => Promise<boolean>;
  stop: () => void;
  userId: string | undefined;
  projectId: string | undefined;
}

export interface CloudChatInput {
  conversationId: string;
  /** The folder, for the activity feed (the island and the tray). */
  workspace: { id: string; name: string };
  /** The thread is away from this computer (cloud or returning): the transcript is the server's. */
  active: boolean;
  /** Its executor is `cloud`: a message may be sent. */
  mayChat: boolean;
}

interface AgentParticipant {
  id: number;
  name: string | undefined;
}

/** The conversation's agent: the participant a cloud turn is addressed to. */
function agentParticipant(participants: readonly unknown[] | undefined): AgentParticipant | undefined {
  for (const raw of participants ?? []) {
    const row = raw as { id?: unknown; entity_name?: unknown; name?: unknown };
    const id = Number(row.id);
    if (row.entity_name === 'application' && Number.isInteger(id) && id > 0) return { id, name: typeof row.name === 'string' ? row.name : undefined };
  }
  return undefined;
}

function questionMessage(text: string, questionId: string, userId: string | undefined, name: string | undefined): ChatMessage {
  return { id: questionId, questionId, role: 'user', name: name ?? '', content: text, createdAt: new Date().toISOString(), ...(userId !== undefined ? { userId } : {}) };
}

export function useCloudChat({ conversationId, workspace, active, mayChat }: CloudChatInput): CloudChat {
  const page = useChatPageData({ conversationId: active ? conversationId : undefined });
  const { activeConversation, projectId, user } = page;
  const queryClient = useQueryClient();
  const server = useMemo(() => convertConversationToChatHistory((activeConversation ?? {}) as unknown as ConversationDetails), [activeConversation]);
  const serverRef = useRef(server);
  serverRef.current = server;

  // The live turn: the server's messages plus what the stream adds, until the server has it all.
  const [live, setLive] = useState<readonly ChatMessage[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const setChatHistory = useCallback((update: (previous: readonly ChatMessage[]) => readonly ChatMessage[]): void => {
    setLive((previous) => update(previous ?? serverRef.current));
  }, []);
  const uuid = activeConversation?.uuid;
  const transport = useChatStreamTransport({
    setChatHistory,
    ...(uuid !== undefined ? { conversationUuid: uuid } : {}),
    onStreamError: (reason) => setError(reason),
  });
  const { isStreaming: streaming } = transport;

  // Not streaming: follow the server (a cloud turn started elsewhere, or this one's persisted result).
  useEffect(() => {
    if (!active || streaming) return undefined;
    const timer = window.setInterval(() => void queryClient.invalidateQueries({ queryKey: ['conversation'] }), SERVER_POLL_MS);
    return () => window.clearInterval(timer);
  }, [active, streaming, queryClient]);
  useEffect(() => {
    if (!streaming && live !== null && server.length >= live.length) setLive(null);
  }, [streaming, live, server]);

  const agent = agentParticipant(activeConversation?.participants);
  const canSend = mayChat && projectId !== undefined && uuid !== undefined && agent !== undefined;

  const send = useCallback(
    async (text: string): Promise<boolean> => {
      if (!canSend || projectId === undefined || uuid === undefined || agent === undefined || text.trim() === '' || streaming) return false;
      setError(null);
      const questionId = crypto.randomUUID();
      setLive([...serverRef.current, questionMessage(text, questionId, user?.id, user?.name)]);
      const outcome = await transport.startDetailed({
        projectId,
        conversationUuid: uuid,
        contract: conversationApi.contracts.application,
        body: {
          project_id: Number.isFinite(Number(projectId)) ? Number(projectId) : projectId,
          conversation_uuid: uuid,
          question_id: questionId,
          interaction_uuid: crypto.randomUUID(),
          mcp_tokens: await getExecutionTokens(projectId),
          participant_id: agent.id,
          payload: { user_input: text },
        },
      });
      if (outcome.started) return true;
      setLive(null);
      setError(outcome.reason === 'rejected' ? outcome.message : t('workspace.cloud.noTransport', 'The cloud copy could not be reached.'));
      return false;
    },
    [canSend, projectId, uuid, agent, streaming, user, transport],
  );

  // The activity island and the tray show a cloud turn while its stream runs.
  const startedAt = useRef<number | null>(null);
  const { id: workspaceId, name: workspaceName } = workspace;
  useEffect(() => {
    if (uuid === undefined) return undefined;
    if (!streaming) {
      startedAt.current = null;
      useActivityFeed.getState().setCloudRun(uuid, null);
      return undefined;
    }
    startedAt.current ??= Date.now();
    useActivityFeed.getState().setCloudRun(uuid, { conversationUuid: uuid, startedAt: startedAt.current, title: workspaceName, workspaceId, conversationId });
    return () => useActivityFeed.getState().setCloudRun(uuid, null);
  }, [streaming, uuid, workspaceId, workspaceName, conversationId]);

  return { history: live ?? server, loading: page.isLoadingConversation, streaming, canSend, error, send, stop: transport.stop, userId: user?.id, projectId };
}
