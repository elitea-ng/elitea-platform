/**
 * `@Agent` / `#Agent` in the composer (ADR-0050 decision 7c, issue 1612).
 *
 * In a conversation with two or more agents, the "@" list and the "#" picker
 * also offer the conversation's agents. A pick inserts the mention as a
 * highlighted token; the send carries `addressed_to = addressedTo(text,
 * agents)` (`@elitea/client-core`), read from the message's own text, so a
 * mention the person deleted again addresses no one and a queued message
 * keeps its own addressing.
 */
import { useCallback, useMemo, useRef, useState } from 'react';
import type { RefObject } from 'react';

import { addressedTo, type AddressableAgent } from '@elitea/client-core/conversation';
import type { ChatInputHandle } from '@/features/chat-input';

import { triggerQueryEnd } from './triggerQueryRange';
import { positiveParticipantId } from './useChatBoxSend.floor';

/** The prefix that marks an agent row in the "@" list, which is keyed by participant id. */
export const AGENT_CANDIDATE_PREFIX = 'agent:';

/** An agent row of the "@" list (`UserMentionCandidate`'s shape). */
export interface AgentMentionCandidate {
  readonly id: string;
  readonly name: string;
  readonly agent: AddressableAgent;
}

interface AgentWire {
  readonly id?: unknown;
  readonly entity_name?: unknown;
  readonly entityName?: unknown;
  readonly entity_meta?: { readonly name?: unknown } | null;
  readonly entityMeta?: { readonly name?: unknown } | null;
  readonly meta?: { readonly name?: unknown } | null;
  readonly persona?: { readonly display_name?: unknown } | null;
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function participantId(value: unknown): number {
  return positiveParticipantId(value) ?? 0;
}

/** The name an agent participant shows and a mention spells: its persona's name, else the agent's. */
export function agentDisplayName(participant: unknown): string {
  const row = (participant ?? {}) as AgentWire;
  return text(row.persona?.display_name) || text(row.entity_meta?.name) || text(row.entityMeta?.name) || text(row.meta?.name);
}

/** The conversation's agents (application participants, raw or normalised rows), in roster order. */
export function conversationAgents(participants: readonly unknown[] | undefined): AddressableAgent[] {
  const out: AddressableAgent[] = [];
  for (const participant of participants ?? []) {
    const row = (participant ?? {}) as AgentWire;
    if ((row.entity_name ?? row.entityName) !== 'application') continue;
    const id = participantId(row.id);
    const name = agentDisplayName(row);
    if (id > 0 && name !== '' && !out.some((agent) => agent.participantId === id)) out.push({ participantId: id, name });
  }
  return out;
}

/** Whether the conversation has enough agents for addressing to mean anything. */
export function isMultiAgent(agents: readonly AddressableAgent[]): boolean {
  return agents.length >= 2;
}

/** The "@" rows for the agents. */
export function agentCandidates(agents: readonly AddressableAgent[]): AgentMentionCandidate[] {
  return agents.map((agent) => ({ id: `${AGENT_CANDIDATE_PREFIX}${agent.participantId}`, name: agent.name, agent }));
}

/** The agent behind an "@" row, or `undefined` for a person. */
export function agentOfCandidate(candidate: unknown): AddressableAgent | undefined {
  const row = candidate as Partial<AgentMentionCandidate> | null | undefined;
  return typeof row?.id === 'string' && row.id.startsWith(AGENT_CANDIDATE_PREFIX) ? row.agent : undefined;
}

const WORD_CHAR = /[\p{L}\p{N}_]/u;

/** The highlight ranges of the chosen agents' mentions in `content`. */
export function agentMentionRanges(content: string, chosen: readonly AddressableAgent[]): { start: number; end: number }[] {
  const ranges: { start: number; end: number }[] = [];
  for (const agent of chosen) {
    for (const sigil of ['@', '#']) {
      const token = sigil + agent.name;
      for (let at = content.indexOf(token); at >= 0; at = content.indexOf(token, at + 1)) {
        const before = at === 0 ? '' : (content[at - 1] ?? '');
        const after = content[at + token.length] ?? '';
        if (!WORD_CHAR.test(before) && !WORD_CHAR.test(after)) ranges.push({ start: at, end: at + token.length });
      }
    }
  }
  return ranges.sort((a, b) => a.start - b.start);
}

export interface UseChatBoxAgentMentionsResult {
  /** The conversation's agents. */
  readonly agents: readonly AddressableAgent[];
  /** True with two or more agents: only then does the composer offer them. */
  readonly enabled: boolean;
  /** The agents picked since the last send, in pick order. */
  readonly chosen: readonly AddressableAgent[];
  /** Highlight ranges of the chosen mentions in the current text. */
  readonly highlightRanges: readonly { start: number; end: number }[];
  /** Mirrors the composer text (for the highlight). */
  readonly onInputChange: (value: string) => void;
  /**
   * Replaces the typed trigger query at `anchor` (null: the start of the
   * text) with the agent's mention and records the agent.
   */
  readonly insert: (agent: AddressableAgent, sigil: '@' | '#', anchor: number | null, countedQuery: string) => void;
  /** The `addressed_to` of `question`: the conversation's agents its text mentions. */
  readonly takeAddressedTo: (question: string) => number[];
}

export function useChatBoxAgentMentions(participants: readonly unknown[] | undefined, chatInput: RefObject<ChatInputHandle | null>): UseChatBoxAgentMentionsResult {
  const agents = useMemo(() => conversationAgents(participants), [participants]);
  const [chosen, setChosen] = useState<readonly AddressableAgent[]>([]);
  const [content, setContent] = useState('');

  const insert = useCallback((agent: AddressableAgent, sigil: '@' | '#', anchor: number | null, countedQuery: string) => {
    const input = chatInput.current;
    if (input) {
      const start = anchor ?? 0;
      const end = anchor === null ? 0 : triggerQueryEnd(input.getInputContent(), anchor, sigil, countedQuery);
      input.replaceRange(start, end, `${sigil}${agent.name} `);
      setContent(input.getInputContent());
    }
    setChosen((prev) => (prev.some((a) => a.participantId === agent.participantId) ? prev : [...prev, agent]));
  }, [chatInput]);

  // Read from the message's own text against the conversation's agents, so a
  // message that waited in the queue keeps its addressing and the draft being
  // typed keeps its picks. The picks only drive the highlight.
  const agentsRef = useRef(agents);
  agentsRef.current = agents;
  const takeAddressedTo = useCallback((question: string) => (agentsRef.current.length >= 2 ? addressedTo(question, agentsRef.current) : []), []);

  const highlightRanges = useMemo(() => agentMentionRanges(content, chosen), [content, chosen]);
  return { agents, enabled: isMultiAgent(agents), chosen, highlightRanges, onInputChange: setContent, insert, takeAddressedTo };
}
