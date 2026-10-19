/**
 * api/interruptLedgerApi.ts — the public per-interrupt decision API
 * (contract `fanout-interrupt-decisions-v1.md` §6), through the generated
 * client (`listExecutionInterrupts`, `decideExecutionInterrupt`).
 *
 *   GET  /elitea_core/task/prompt_lib/{projectId}/{responseMessageId}/interrupts
 *   POST /elitea_core/task/prompt_lib/{projectId}/{responseMessageId}/interrupts/{interruptKey}/decision
 *
 * Every answer becomes a value: a 409 is `consumed` (the card was answered
 * elsewhere — quiet), 400/403/404 are readable refusals, and nothing here
 * throws or retries on its own. Path segments are opaque ids the server minted
 * (a 36-character UUID and a 64-hex key) and are checked before use, so no
 * value can address another route.
 */
import { decideExecutionInterrupt, listExecutionInterrupts } from '@/shared/api/generated/chat/chat';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import { isInterruptKey } from '../lib/fanoutFrames';
import { readInterruptList, type LedgerDecisionBody } from '../lib/interruptLedger';
import type { NormalizedHitlInterrupt } from '../lib/hitlInterrupts';

const RESPONSE_MESSAGE_ID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/u;
const PROJECT_ID = /^[1-9][0-9]{0,18}$/u;

type LedgerRefusal = 'invalid' | 'forbidden' | 'not_found' | 'unavailable';

export type LedgerDecisionOutcome =
  | { readonly kind: 'decided' }
  | { readonly kind: 'consumed' }
  | { readonly kind: 'refused'; readonly reason: LedgerRefusal };

function pathIds(projectId: string | number | undefined, responseMessageId: string): { project: string; response: string } | undefined {
  const project = String(projectId ?? '');
  return PROJECT_ID.test(project) && RESPONSE_MESSAGE_ID.test(responseMessageId) ? { project, response: responseMessageId } : undefined;
}

function refusalOf(error: unknown): LedgerDecisionOutcome {
  if (!(error instanceof EliteaApiError)) return { kind: 'refused', reason: 'unavailable' };
  const failure = error.failure;
  if (failure.kind === 'auth') return { kind: 'refused', reason: 'forbidden' };
  if (failure.kind !== 'http') return { kind: 'refused', reason: 'unavailable' };
  switch (failure.status) {
    case 409:
      return { kind: 'consumed' };
    case 400:
      return { kind: 'refused', reason: 'invalid' };
    case 403:
      return { kind: 'refused', reason: 'forbidden' };
    case 404:
      return { kind: 'refused', reason: 'not_found' };
    default:
      return { kind: 'refused', reason: 'unavailable' };
  }
}

/** Record one card's decision. One attempt; the caller reuses `request_id` for a retry of the same body. */
export async function decideLedgerInterrupt(
  projectId: string | number | undefined,
  responseMessageId: string,
  interruptKey: string,
  body: LedgerDecisionBody,
): Promise<LedgerDecisionOutcome> {
  const ids = pathIds(projectId, responseMessageId);
  if (!ids || !isInterruptKey(interruptKey)) return { kind: 'refused', reason: 'invalid' };
  try {
    await decideExecutionInterrupt(ids.project, ids.response, interruptKey, body);
    return { kind: 'decided' };
  } catch (error) {
    return refusalOf(error);
  }
}

/** The open ledger cards of one response, or `undefined` when they could not be read. */
export async function listLedgerInterrupts(
  projectId: string | number | undefined,
  responseMessageId: string,
): Promise<readonly NormalizedHitlInterrupt[] | undefined> {
  const ids = pathIds(projectId, responseMessageId);
  if (!ids) return undefined;
  try {
    const response = await listExecutionInterrupts(ids.project, ids.response);
    return readInterruptList(response.data);
  } catch {
    return undefined;
  }
}
