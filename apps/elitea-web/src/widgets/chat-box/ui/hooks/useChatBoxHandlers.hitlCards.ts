/**
 * hooks/useChatBoxHandlers.hitlCards.ts — the pause cards of one response, as
 * the decision path reads and patches them.
 *
 * Its own file for the §3.5 file-length budget: `useChatBoxHandlers.hitl.ts`
 * owns the continuation body and `decideHitlCard`, this file owns the card
 * list, its identities, its local flags, and the reload restore.
 */
import type { ChatMessage } from '@/features/chat-messages';

import { forgetHitlDecisions, retainHitlDecisions, type HitlQueueScope } from '../../model/hitlDecisionQueue';

/** The fields of a `NormalizedHitlInterrupt` the decision path reads. */
export interface HitlCard {
  readonly interrupt_id?: string;
  readonly tool_call_id?: string;
  readonly thread_id?: string;
  readonly child_thread_id?: string;
  readonly resume_strategy?: string;
  readonly decided?: boolean;
  readonly queued?: boolean;
  readonly hidden?: boolean;
  readonly [key: string]: unknown;
}

function nonEmpty(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : '';
}

/**
 * `getInterruptIdentity` (`features/chat-messages/lib/hitlInterrupts.ts`),
 * restated because the slice barrel does not export it; the queue test pins
 * the two to the same answers.
 */
export function hitlCardIdentity(card: HitlCard | undefined): string {
  const interruptId = nonEmpty(card?.interrupt_id);
  if (interruptId) return interruptId;
  const threadId = nonEmpty(card?.child_thread_id) || nonEmpty(card?.thread_id);
  const toolCallId = nonEmpty(card?.tool_call_id);
  if (!threadId && !toolCallId) return '';
  return JSON.stringify([threadId, toolCallId]);
}

/** Every card on the message, in message order; the singular field stands in for an unset list. */
export function cardsOf(message: ChatMessage): readonly HitlCard[] {
  if (Array.isArray(message.hitlInterrupts) && message.hitlInterrupts.length > 0) {
    return message.hitlInterrupts as readonly HitlCard[];
  }
  return message.hitlInterrupt ? [message.hitlInterrupt as HitlCard] : [];
}

/** The key a card's decision is queued under: its identity, or its position when it states none. */
export function cardKey(card: HitlCard, cards: readonly HitlCard[]): string {
  return hitlCardIdentity(card) || `#${cards.indexOf(card)}`;
}

/** The cards of the CURRENT pending set: not yet submitted. */
export function pendingCards(message: ChatMessage): readonly HitlCard[] {
  return cardsOf(message).filter((card) => !card.decided);
}

function isAnswerable(card: HitlCard): boolean {
  return !card.decided && !card.queued && !card.hidden;
}

/** A card with its local flags removed — answerable again, and equal to what the stream delivered. */
function withoutFlags(card: HitlCard): HitlCard {
  const { decided: _decided, queued: _queued, hidden: _hidden, ...rest } = card;
  return rest;
}

/** The flag patches the decision path applies. */
export const cardFlags = {
  queued: (card: HitlCard): HitlCard => ({ ...card, queued: true, hidden: true }),
  decided: (card: HitlCard): HitlCard => ({ ...card, decided: true, queued: false, hidden: true }),
  restored: withoutFlags,
} as const;

/**
 * The message with the cards whose key is in `keys` patched, and the singular
 * field re-pointed at the first card still owed an answer.
 */
function patchCards(
  message: ChatMessage,
  keys: ReadonlySet<string>,
  patch: (card: HitlCard) => HitlCard,
  fields: Partial<ChatMessage> = {},
): ChatMessage {
  const cards = cardsOf(message);
  const next = cards.map((card) => (keys.has(cardKey(card, cards)) ? patch(card) : card));
  return {
    ...message,
    ...fields,
    hitlInterrupts: next,
    hitlInterrupt: next.find(isAnswerable),
  };
}

/** `patchCards` applied through `setChatHistory` to the message with `messageId`. */
export function patchMessageCards(
  setChatHistory: (update: (previous: readonly ChatMessage[]) => readonly ChatMessage[]) => void,
  messageId: string,
  keys: ReadonlySet<string>,
  patch: (card: HitlCard) => HitlCard,
  fields: Partial<ChatMessage> = {},
): void {
  setChatHistory((previous) =>
    previous.map((message) => (message.id === messageId ? patchCards(message, keys, patch, fields) : message)),
  );
}

/**
 * Re-apply decisions persisted before a reload to a freshly hydrated history.
 *
 * A card whose decision survived comes back queued and hidden; a decision for a
 * card the server no longer holds pending is dropped from storage. When the
 * restore would leave a response with nothing to answer — every pending card
 * already queued — the queue is dropped instead, so the cards come back
 * answerable rather than stuck waiting for a click that cannot happen.
 */
export function restoreQueuedHitlDecisions(
  history: readonly ChatMessage[],
  conversation: string | undefined,
): readonly ChatMessage[] {
  if (!conversation) return history;
  let changed = false;
  const restored = history.map((message) => {
    const pending = pendingCards(message);
    if (pending.length < 2) return message;
    const cards = cardsOf(message);
    const scope: HitlQueueScope = { conversation, message: message.id, kind: 'hitl' };
    const keys = new Set(pending.map((card) => cardKey(card, cards)));
    const kept = new Set(retainHitlDecisions(scope, keys).map((decision) => decision.interruptId));
    if (kept.size === 0) return message;
    if (kept.size >= keys.size) {
      forgetHitlDecisions(scope);
      return message;
    }
    changed = true;
    return patchCards(message, kept, cardFlags.queued);
  });
  return changed ? restored : history;
}
