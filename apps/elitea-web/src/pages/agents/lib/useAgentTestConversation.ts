/**
 * The agent editor's test conversation (#1974, #1980).
 *
 * It is created on the first interaction with the composer, not on mount, so
 * opening an agent to read it never mints a conversation row. It is filed as
 * an `editor_test` conversation: Main stamps it hidden, private and bound to
 * this agent version (`meta.editor_test`), which is what keeps it out of the
 * Chats sidebar (the conversation list leaves `is_hidden` rows out) while it
 * still belongs to the agent. The same contract the pipeline editor's test
 * chat uses (`pages/pipelines/lib/usePipelineTestConversation.ts`), minus its
 * durable-run history, which an agent has no use for.
 *
 * ONE conversation per user, project, agent and version. It is kept in a
 * module-scope map, so leaving the page and coming back restores it (with its
 * messages) instead of minting another hidden row; Clear deletes it, so the
 * rows do not pile up. A full reload forgets the map, and the next first use
 * then creates a new conversation.
 *
 * The participant is sent WITH the create call and is the only one: Main adds
 * the user itself and refuses an `editor_test` create that carries anything
 * but one saved-application participant.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { conversationApi } from '@/entities/conversation';
import { applicationParticipant, editorTestIdentityKey } from '@/shared/lib/editorTestConversation';
import type { ChatBoxProps } from '@/widgets/chat-box';

type ConversationWire = Awaited<ReturnType<typeof conversationApi.details>>;
type ActiveConversation = NonNullable<NonNullable<ChatBoxProps['conversation']>['active']>;
type MessageGroups = NonNullable<ActiveConversation['message_groups']>;

/** What identifies the agent version being tested. Every id is the decimal string the route and the version carry. */
export interface AgentTestIdentity {
  readonly projectId: string | undefined;
  readonly applicationId: string | undefined;
  readonly agentName: string | undefined;
  readonly versionId: string | undefined;
  readonly agentType: string | undefined;
  readonly userId: string | undefined;
}

export interface UseAgentTestConversationResult {
  readonly conversation: ActiveConversation | undefined;
  readonly activeParticipant: unknown;
  readonly isCreating: boolean;
  readonly hasFailed: boolean;
  /** The create call's own failure, so the pane can show Main's named refusal (FU-244 composition) rather than a generic line. */
  readonly failure: unknown;
  /** The test subject (user, project, agent, version); changes when the page moves to another one. */
  readonly scopeKey: string | undefined;
  /** Bumped by `clear` and by a change of `scopeKey`, so the pane remounts its `ChatBox` onto an empty transcript. */
  readonly generation: number;
  /** Idempotent: a no-op while the identity is incomplete (the next interaction retries), while a create is in flight and once a conversation exists. */
  readonly ensure: () => void;
  /** Starts over: forgets the conversation and deletes it (best effort). Creates nothing. */
  readonly clear: () => void;
}

interface State {
  readonly key: string;
  readonly conversation: ActiveConversation | undefined;
  readonly activeParticipant: unknown;
  readonly isCreating: boolean;
  readonly hasFailed: boolean;
  readonly failure?: unknown;
}

const EMPTY: State = { key: '', conversation: undefined, activeParticipant: undefined, isCreating: false, hasFailed: false };
const MAX_NAME = 50;
const LIVE_TEST_CONVERSATIONS = new Map<string, State>();

export function resetAgentTestConversationsForTests(): void {
  LIVE_TEST_CONVERSATIONS.clear();
}

function toState(detail: ConversationWire, key: string, groups: MessageGroups): State {
  const participants = [...(detail.participants ?? [])];
  return {
    ...EMPTY,
    key,
    activeParticipant: participants.find((row) => row.entity_name === 'application'),
    conversation: { id: detail.id, uuid: detail.uuid ?? '', name: detail.name, participants, message_groups: groups, meta: detail.meta ?? {} },
  };
}

function groupsOf(detail: ConversationWire): MessageGroups {
  const groups = detail['message_groups'];
  return Array.isArray(groups) ? (groups as MessageGroups) : [];
}

export function useAgentTestConversation(identity: AgentTestIdentity): UseAgentTestConversationResult {
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: deleteConversation } = conversationApi.useDelete();
  const [state, setState] = useState<State>(EMPTY);
  const [generation, setGeneration] = useState(0);
  const key = editorTestIdentityKey([identity.userId, identity.projectId, identity.applicationId, identity.versionId]);
  // The latest identity, read at click time so `ensure` keeps one stable identity itself.
  const latest = useRef({ identity, key });
  latest.current = { identity, key };
  const stateRef = useRef(state);
  stateRef.current = state;
  // True from the moment a create (or restore) starts until it fails, the scope changes or the chat is cleared.
  const pending = useRef(false);
  const ticket = useRef(0);
  const previousKey = useRef(key);

  const ensure = useCallback(() => {
    const { identity: input, key: expected } = latest.current;
    if (expected === undefined || pending.current) return;
    if (stateRef.current.key === expected && stateRef.current.conversation !== undefined) return;
    pending.current = true;
    const mine = ticket.current;
    const fail = (error: unknown): void => {
      if (mine !== ticket.current) return;
      LIVE_TEST_CONVERSATIONS.delete(expected);
      pending.current = false;
      setState({ ...EMPTY, key: expected, hasFailed: true, failure: error });
    };
    const settle = (next: State): void => {
      if (mine !== ticket.current) return;
      LIVE_TEST_CONVERSATIONS.set(expected, next);
      setState(next);
    };
    const live = LIVE_TEST_CONVERSATIONS.get(expected);
    setState({ ...(live ?? EMPTY), key: expected, isCreating: true });
    const liveId = live?.conversation?.id;
    if (liveId !== undefined) {
      // Back on the page: reload what the conversation holds rather than showing an empty transcript over it.
      void conversationApi
        .details({ projectId: input.projectId!, id: liveId, messages_limit: 50, sort_order: 'desc' })
        .then((detail) => settle(toState(detail, expected, groupsOf(detail))))
        .catch(fail);
      return;
    }
    void createConversation({
      projectId: input.projectId!,
      name: input.agentName?.slice(0, MAX_NAME) || 'Agent test',
      is_private: true,
      source: 'editor_test',
      meta: { is_hidden: true },
      participants: [
        applicationParticipant({
          applicationId: input.applicationId!,
          projectId: input.projectId!,
          name: input.agentName,
          versionId: input.versionId!,
          agentType: input.agentType ?? 'openai',
        }),
      ],
    })
      .then((detail) => settle(toState(detail, expected, [])))
      .catch(fail);
  }, [createConversation]);

  // A new agent, version or user is a different test subject: drop what belonged to the old one and remount the
  // transcript. When this subject already has a conversation (the page was left and re-entered) it is brought back
  // at once, with its messages; that reads, and creates nothing.
  useEffect(() => {
    ticket.current += 1;
    pending.current = false;
    setState(EMPTY);
    // Only a move AWAY from a real subject has a transcript to drop; the identity completing on first load has none.
    if (previousKey.current !== undefined && previousKey.current !== key) setGeneration((value) => value + 1);
    previousKey.current = key;
    if (key !== undefined && LIVE_TEST_CONVERSATIONS.has(key)) ensure();
  }, [key, ensure]);

  const clear = useCallback(() => {
    const { identity: input, key: current } = latest.current;
    const old = current === undefined ? undefined : LIVE_TEST_CONVERSATIONS.get(current);
    if (current !== undefined) LIVE_TEST_CONVERSATIONS.delete(current);
    const oldId = old?.conversation?.id;
    if (oldId !== undefined && input.projectId !== undefined) {
      // Best effort: the chat is already gone from the screen, and a row that stays behind is only hidden.
      void deleteConversation({ projectId: input.projectId, id: oldId }).catch(() => undefined);
    }
    ticket.current += 1;
    pending.current = false;
    setState(EMPTY);
    setGeneration((value) => value + 1);
  }, [deleteConversation]);

  const visible = state.key === key ? state : EMPTY;
  return { ...visible, failure: visible.failure, scopeKey: key, generation, ensure, clear };
}
