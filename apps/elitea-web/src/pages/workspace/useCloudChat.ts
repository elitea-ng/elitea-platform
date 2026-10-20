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

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { convertConversationToChatHistory, useChatStreamTransport, type ChatMessage, type ConversationDetails } from '@/features/chat-messages';
import { getExecutionTokens } from '@/features/mcps';
import { t } from '@/shared/i18n';
import { useActivityFeed } from '@/widgets/desktop-shell';

import { useChatPageData } from '@/pages/chat/useChatPageData';

import { cloudPrompt, continueBody, hitlBody, markDecided, type CloudPrompt, type HitlDecision } from './cloudPrompts';

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
  /** What the cloud copy waits on (an approval, the token-limit continue, or something only the chat answers); `null` when nothing. */
  prompt: CloudPrompt | null;
  /** Answer the pending approval card; resumes the turn. */
  answerHitl: (decision: HitlDecision) => Promise<void>;
  /** Continue a turn that stopped on the model's token limit. */
  continueTurn: (messageId: string) => Promise<void>;
  /** Hand the thread to the web chat (one click, for a prompt only it can answer). */
  openInChat: () => void;
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
  const history = live ?? server;
  const historyRef = useRef(history);
  historyRef.current = history;
  const prompt = useMemo(
    () =>
      mayChat && !streaming
        ? cloudPrompt(history, {
            approve: (tool) => t('desktop.island.approve', 'Approve: {{tool}}', { tool }),
            continuePrompt: t('workspace.cloud.continue', 'Continue?'),
            chat: t('workspace.cloud.answerInChat', 'Answer in chat'),
          })
        : null,
    [history, mayChat, streaming],
  );
  const navigate = useNavigate();
  const openInChat = useCallback((): void => {
    if (conversationId !== '') void navigate({ to: '/chat/$conversationId', params: { conversationId } });
  }, [navigate, conversationId]);

  /** Resume the paused message over the continue route; the stream folds in like any cloud turn. */
  const resume = useCallback(
    async (message: ChatMessage, contract: string, body: Record<string, unknown> | undefined, fail: () => void): Promise<void> => {
      if (projectId === undefined || uuid === undefined || body === undefined) {
        setError(t('workspace.cloud.cannotAnswer', 'This cannot be answered here. Answer it in the chat.'));
        fail();
        return;
      }
      setError(null);
      const outcome = await transport.resumeDetailed({ projectId, conversationUuid: uuid, contract, body });
      if (outcome.started) return;
      // Already answered elsewhere (another window, the web chat): nothing to resume, the server's transcript follows.
      if (outcome.reason === 'already-resolved') return;
      fail();
      setError(outcome.reason === 'rejected' ? outcome.message : t('workspace.cloud.noTransport', 'The cloud copy could not be reached.'));
      void message;
    },
    [projectId, uuid, transport],
  );

  const answerHitl = useCallback(
    async (decision: HitlDecision): Promise<void> => {
      const message = historyRef.current[historyRef.current.length - 1];
      if (message === undefined) return;
      const card = (message.hitlInterrupts as readonly { interrupt_id?: string; child_thread_id?: string }[] | undefined)?.find((c) => !decision.interruptId || c.interrupt_id === decision.interruptId);
      const patch = (next: boolean): void => setChatHistory((previous) => previous.map((item) => (item.id === message.id ? markDecided(item, next) : item)));
      patch(true);
      await resume(message, conversationApi.contracts.continueHitl, hitlBody({ projectId, conversationUuid: uuid, message }, decision, card), () => patch(false));
    },
    [projectId, uuid, resume, setChatHistory],
  );

  const continueTurn = useCallback(
    async (messageId: string): Promise<void> => {
      const message = historyRef.current.find((item) => item.id === messageId);
      if (message === undefined) return;
      const flags = (on: boolean): void => setChatHistory((previous) => previous.map((item) => (item.id === messageId ? { ...item, isLoading: on, isStreaming: on } : item)));
      flags(true);
      await resume(message, conversationApi.contracts.continueOutputLimit, continueBody({ projectId, conversationUuid: uuid, message }), () => flags(false));
    },
    [projectId, uuid, resume, setChatHistory],
  );

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

  // The activity island, the tray and the Dock show a cloud turn while its stream runs, and while it waits on the person.
  const startedAt = useRef<number | null>(null);
  const { id: workspaceId, name: workspaceName } = workspace;
  const waiting = prompt !== null;
  const waitingLabel = prompt?.label ?? '';
  useEffect(() => {
    if (uuid === undefined) return undefined;
    if (!streaming) startedAt.current = null;
    else startedAt.current ??= Date.now();
    if (!streaming && !waiting) {
      useActivityFeed.getState().setCloudRun(uuid, null);
      return undefined;
    }
    useActivityFeed.getState().setCloudRun(uuid, {
      conversationUuid: uuid,
      startedAt: startedAt.current ?? Date.now(),
      running: streaming,
      waiting: waiting ? waitingLabel : null,
      title: workspaceName,
      workspaceId,
      conversationId,
    });
    return () => useActivityFeed.getState().setCloudRun(uuid, null);
  }, [streaming, waiting, waitingLabel, uuid, workspaceId, workspaceName, conversationId]);

  return { history, loading: page.isLoadingConversation, streaming, canSend, error, send, stop: transport.stop, prompt, answerHitl, continueTurn, openInChat, userId: user?.id, projectId };
}
