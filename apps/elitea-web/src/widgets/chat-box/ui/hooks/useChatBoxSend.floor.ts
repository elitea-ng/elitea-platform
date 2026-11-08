/**
 * ADR-0050 decision 7c on the send path (issue 1612): the agents a message
 * addresses travel as `addressed_to` (`ChatSendRequest`), and the turn goes
 * to an agent the person addressed.
 *
 * Split out of `useChatBoxSend.helpers.ts`, which is at its §3.5 length budget.
 */
import { MAX_ADDRESSED } from '@elitea/client-core/conversation';

function positiveId(value: unknown): number | undefined {
  const numeric = typeof value === 'string' && /^\d{1,15}$/.test(value) ? Number(value) : value;
  return typeof numeric === 'number' && Number.isSafeInteger(numeric) && numeric > 0 ? numeric : undefined;
}

/**
 * The addressed participant ids a composer payload carries (`addressedTo`):
 * positive integers, each once, at most `MAX_ADDRESSED`. Anything else is
 * dropped, as the start route would refuse the whole message for it.
 */
export function addressedIdsOf(raw: unknown): number[] {
  if (!Array.isArray(raw)) return [];
  const ids: number[] = [];
  for (const value of raw) {
    const id = positiveId(value);
    if (id !== undefined && !ids.includes(id)) ids.push(id);
    if (ids.length === MAX_ADDRESSED) break;
  }
  return ids;
}

/** The start body's `addressed_to`, present only when the message addresses someone. */
export function addressedField(payload: Readonly<Record<string, unknown>>): { addressed_to?: number[] } {
  const ids = addressedIdsOf(payload['addressedTo']);
  return ids.length > 0 ? { addressed_to: ids } : {};
}

/**
 * The start body's `participant_id`: the composer payload's own pick when the
 * turn still goes to the composer's target, else the agent it goes to.
 */
export function startParticipantId(input: {
  readonly isApplicationTurn: boolean;
  readonly redirected: boolean;
  readonly payloadParticipantId: unknown;
  readonly targetParticipantId: number | undefined;
}): number | undefined {
  const own = input.isApplicationTurn && !input.redirected ? positiveId(input.payloadParticipantId) : undefined;
  return own ?? input.targetParticipantId;
}

/**
 * The participant the turn is sent to when the message addresses agents.
 * Main lets the composer's target answer first only when it is one of the
 * addressees; otherwise the first addressee answers. The client sends the
 * turn to that agent too, so the contract (`agent.execute.application.v1`)
 * and `participant_id` match the agent that answers.
 */
export function targetForAddressed(target: unknown, participants: readonly unknown[] | undefined, addressed: readonly number[]): unknown {
  const first = addressed[0];
  if (first === undefined) return target;
  const targetId = positiveId((target as { readonly id?: unknown } | null | undefined)?.id);
  if (targetId !== undefined && addressed.includes(targetId)) return target;
  return (participants ?? []).find((participant) => positiveId((participant as { readonly id?: unknown } | null)?.id) === first) ?? target;
}
