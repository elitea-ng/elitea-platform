/**
 * The class of a conversation message (ADR-0050 decision 7e). Every client
 * renders a class its own way, but none decides the class itself: this is the
 * one rule, read from the typed fields of client contract 1.38
 * (`author_kind`, `addressed_to`, `visibility`, `cause` on a message, `kind`
 * on a participant).
 */

/** What a participant or a message author is (client contract 1.38). */
export const PARTICIPANT_KIND = {
  Human: 'human',
  Agent: 'agent',
  EphemeralAgent: 'ephemeral_agent',
  System: 'system',
} as const;

export type ParticipantKind = (typeof PARTICIPANT_KIND)[keyof typeof PARTICIPANT_KIND];

/** Why a message was written (client contract 1.38). */
export const MESSAGE_CAUSE = {
  HumanMessage: 'human_message',
  AgentMention: 'agent_mention',
  Heartbeat: 'heartbeat',
  Delegation: 'delegation',
  System: 'system',
} as const;

export type MessageCause = (typeof MESSAGE_CAUSE)[keyof typeof MESSAGE_CAUSE];

/** Who sees a message in the transcript: `side` is agent-to-agent talk, shown collapsed. */
export const MESSAGE_VISIBILITY = {
  All: 'all',
  Side: 'side',
} as const;

export type MessageVisibility = (typeof MESSAGE_VISIBILITY)[keyof typeof MESSAGE_VISIBILITY];

/** The rendering classes of decision 7e. */
export const MESSAGE_CLASS = {
  /** A person's message: right for the reader, left with a name for other people. */
  Human: 'human',
  /** An agent answering people: the agent's avatar, `display_name` and `role`. */
  AgentToHumans: 'agent_to_humans',
  /** Agent-to-agent talk: a compact indented row, collapsed under its cause. */
  AgentToAgent: 'agent_to_agent',
  /** A temporary sub-agent: a dashed avatar ring and a "temporary" badge. */
  Ephemeral: 'ephemeral',
  /** An agent's proactive message: a "woke up" marker. */
  Heartbeat: 'heartbeat',
  /** A platform line (a loop guard, a hand-off): centred and muted. */
  System: 'system',
} as const;

export type MessageClass = (typeof MESSAGE_CLASS)[keyof typeof MESSAGE_CLASS];

const KINDS: ReadonlySet<string> = new Set(Object.values(PARTICIPANT_KIND));

/** A wire value as a participant kind; `undefined` for an absent or unknown one. */
export function participantKindOf(value: unknown): ParticipantKind | undefined {
  return typeof value === 'string' && KINDS.has(value) ? (value as ParticipantKind) : undefined;
}

/** The fields of a message that decide its class, as the wire or a client model carries them. */
export interface ClassifiableMessage {
  /** The message's `author_kind`. Absent on a row written before contract 1.38. */
  readonly authorKind?: unknown;
  /**
   * The role the client already derived for a row without `author_kind`
   * (`user` for a person's message, anything else for an answer).
   */
  readonly fallbackRole?: string | undefined;
  /** Participant ids the message is addressed to; empty or absent is everyone. */
  readonly addressedTo?: readonly unknown[] | null | undefined;
  readonly visibility?: unknown;
  readonly cause?: unknown;
  /**
   * The kind of each addressee, when the client knows the roster: an agent
   * message to agents only is agent-to-agent talk even when `visibility` is
   * `all`.
   */
  readonly kindOfParticipant?: ((id: number) => ParticipantKind | undefined) | undefined;
}

function addressees(value: readonly unknown[] | null | undefined): number[] {
  if (!Array.isArray(value)) return [];
  return value.filter((id): id is number => typeof id === 'number' && Number.isSafeInteger(id) && id > 0);
}

/** Whether every addressee is an agent (an empty list is everyone, so it is not). */
function addressedToAgentsOnly(message: ClassifiableMessage): boolean {
  const ids = addressees(message.addressedTo);
  if (ids.length === 0 || message.kindOfParticipant === undefined) return false;
  return ids.every((id) => {
    const kind = message.kindOfParticipant?.(id);
    return kind === PARTICIPANT_KIND.Agent || kind === PARTICIPANT_KIND.EphemeralAgent;
  });
}

/**
 * The class of one message. A row without `author_kind` (written before
 * contract 1.38) is classed from the role the client derived: a person's
 * message or an answer to people, as it rendered before.
 */
export function classifyMessage(message: ClassifiableMessage): MessageClass {
  const kind = participantKindOf(message.authorKind);
  if (kind === undefined) {
    return message.fallbackRole === 'user' ? MESSAGE_CLASS.Human : MESSAGE_CLASS.AgentToHumans;
  }
  switch (kind) {
    case PARTICIPANT_KIND.Human:
      return MESSAGE_CLASS.Human;
    case PARTICIPANT_KIND.System:
      return MESSAGE_CLASS.System;
    case PARTICIPANT_KIND.EphemeralAgent:
      return MESSAGE_CLASS.Ephemeral;
    case PARTICIPANT_KIND.Agent:
      break;
  }
  if (message.visibility === MESSAGE_VISIBILITY.Side || addressedToAgentsOnly(message)) return MESSAGE_CLASS.AgentToAgent;
  if (message.cause === MESSAGE_CAUSE.Heartbeat) return MESSAGE_CLASS.Heartbeat;
  return MESSAGE_CLASS.AgentToHumans;
}
