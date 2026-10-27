/**
 * lib/interruptLedger.ts — the per-interrupt decision API's client-side rules
 * (contract `fanout-interrupt-decisions-v1.md` §6), pure.
 *
 * - `buildDecisionRequest` shapes one card's POST body
 *   (`fanout-interrupt-decision-request`): valueless actions send `""`, valued
 *   ones send their non-empty text, `credential_ref` only for `authorize`.
 * - `applyInterruptList` folds a `GET …/interrupts` answer into one message:
 *   PENDING cards are answerable, DECIDED ones stay hidden, a ledger card the
 *   list does not hold is dropped. Non-ledger (continuation) cards are never
 *   touched, and the list never creates a coordinator card out of thin air:
 *   every card it lists is a card Main holds open for this response.
 */
import { ExecutionInterruptDecisionRequest } from '@/shared/api/generated/model/executionInterruptDecisionRequest.zod';

import { cardToInterrupt, isLedgerInterrupt, readInterruptCard } from './fanoutFrames';
import { firstAnswerableInterrupt, type NormalizedHitlInterrupt } from './hitlInterrupts';
import type { ChatMessage } from './convertMessagesToChatHistory';

/** §6: these four actions carry an empty value. */
const VALUELESS_ACTIONS: ReadonlySet<string> = new Set(['approve', 'reject', 'authorize', 'skip']);

export interface LedgerDecisionInput {
  readonly requestId: string;
  readonly expectedRevision: number;
  readonly action: string;
  readonly value?: string | undefined;
  readonly credentialRef?: string | undefined;
}

export type LedgerDecisionBody = ReturnType<typeof ExecutionInterruptDecisionRequest.parse>;

/** The POST body, or `undefined` when no body the route admits fits (checked against the generated schema). */
export function buildDecisionRequest(input: LedgerDecisionInput): LedgerDecisionBody | undefined {
  const value = VALUELESS_ACTIONS.has(input.action) ? '' : (input.value ?? '');
  if (!VALUELESS_ACTIONS.has(input.action) && value === '') return undefined;
  const body = {
    request_id: input.requestId,
    expected_revision: input.expectedRevision,
    action: input.action,
    value,
    ...(input.action === 'authorize' && input.credentialRef ? { credential_ref: input.credentialRef } : {}),
  };
  if (input.action === 'authorize' && !input.credentialRef) return undefined;
  const parsed = ExecutionInterruptDecisionRequest.safeParse(body);
  return parsed.success ? parsed.data : undefined;
}

/** A fresh idempotency key: 64 lowercase hex, never all zeros (§2 grammar). */
export function newDecisionRequestId(random: (bytes: Uint8Array<ArrayBuffer>) => Uint8Array = (bytes) => crypto.getRandomValues(bytes)): string {
  for (;;) {
    const bytes = random(new Uint8Array(32));
    if (bytes.some((byte) => byte !== 0)) return [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('');
  }
}

type Raw = Readonly<Record<string, unknown>>;

/** The cards of a `fanout-interrupt-list` body, or `undefined` when it is not one. */
export function readInterruptList(body: unknown): readonly NormalizedHitlInterrupt[] | undefined {
  const entries = (body as Raw | null)?.['interrupts'];
  if (!Array.isArray(entries) || entries.length > 16) return undefined;
  const cards: NormalizedHitlInterrupt[] = [];
  for (const entry of entries) {
    const raw = entry as Raw | null;
    const card = readInterruptCard(raw?.['card']);
    const state = raw?.['state'];
    const revision = raw?.['revision'];
    if (!card || (state !== 'PENDING' && state !== 'DECIDED') || typeof revision !== 'number' || !Number.isInteger(revision) || revision < 1) continue;
    cards.push(cardToInterrupt(card, { state, revision }));
  }
  return cards;
}

/** Fold the listed ledger cards into one message; the local hidden state of a card the user already answered survives. */
export function applyInterruptList(
  history: readonly ChatMessage[],
  messageId: string,
  listed: readonly NormalizedHitlInterrupt[],
): readonly ChatMessage[] {
  const index = history.findIndex((message) => message.id === messageId);
  const message = history[index];
  if (!message) return history;
  const existing = (message.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
  const local = new Map(existing.filter((card) => isLedgerInterrupt(card)).map((card) => [card.interrupt_key, card]));
  const kept = existing.filter((card) => !isLedgerInterrupt(card));
  const fresh = listed.map((card) => {
    const previous = local.get(card.interrupt_key);
    return previous?.hidden && !card.decided ? { ...card, queued: Boolean(previous.queued), decided: Boolean(previous.decided), hidden: true } : card;
  });
  const cards = [...kept, ...fresh];
  const next = [...history];
  next[index] = { ...message, hitlInterrupts: cards, hitlInterrupt: firstAnswerableInterrupt(cards) };
  return next;
}

/** The most resolved keys a message remembers (contract v1.1: 16 open cards per activation). */
const MAX_RESOLVED_LEDGER_KEYS = 64;

/** `keys` with `interruptKey` remembered, newest last, bounded. */
export function rememberResolvedKey(keys: readonly string[] | undefined, interruptKey: string): readonly string[] {
  const kept = (keys ?? []).filter((key) => key !== interruptKey);
  return [...kept, interruptKey].slice(-MAX_RESOLVED_LEDGER_KEYS);
}

/** Drop one ledger card from one message (409 = consumed, or a resolution from another tab). */
export function dropLedgerCardFromMessage(
  history: readonly ChatMessage[],
  messageId: string,
  interruptKey: string,
): readonly ChatMessage[] {
  const index = history.findIndex((message) => message.id === messageId);
  const message = history[index];
  if (!message) return history;
  const existing = (message.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
  const cards = existing.filter((card) => !(isLedgerInterrupt(card) && card.interrupt_key === interruptKey));
  const next = [...history];
  next[index] = {
    ...message,
    resolvedLedgerKeys: rememberResolvedKey(message.resolvedLedgerKeys, interruptKey),
    ...(cards.length === existing.length ? {} : { hitlInterrupts: cards, hitlInterrupt: firstAnswerableInterrupt(cards) }),
  };
  return next;
}

/** Does this message still hold an open ledger card, or wait parked for one? */
export function holdsLedgerWork(message: ChatMessage): boolean {
  if (message.fanoutParked) return true;
  return ((message.hitlInterrupts ?? []) as readonly Raw[]).some((card) => isLedgerInterrupt(card));
}
