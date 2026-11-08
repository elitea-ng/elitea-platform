/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props
 * budgets (§3.5) — "@" mention -> send-to-user/everyone routing, "@"/"#"
 * agent addressing (ADR-0050 7c, issue 1612), and skill ("~") mention
 * selection.
 */
import { useCallback, useMemo } from 'react';
import type { RefObject } from 'react';

import type { AddressableAgent } from '@elitea/client-core/conversation';
import type { ChatInputHandle } from '@/features/chat-input';

import { agentOfCandidate, useChatBoxAgentMentions } from './useChatBoxAgentMentions';
import type { SendQuestionParams, SendResult } from './useChatBoxHandlers.helpers';
import type { useChatBoxState, ResolvedUserMention } from './useChatBoxState';

export interface UseChatBoxMentionsParams {
  readonly state: ReturnType<typeof useChatBoxState>;
  readonly onChangeParticipant: ((participant: unknown) => void) | undefined;
  /** The conversation's participants (raw rows): its agents can be addressed. */
  readonly participants?: readonly unknown[] | undefined;
  readonly chatInput?: RefObject<ChatInputHandle | null> | undefined;
  /** The send the queue delivers through; `queuedSend` adds each message's own `addressedTo`. */
  readonly sendQuestion?: QueuedSend | undefined;
}

type QueuedSend = (params: SendQuestionParams) => Promise<SendResult>;
const NO_SEND: QueuedSend = () => Promise.resolve({ success: false });

export interface UseChatBoxMentionsResult {
  readonly handleMentionChange: (mentions: readonly { readonly user: unknown; readonly isValid: boolean }[]) => void;
  readonly handleSelectUserMention: (user: unknown) => void;
  readonly handleSelectSkillTool: (toolName: string | null) => void;
  /** The conversation's agents, when there are two or more (empty otherwise). */
  readonly addressableAgents: readonly AddressableAgent[];
  /** Inserts a "#"-picked conversation agent as `#Name` and records it. */
  readonly handleSelectHashAgent: (agent: AddressableAgent) => void;
  /** The "/" and "~" highlights plus the addressed agents' mentions. */
  readonly highlightRanges: readonly { start: number; end: number }[];
  readonly onInputChange: (value: string) => void;
  /** The `addressed_to` of `question`; clears the picks. */
  readonly takeAddressedTo: (question: string) => number[];
  /** Puts `@Name` at the start of the composer text and records the agent ("To:" with a lead). */
  readonly handleAddressAgent: (agent: AddressableAgent) => void;
  /**
   * `sendQuestion` with each message's `addressedTo` read from its own text:
   * the queue delivers a message after the turn ends, when the composer may
   * hold the next draft.
   */
  readonly queuedSend: QueuedSend;
}

const NO_CHAT_INPUT: RefObject<ChatInputHandle | null> = { current: null };

export function useChatBoxMentions({ state, onChangeParticipant, participants, chatInput = NO_CHAT_INPUT, sendQuestion = NO_SEND }: UseChatBoxMentionsParams): UseChatBoxMentionsResult {
  const agentMentions = useChatBoxAgentMentions(participants, chatInput);
  const { takeAddressedTo } = agentMentions;
  const queuedSend = useCallback<QueuedSend>((params) => sendQuestion({ ...params, addressedTo: takeAddressedTo(params.question) }), [sendQuestion, takeAddressedTo]);
  const handleMentionChange = useCallback(
    (mentions: readonly { readonly user: unknown; readonly isValid: boolean }[]) => {
      const mentionedEveryone = mentions.find((m) => (m.user as ResolvedUserMention | undefined)?.id === '@everyone');
      if (mentionedEveryone) {
        state.setIsMentioningEveryone(true);
        state.setSelectedUsers([]);
        onChangeParticipant?.(undefined);
        return;
      }
      const mentionedUsers = mentions.filter((m) => m.isValid && m.user);
      state.setIsMentioningEveryone(false);
      state.setSelectedUsers(mentionedUsers.map((m) => m.user as ResolvedUserMention));
      if (mentionedUsers.length > 0) onChangeParticipant?.(undefined);
    },
    [state, onChangeParticipant],
  );

  const { insert, onInputChange: onAgentInputChange } = agentMentions;
  const handleSelectUserMention = useCallback(
    (user: unknown) => {
      const agent = agentOfCandidate(user);
      if (agent === undefined) { state.onSelectUserMention(user as ResolvedUserMention); return; }
      insert(agent, '@', state.keyDown.atAnchorRef.current, state.keyDown.atQuery);
      state.keyDown.stopProcessingAtSymbol();
    },
    [state, insert],
  );

  const handleSelectHashAgent = useCallback(
    (agent: AddressableAgent) => {
      insert(agent, '#', state.keyDown.hashAnchorRef.current, state.keyDown.query);
      state.keyDown.stopProcessingSymbols();
    },
    [state, insert],
  );

  const handleAddressAgent = useCallback((agent: AddressableAgent) => { insert(agent, '@', null, ''); }, [insert]);

  const handleSelectSkillTool = useCallback(
    (toolName: string | null) => {
      if (toolName === null) { state.skill.resetSkill(); return; }
      const item = state.skill.filteredItems.find((i) => i.name === toolName);
      if (item) state.skill.onSelectSkill(item);
    },
    [state.skill],
  );

  const stateInputChange = state.onInputChange;
  const onInputChange = useCallback((value: string) => { stateInputChange(value); onAgentInputChange(value); }, [stateInputChange, onAgentInputChange]);
  const highlightRanges = useMemo(
    () => (agentMentions.highlightRanges.length === 0
      ? state.combinedHighlightRanges
      : [...state.combinedHighlightRanges, ...agentMentions.highlightRanges].sort((a, b) => a.start - b.start)),
    [state.combinedHighlightRanges, agentMentions.highlightRanges],
  );

  return {
    handleMentionChange,
    handleSelectUserMention,
    handleSelectSkillTool,
    addressableAgents: agentMentions.enabled ? agentMentions.agents : [],
    handleSelectHashAgent,
    highlightRanges,
    onInputChange,
    takeAddressedTo,
    handleAddressAgent,
    queuedSend,
  };
}

