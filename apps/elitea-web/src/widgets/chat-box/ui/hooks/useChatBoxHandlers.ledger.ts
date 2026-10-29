/**
 * hooks/useChatBoxHandlers.ledger.ts — one per-interrupt card decided through
 * the public decision API (contract `fanout-interrupt-decisions-v1.md` §6,
 * §9.3), replacing queue-then-submit for every ledger card.
 *
 * Each card is decided on its own, whatever its kind: no complete set, no
 * grouping by kind, no continuation and no socket. The card goes off screen
 * while the POST is in flight and stays off once Main recorded it; the
 * resolution (`agent_hitl_resolved`, on the stream or the project plane)
 * then drops it. A 409 means another tab or session answered it: the card
 * is dropped quietly and the response is reconciled. Any other refusal puts
 * the card back with a readable reason. `request_id` is reused for a retry
 * of the same body, so a lost answer cannot record a second decision.
 */
import { interruptLedger } from '@/features/chat-messages';
import type { ChatMessage } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { ChatBoxHandlerDeps, HitlInterruptAction } from './useChatBoxHandlers.helpers';
import { cardFlags, cardKey, cardsOf, patchMessageCards, type HitlCard } from './useChatBoxHandlers.hitlCards';

/** Page-life memory of the request id each card's body was sent under. Bounded: one entry per card key. */
const MAX_REMEMBERED_REQUESTS = 64;
const requestIds = new Map<string, { readonly body: string; readonly requestId: string }>();

function requestIdFor(slot: string, bodyText: string): string {
  const known = requestIds.get(slot);
  if (known?.body === bodyText) return known.requestId;
  const requestId = interruptLedger.newRequestId();
  requestIds.delete(slot);
  requestIds.set(slot, { body: bodyText, requestId });
  while (requestIds.size > MAX_REMEMBERED_REQUESTS) {
    const oldest = requestIds.keys().next().value;
    if (oldest === undefined) break;
    requestIds.delete(oldest);
  }
  return requestId;
}

function refusalText(reason: string): string {
  switch (reason) {
    case 'forbidden':
      return t('widgets.chatBox.ledger.forbidden', 'You cannot answer this request.');
    case 'not_found':
      return t('widgets.chatBox.ledger.notFound', 'This request is no longer waiting for an answer. Reload the conversation to see its current state.');
    case 'invalid':
      return t('widgets.chatBox.ledger.invalid', 'This answer could not be accepted. Check it and try again.');
    default:
      return t('widgets.chatBox.hitlDecisionRefused', 'The decision could not be sent. Try again.');
  }
}

function unsendableText(action: string): string {
  if (action === 'authorize') {
    return t('widgets.chatBox.ledger.authorizeUnavailable', 'Sign-in cannot be completed from this card yet. Skip it to let the run continue without it.');
  }
  return t('widgets.chatBox.ledger.invalid', 'This answer could not be accepted. Check it and try again.');
}

/** Is this card answered through the ledger rather than a continuation? */
export function isLedgerCard(card: HitlCard | undefined): boolean {
  return interruptLedger.isLedgerCard(card);
}

/** Decide ONE ledger card. Siblings, the message's streaming state and every other card are untouched. */
export async function decideLedgerCard(
  deps: ChatBoxHandlerDeps,
  message: ChatMessage,
  card: HitlCard,
  action: HitlInterruptAction,
): Promise<void> {
  const cards = cardsOf(message);
  const keys = new Set([cardKey(card, cards)]);
  const interruptKey = typeof card['interrupt_key'] === 'string' ? card['interrupt_key'] : '';
  const revision = typeof card['ledger_revision'] === 'number' ? card['ledger_revision'] : 1;
  const putBack = (exception: string): void =>
    patchMessageCards(deps.setChatHistory, message.id, keys, cardFlags.restored, { exception });
  const value = action.action === 'continue' && !action.value
    ? t('widgets.chatBox.staticPause.defaultMessage', 'Continue')
    : action.value;
  // The same card, action, value and revision reuse their request id: a
  // retry after a lost answer replays (`replay: true`) instead of conflicting.
  const slot = `${message.id}:${interruptKey}`;
  const requestId = requestIdFor(slot, JSON.stringify([action.action, value ?? '', revision]));
  const body = interruptLedger.buildRequest({ requestId, expectedRevision: revision, action: action.action, value });
  if (!body) {
    putBack(unsendableText(action.action));
    return;
  }
  patchMessageCards(deps.setChatHistory, message.id, keys, cardFlags.queued, { exception: undefined });
  const outcome = await interruptLedger.decide(deps.projectId, message.id, interruptKey, body);
  // Only a refusal may be retried with the same body; nothing else needs the id.
  if (outcome.kind !== 'refused') requestIds.delete(slot);
  if (outcome.kind === 'decided') {
    patchMessageCards(deps.setChatHistory, message.id, keys, cardFlags.decided);
    return;
  }
  if (outcome.kind === 'consumed') {
    deps.setChatHistory((previous) => interruptLedger.dropCard(previous, message.id, interruptKey));
    await interruptLedger.reconcile(deps.projectId, message.id, deps.setChatHistory);
    return;
  }
  putBack(refusalText(outcome.reason));
}
