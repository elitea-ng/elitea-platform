/**
 * The agent editor's test conversation (#1974, #1980).
 *
 * It is created on the FIRST interaction with the pane, not on mount, so
 * opening an agent to read it never mints a conversation row. It is filed as
 * an `editor_test` conversation: Main stamps it hidden, private and bound to
 * this agent version (`meta.editor_test`), which is what keeps it out of the
 * Chats sidebar (the conversation list leaves `is_hidden` rows out) while it
 * still belongs to the agent. The same contract the pipeline editor's test
 * chat uses (`pages/pipelines/lib/usePipelineTestConversation.ts`), minus its
 * durable-run history, which an agent has no use for.
 *
 * The participant is sent WITH the create call and is the only one: Main adds
 * the user itself and refuses an `editor_test` create that carries anything
 * but one saved-application participant.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { conversationApi } from '@/entities/conversation';
import type { ChatBoxProps } from '@/widgets/chat-box';

type CreatedConversation = Awaited<ReturnType<typeof conversationApi.details>>;
type ActiveConversation = NonNullable<NonNullable<ChatBoxProps['conversation']>['active']>;

/** What identifies the agent version being tested. Every id is the decimal string the route and the version carry. */
export interface AgentTestIdentity {
  readonly projectId: string | undefined;
  readonly applicationId: string | undefined;
  readonly agentName: string | undefined;
  readonly versionId: string | undefined;
  readonly agentType: string | undefined;
}

export interface UseAgentTestConversationResult {
  readonly conversation: ActiveConversation | undefined;
  readonly activeParticipant: unknown;
  readonly isCreating: boolean;
  readonly hasFailed: boolean;
  /** The create call's own failure, so the pane can show Main's named refusal (FU-244 composition) rather than a generic line. */
  readonly failure: unknown;
  /** Bumped by `clear`, so the pane can remount its `ChatBox` onto an empty transcript. */
  readonly generation: number;
  /** Idempotent; a no-op until the identity is complete, so a click that lands before the version loads is retried by the next one. */
  readonly ensure: () => void;
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
const ID_PATTERN = /^[1-9][0-9]*$/;

/** `undefined` until every id is a positive integer, the shape Main's `editor_test` identity check accepts. */
function identityKey(identity: AgentTestIdentity): string | undefined {
  const ids = [identity.projectId, identity.applicationId, identity.versionId];
  return ids.every((id) => id !== undefined && ID_PATTERN.test(id)) ? ids.join(':') : undefined;
}

function toState(detail: CreatedConversation, key: string): State {
  const participants = [...(detail.participants ?? [])];
  return {
    ...EMPTY,
    key,
    activeParticipant: participants.find((row) => row.entity_name === 'application'),
    conversation: { id: detail.id, uuid: detail.uuid ?? '', name: detail.name, participants, message_groups: [], meta: detail.meta ?? {} },
  };
}

export function useAgentTestConversation(identity: AgentTestIdentity): UseAgentTestConversationResult {
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const [state, setState] = useState<State>(EMPTY);
  const [generation, setGeneration] = useState(0);
  const key = identityKey(identity);
  // The latest identity, read at click time so `ensure` keeps one stable identity itself.
  const latest = useRef({ identity, key });
  latest.current = { identity, key };
  const pending = useRef(false);
  const ticket = useRef(0);
  // A new agent or version is a different test subject: drop what belonged to the old one.
  useEffect(() => {
    ticket.current += 1;
    pending.current = false;
    setState(EMPTY);
  }, [key]);

  const ensure = useCallback(() => {
    const { identity: input, key: expected } = latest.current;
    if (expected === undefined || pending.current) return;
    pending.current = true;
    const mine = ticket.current;
    setState({ ...EMPTY, key: expected, isCreating: true });
    void createConversation({
      projectId: input.projectId!,
      name: input.agentName?.slice(0, MAX_NAME) || 'Agent test',
      is_private: true,
      source: 'editor_test',
      meta: { is_hidden: true },
      participants: [
        {
          entity_name: 'application',
          entity_meta: { id: input.applicationId, project_id: input.projectId, ...(input.agentName !== undefined ? { name: input.agentName } : {}) },
          entity_settings: { version_id: input.versionId, agent_type: input.agentType ?? 'openai', variables: [], icon_meta: {} },
        },
      ],
    })
      .then((detail) => {
        if (mine === ticket.current) setState(toState(detail, expected));
      })
      .catch((error: unknown) => {
        if (mine === ticket.current) setState({ ...EMPTY, key: expected, hasFailed: true, failure: error });
      })
      .finally(() => {
        if (mine === ticket.current) pending.current = false;
      });
  }, [createConversation]);

  const clear = useCallback(() => {
    ticket.current += 1;
    pending.current = false;
    setState(EMPTY);
    setGeneration((value) => value + 1);
  }, []);

  const visible = state.key === key ? state : EMPTY;
  return { ...visible, failure: visible.failure, generation, ensure, clear };
}
