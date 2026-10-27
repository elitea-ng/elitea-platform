/**
 * lib/chatStreamFanoutFrames.ts — the fan-out frames of contract
 * `fanout-interrupt-decisions-v1.md` §9 (v1.1).
 *
 * - `agent_interrupt_pending` raises ONE card, merged by `interrupt_id` and
 *   keyed by `interrupt_key`; a re-announced card keeps the user's local
 *   decided/hidden state, a new pause of the same child (new key) starts over.
 * - `agent_hitl_resolved` closes one card. It carries the action, never the
 *   value, and nothing here reads either: the card is dropped.
 * - `agent_fanout_member` is one member's lifecycle. A member renders exactly
 *   like a sub-agent instance (runtime design §4): one action owned by the
 *   member's own `parent_agent_path`, so the thinking view gives it its
 *   accordion and the approval buckets give its cards the same label.
 * - `agent_fanout_progress` appends each entry's text to the member with the
 *   same `call_id`, in frame order, losslessly; an unknown member is dropped.
 * - `fanout_parked_v1`, on the parked terminal frame, is RECONCILE-ONLY: it
 *   drops local ledger cards it does not list and never creates one.
 */
import { ToolActionStatus, TOOL_ACTION_TYPES } from '@/shared/lib/chat';

import {
  cardToInterrupt,
  FanoutFrameType,
  isLedgerInterrupt,
  readInterruptCard,
  readInterruptResolved,
  readMemberStatus,
  readParked,
  readProgress,
  type FanoutMemberStatus,
  type FanoutProgressEntry,
} from './fanoutFrames';
import { firstAnswerableInterrupt, type NormalizedHitlInterrupt } from './hitlInterrupts';
import { rememberResolvedKey } from './interruptLedger';
import { findTarget, nowIso, replaceAt, type ChatStreamContext } from './chatStreamShared';
import type { ToolAction } from './chatStreamToolAction';

import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ChatStreamFrame } from './chatStreamFrame';

/** The id of the action that renders one member: stable for the member's frozen child thread. */
export function fanoutMemberActionId(memberCallId: string): string {
  return `fanout-member:${memberCallId}`;
}

const MEMBER_STATUS_TO_ACTION: Readonly<Record<string, string>> = {
  start: ToolActionStatus.processing,
  paused: ToolActionStatus.processing,
  end: ToolActionStatus.complete,
  failed: ToolActionStatus.error,
  cancelled: ToolActionStatus.cancelled,
};

function cardsOf(message: ChatMessage): readonly NormalizedHitlInterrupt[] {
  return (message.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
}

/** The card list replaced, and the singular field re-pointed at the first card still owed an answer. */
function withCards(cards: readonly NormalizedHitlInterrupt[]): Partial<ChatMessage> {
  return { hitlInterrupts: cards, hitlInterrupt: firstAnswerableInterrupt(cards) };
}

/**
 * Upsert one ledger card by `interrupt_id`. The same key keeps the local
 * flags (the user already answered it); a different key under the same id is
 * a later pause of that child and replaces the old card outright.
 */
function mergeLedgerCard(
  existing: readonly NormalizedHitlInterrupt[],
  incoming: NormalizedHitlInterrupt,
): readonly NormalizedHitlInterrupt[] {
  const at = existing.findIndex((card) => card.interrupt_id === incoming.interrupt_id);
  if (at === -1) return [...existing, incoming];
  const local = existing[at] as NormalizedHitlInterrupt;
  const keep = local.interrupt_key === incoming.interrupt_key && (local.decided || local.queued || local.hidden);
  const next = [...existing];
  next[at] = keep ? { ...incoming, decided: Boolean(local.decided), queued: Boolean(local.queued), hidden: true } : incoming;
  return next;
}

/** Drop the ledger card a resolution names; other cards are untouched. */
function dropLedgerCard(
  existing: readonly NormalizedHitlInterrupt[],
  interruptKey: string,
): readonly NormalizedHitlInterrupt[] {
  const next = existing.filter((card) => !(isLedgerInterrupt(card) && card.interrupt_key === interruptKey));
  return next.length === existing.length ? existing : next;
}

/** Keep only the ledger cards in `pending`; never adds one. Non-ledger cards are untouched. */
function reconcileLedgerCards(
  existing: readonly NormalizedHitlInterrupt[],
  pending: ReadonlySet<string>,
): readonly NormalizedHitlInterrupt[] {
  const next = existing.filter((card) => !isLedgerInterrupt(card) || pending.has(card.interrupt_key ?? ''));
  return next.length === existing.length ? existing : next;
}

function memberAction(status: FanoutMemberStatus, previous: ToolAction | undefined, at: string): ToolAction {
  const hierarchy = {
    parent_agent_name: status.parent_agent_name,
    parent_agent_call_id: status.parent_agent_call_id,
    parent_agent_path: status.parent_agent_path,
  };
  return {
    ...previous,
    ...hierarchy,
    id: fanoutMemberActionId(status.memberCallId),
    type: TOOL_ACTION_TYPES.Llm,
    name: status.memberName,
    content: typeof previous?.['content'] === 'string' ? previous['content'] : '',
    status: MEMBER_STATUS_TO_ACTION[status.status] ?? ToolActionStatus.processing,
    created_at: previous?.['created_at'] ?? at,
    timestamp: at,
    toolMeta: { ...previous?.toolMeta, ...hierarchy, fanout_v1: status.fanout_v1 },
  };
}

function upsertMember(actions: readonly ToolAction[], status: FanoutMemberStatus, at: string): readonly ToolAction[] {
  const id = fanoutMemberActionId(status.memberCallId);
  const index = actions.findIndex((action) => action.id === id);
  if (index === -1) return [...actions, memberAction(status, undefined, at)];
  const next = [...actions];
  next[index] = memberAction(status, actions[index], at);
  return next;
}

/** Append each entry to its member, in frame order. Entries for an unknown member are dropped. */
function appendFanoutProgress(
  actions: readonly ToolAction[],
  entries: readonly FanoutProgressEntry[],
): readonly ToolAction[] {
  let next = actions;
  for (const entry of entries) {
    const id = fanoutMemberActionId(entry.call_id);
    const index = next.findIndex((action) => action.id === id);
    if (index === -1) {
      // Contract §9.1: dropped and logged by ordinal only (the text is model output).
      console.debug(`fan-out progress for an unknown member, ordinal ${String(entry.ordinal)}`);
      continue;
    }
    const action = next[index] as ToolAction;
    const content = typeof action['content'] === 'string' ? action['content'] : '';
    next = next === actions ? [...actions] : next;
    (next as ToolAction[])[index] = { ...action, content: content + entry.text };
  }
  return next;
}

/**
 * Reduce one fan-out frame, or return `undefined` for a frame this family
 * does not own.
 */
export function reduceFanoutFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  context: ChatStreamContext,
  index: number,
): readonly ChatMessage[] | undefined {
  const metadata = frame.response_metadata;
  const current = history[index];
  switch (type) {
    case FanoutFrameType.InterruptPending: {
      const card = readInterruptCard(metadata?.['interrupt_card_v1']);
      if (!current || !card) return history;
      // A restore re-announces open cards before it applies its decisions.
      if (current.resolvedLedgerKeys?.includes(card.interrupt_key)) return history;
      return replaceAt(history, index, { ...withCards(mergeLedgerCard(cardsOf(current), cardToInterrupt(card))), isLoading: false });
    }
    case FanoutFrameType.HitlResolved: {
      const resolved = readInterruptResolved(metadata?.['interrupt_resolved_v1']);
      if (!current || !resolved) return history;
      const cards = cardsOf(current);
      const next = dropLedgerCard(cards, resolved.interrupt_key);
      const resolvedLedgerKeys = rememberResolvedKey(current.resolvedLedgerKeys, resolved.interrupt_key);
      return replaceAt(history, index, next === cards ? { resolvedLedgerKeys } : { ...withCards(next), resolvedLedgerKeys });
    }
    case FanoutFrameType.FanoutMember: {
      const status = readMemberStatus(metadata?.['fanout_member_status_v1']);
      if (!current || !status) return history;
      const actions = (current.toolActions ?? []) as readonly ToolAction[];
      return replaceAt(history, index, { toolActions: upsertMember(actions, status, nowIso(context)), isLoading: false });
    }
    case FanoutFrameType.FanoutProgress: {
      const entries = readProgress(metadata?.['fanout_progress_v1']);
      if (!current || !entries) return history;
      const actions = (current.toolActions ?? []) as readonly ToolAction[];
      const next = appendFanoutProgress(actions, entries);
      return next === actions ? history : replaceAt(history, index, { toolActions: next, isLoading: false });
    }
    default:
      return undefined;
  }
}

/** The pending set of a parked terminal frame, when it carries one. */
function parkedPendingOf(frame: ChatStreamFrame): ReadonlySet<string> | undefined {
  return readParked(frame.response_metadata?.['fanout_parked_v1']);
}

/**
 * The parked terminal frame (§8): reconcile the ledger cards against its
 * pending list and mark the run parked, which is not streaming. It never
 * creates a card.
 */
export function applyFanoutParkedFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
): readonly ChatMessage[] {
  const pending = parkedPendingOf(frame);
  if (!pending) return history;
  const index = findTarget(history, frame);
  const current = history[index];
  if (!current) return history;
  return replaceAt(history, index, {
    ...withCards(reconcileLedgerCards(cardsOf(current), pending)),
    fanoutParked: true,
    isStreaming: false,
    isLoading: false,
    isRegenerating: false,
  });
}
