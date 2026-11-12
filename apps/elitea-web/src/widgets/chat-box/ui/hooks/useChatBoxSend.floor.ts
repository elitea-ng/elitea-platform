/**
 * ADR-0050 decision 7c on the send path (issue 1612): the agents a message
 * addresses travel as `addressed_to` (`ChatSendRequest`), and the turn goes
 * to an agent the person addressed.
 *
 * Split out of `useChatBoxSend.helpers.ts`, which is at its §3.5 length budget.
 */
import { MAX_ADDRESSED, isPeopleOnly } from '@elitea/client-core/conversation';

/** A positive integer participant id, from a number or its decimal string; `undefined` otherwise. */
export function positiveParticipantId(raw: unknown): number | undefined {
  if (raw === undefined || raw === null || raw === '') return undefined;
  const numeric = Number(raw);
  return Number.isSafeInteger(numeric) && numeric > 0 ? numeric : undefined;
}

const positiveId = positiveParticipantId;

/**
 * The participant an unaddressed message goes to, with its id read ONCE
 * (issue 1859): the send, the stream identity, the attachments gate and
 * "To:" all read `participantId`, so a row whose id is `0` or not numeric
 * has no id for every one of them alike.
 */
export interface ResolvedTarget {
  /** The raw participant row (an agent, the conversation's model, a user, or unresolved). */
  readonly participant: unknown;
  /** Its positive integer id (`positiveParticipantId`); `undefined` without one. */
  readonly participantId: number | undefined;
}

/** No target: nothing picked and nothing to resolve to. */
export const NO_TARGET: ResolvedTarget = { participant: undefined, participantId: undefined };

/** The `ResolvedTarget` of a participant row. */
export function resolvedTarget(participant: unknown): ResolvedTarget {
  if (participant === undefined) return NO_TARGET;
  const id = typeof participant === 'object' && participant !== null && 'id' in participant ? participant.id : undefined;
  return { participant, participantId: positiveId(id) };
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

/**
 * Whether a composer payload is for people alone: it carries mentions (`userIds` as positive integers, or
 * `isMentioningEveryone`) and addresses no agent (`addressedTo`). Main decides the same from the start body, so a
 * message this answers `true` for is stored and starts no turn: the send opens no stream and shows no pending answer.
 */
export function isPeopleOnlyPayload(payload: Readonly<Record<string, unknown>>): boolean {
  const raw = payload['userIds'];
  const mentionedUserIds = Array.isArray(raw) ? raw.filter((value) => positiveId(value) !== undefined) : [];
  return isPeopleOnly({ mentionedUserIds, mentionsEveryone: payload['isMentioningEveryone'] === true, addressedTo: addressedIdsOf(payload['addressedTo']) });
}

/**
 * The agents a message addresses once the composer's PICKED agent is counted. A message with mentions and no
 * `@Agent` / `#Agent` is for people alone, so an agent the person picked on purpose has to travel as an address to
 * keep the turn (Main cannot tell a pick from the sticky `participant_id`). Only an application participant is an
 * agent: a picked model is not addressable and does not turn a message to people into a model turn.
 */
export function addressedWithPick(addressedTo: readonly number[] | undefined, picked: unknown, mentions: { readonly userIds?: readonly unknown[] | undefined; readonly everyone?: boolean | undefined }, participants: readonly unknown[] | undefined): readonly number[] | undefined {
  const forPeople = isPeopleOnly({ mentionedUserIds: mentions.userIds, mentionsEveryone: mentions.everyone, addressedTo });
  const id = forPeople ? pickedAgentId(picked) : undefined;
  // Only an agent of THIS conversation: Main refuses an addressee that is not one (422), which would drop the whole message.
  const pickedAgent = id !== undefined && (participants ?? []).some((row) => resolvedTarget(row).participantId === id) ? id : undefined;
  return pickedAgent === undefined ? addressedTo : [pickedAgent];
}

/** The id of a picked application participant (an agent); `undefined` for a model, a person or nothing. */
function pickedAgentId(picked: unknown): number | undefined {
  const row = (picked ?? {}) as { readonly id?: unknown; readonly entity_name?: unknown; readonly entityName?: unknown };
  return (row.entity_name ?? row.entityName) === 'application' ? positiveId(row.id) : undefined;
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
export function targetForAddressed(target: ResolvedTarget, participants: readonly unknown[] | undefined, addressed: readonly number[]): ResolvedTarget {
  const first = addressed[0];
  if (first === undefined) return target;
  if (target.participantId !== undefined && addressed.includes(target.participantId)) return target;
  const row = (participants ?? []).find((participant) => resolvedTarget(participant).participantId === first);
  return row === undefined ? target : resolvedTarget(row);
}
