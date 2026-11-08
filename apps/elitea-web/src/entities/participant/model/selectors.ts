import type { Participant, ParticipantType } from './types';
import { productName } from '@/shared/brand';

/** apps/elitea-ui/src/common/constants.js:88 `DEFAULT_PARTICIPANT_NAME`. */
/** The assistant's name when no sender name is configured: the product's (ADR-0052 W3.3). */
export const DEFAULT_PARTICIPANT_NAME = productName();

/**
 * participants.helpers.js:5-8 — an `application` participant whose
 * `entitySettings.agentType` is `"pipeline"` is keyed as a pipeline
 * (Applications/Pipelines share an underlying entity type, see
 * entities/application's module doc).
 */
function uniqueIdEntityKey(participant: Participant): string {
  const isApplicationActingAsPipeline =
    participant.entityName === 'application' && participant.entitySettings?.agentType === 'pipeline';
  return isApplicationActingAsPipeline ? 'pipeline' : participant.entityName;
}

/** participants.helpers.js:10-14 — Models compose `modelName-integrationUid`; everything else uses `entityMeta.id`. */
function uniqueIdBody(participant: Participant): string {
  if (participant.entityName === 'llm') {
    return `${participant.entityMeta?.modelName ?? ''}-${participant.entityMeta?.integrationUid ?? ''}`;
  }
  return participant.entityMeta?.id ?? '';
}

/**
 * apps/elitea-ui/src/[fsd]/features/chat/participants/lib/helpers/
 * participants.helpers.js:3-21 `getChatParticipantUniqueId`, ported
 * verbatim (split into the two pure helpers above to stay under the §3.5
 * cyclomatic-complexity budget); `entityMeta.projectId` is always appended,
 * defaulting to `""`.
 */
export function chatParticipantUniqueId(participant: Participant): string {
  return `${uniqueIdEntityKey(participant)}_${uniqueIdBody(participant)}_${participant.entityMeta?.projectId ?? ''}`;
}

/** participants.helpers.js:24,32,37,40 — the shared entityMeta.name/meta.name fallback chain. */
function nameFromEntityMetaOrMeta(participant: Participant): string {
  return participant.entityMeta?.name ?? participant.meta?.name ?? '';
}

function nameFromModel(participant: Participant): string {
  return participant.entityMeta?.modelName ?? '';
}

function nameFromUserMeta(participant: Participant): string {
  return participant.meta?.userName ?? '';
}

/**
 * participants.helpers.js:23-44 `getParticipantName`'s per-`entityName`
 * dispatch, as a lookup table rather than a `switch` — behaviourally
 * identical, but keeps `participantDisplayName` itself under the §3.5
 * cyclomatic-complexity budget (a `switch` with this many cases plus
 * per-case fallback chains does not fit under 12).
 */
const NAME_RESOLVERS: Readonly<Record<ParticipantType, (participant: Participant, systemSenderName: string) => string>> = {
  application: nameFromEntityMetaOrMeta,
  pipeline: nameFromEntityMetaOrMeta,
  toolkit: nameFromEntityMetaOrMeta,
  skill: nameFromEntityMetaOrMeta,
  llm: nameFromModel,
  user: nameFromUserMeta,
  dummy: (_participant, systemSenderName) => systemSenderName,
};

/**
 * apps/elitea-ui/src/[fsd]/features/chat/participants/lib/helpers/
 * participants.helpers.js:23-44 `getParticipantName`, ported verbatim in
 * effect via the `NAME_RESOLVERS` table above. A lookup miss is
 * unreachable given `ParticipantType`'s closed union, but the `''`
 * fallback is kept for resilience against malformed/mistyped input at a
 * real socket/API boundary — mirroring the old app's own defensive
 * `default: return ''`.
 */
export function participantDisplayName(
  participant: Participant,
  systemSenderName: string = DEFAULT_PARTICIPANT_NAME,
): string {
  const resolve = NAME_RESOLVERS[participant.entityName];
  return resolve !== undefined ? resolve(participant, systemSenderName) : '';
}

/**
 * apps/elitea-ui/src/[fsd]/features/chat/participants/lib/helpers/
 * participants.helpers.js:55-62 `isSkippedContainerParticipant`, ported
 * verbatim: a "container" agent (self-orchestrating, not itself callable as
 * a tool) is any non-pipeline `application` participant with
 * `meta.isContainer === true`. Pipelines are the sanctioned
 * deep-composition primitive and are never flagged, hence the explicit
 * `entitySettings.agentType`/top-level `agentType` pipeline check.
 */
export function isSkippedContainerParticipant(participant: Participant): boolean {
  if (participant.meta?.isContainer !== true) return false;
  if (participant.entityName !== 'application') return false;
  const isPipeline = participant.entitySettings?.agentType === 'pipeline' || participant.agentType === 'pipeline';
  return !isPipeline;
}

/**
 * The RAW snake_case participant row, exactly as the conversation payload
 * carries it (`services/elitea-main/internal/api/v2/conversations/
 * handler.go`'s `Participant`: `id`/`entity_name`/`entity_meta`/
 * `entity_settings`/`meta`, with `meta.user_name` overlaid by
 * `ListParticipants`). `features/chat-participants` works on this shape end
 * to end and never normalises it — see that slice's `lib/helpers.ts`, whose
 * own CORRECTION note records the same camelCase/snake_case mismatch being
 * found and fixed for `getChatParticipantUniqueId`/`getParticipantName`.
 * `isParticipantStillActive` was the third selector of that trio and was
 * left behind, so it is the one function here that accepts both shapes.
 */
interface ParticipantWireRow {
  readonly id?: string | number;
  readonly entity_name?: string;
  /**
   * Open shapes, not the two keys this predicate happens to read: a wire row
   * is handed over whole (`entity_meta` carries `id`/`project_id`/`name`/
   * `integration_uid` besides `model_name`), and a closed literal type would
   * reject every real row at the call site while accepting the trimmed
   * fixture that hid the original defect.
   */
  readonly entity_meta?: Record<string, unknown>;
  readonly entity_settings?: Record<string, unknown>;
  readonly meta?: Record<string, unknown>;
}

/** Either shape this predicate accepts. */
type LivenessInput = Participant | ParticipantWireRow;

/**
 * Both shapes at once. A participant row only ever carries ONE of each pair
 * at runtime; the intersection exists so the reads below can name both keys
 * without a per-key narrowing dance that TS cannot do on an optional-property
 * union anyway.
 */
type EitherShape = Participant & ParticipantWireRow;

function livenessEntityName(participant: LivenessInput): string | undefined {
  const row = participant as EitherShape;
  return row.entityName ?? row.entity_name;
}

/** `meta.name` is spelled the same in both shapes. */
function hasMetaName(participant: LivenessInput): boolean {
  return Boolean((participant as EitherShape).meta?.name);
}

function hasModelName(participant: LivenessInput): boolean {
  const row = participant as EitherShape;
  return Boolean(row.entityMeta?.modelName ?? row.entity_meta?.model_name);
}

/**
 * `participants.helpers.js:64-77`'s four explicit cases, as a lookup table
 * (same technique as `NAME_RESOLVERS` above, and for the same §3.5
 * complexity reason). Every entity_name absent from this table — `user`,
 * `toolkit`, `pipeline`, and anything unrecognised — is the baseline's
 * `default: return false`.
 */
const LIVENESS_RESOLVERS: Readonly<Record<string, (participant: LivenessInput) => boolean>> = {
  application: hasMetaName,
  skill: hasMetaName,
  llm: hasModelName,
  dummy: () => true,
};

/**
 * apps/elitea-ui/src/[fsd]/features/chat/participants/lib/helpers/
 * participants.helpers.js:64-77 `isParticipantStillActive`.
 *
 * MEASURED BASELINE, because the `user` arm looks like a bug and is not.
 * The old app calls this from exactly ONE place — `features/chat/ui/
 * chat-box/ChatMessageWrapper.jsx:148`, gating the **Regenerate** control on
 * the last message. "Still active" therefore means "this message's sender is
 * an entity a new turn could be re-addressed to", which a user, a toolkit
 * and a bare pipeline are not; `false` for those three is the answer the
 * baseline gives and the answer this port keeps.
 *
 * It is NOT a visibility predicate. The baseline's participants rail
 * (`ExpandedParticipantsList.jsx:50-56`) never calls it: it filters user
 * participants by `entity_name === ChatParticipantType.Users` alone, so user
 * rows DO render there. `features/chat-participants/ui/Participants.tsx`
 * used it as a rail filter and dropped every user row twice over — once on
 * the shape, once on this arm. That call site is gone; see its own comment.
 *
 * Two fidelity fixes over the first port, both in the "absence reads as
 * correctness" class this repo keeps hitting:
 *  - a `switch` over `ParticipantType` returned `undefined` (not `false`)
 *    for anything outside the closed union — including a row whose
 *    `entityName` was simply absent. The baseline has an explicit
 *    `default: return false`; so does the table above.
 *  - a raw wire row matched nothing at all. Both spellings are read now, so
 *    handing this a `GET /conversation/...` participant answers the baseline
 *    answer rather than silently answering "gone".
 */
export function isParticipantStillActive(participant: LivenessInput): boolean {
  const entityName = livenessEntityName(participant);
  const resolve = entityName === undefined ? undefined : LIVENESS_RESOLVERS[entityName];
  return resolve !== undefined && resolve(participant);
}
