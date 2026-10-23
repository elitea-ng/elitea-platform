// @ts-nocheck
/**
 * AddParticipants helpers — ported from
 * `apps/elitea-ui/src/[fsd]/features/chat/participants/lib/helpers/addParticipants.helpers.js`.
 *
 * CORRECTION (was: "getChatParticipantUniqueId/getParticipantName imports are
 * redirected to entities/participant — do NOT re-port participants.helpers.js
 * into this unit"): that redirect was wrong and is reverted below.
 * `entities/participant`'s `chatParticipantUniqueId`/`participantDisplayName`
 * key off the camelCase `Participant` domain shape (`entityName`/
 * `entityMeta`, produced by that slice's own wire normaliser). Every caller
 * in THIS cluster — `ParticipantItem.tsx`, `transformParticipant` below,
 * `useAddNewParticipants.ts` — works on the raw snake_case wire shape
 * (`entity_name`/`entity_meta`) instead, so calling the camelCase-keyed
 * selectors with a snake_case object always misses their lookup tables
 * (`NAME_RESOLVERS[undefined]`) and silently returns `''`/an identical
 * `"undefined__"` id for every participant. `getParticipantName` and
 * `getChatParticipantUniqueId` below are `participants.helpers.js:3-44`
 * ported directly on the snake_case shape instead, exactly as old-app did.
 */
import { ChatParticipantType } from '../model/constants';

import type { TransformedParticipant } from '../model/types';

const { Models } = ChatParticipantType;

// ---------------------------------------------------------------------------
// getChatParticipantUniqueId — ported from participants.helpers.js:3-21
// ---------------------------------------------------------------------------

/**
 * Builds a stable identity string for a participant on the raw snake_case
 * wire shape. Models have no stable top-level id, so they're keyed by
 * `model_name`-`integration_uid`; every other type is keyed by
 * `entity_meta.id`. An `applications` participant whose
 * `entity_settings.agent_type` is `'pipelines'` is keyed as a pipeline
 * (Applications/Pipelines share an underlying entity type).
 */
export function getChatParticipantUniqueId(participant: Record<string, unknown> | undefined): string {
  if (!participant) return '';
  const entityMeta = (participant.entity_meta as Record<string, unknown> | undefined) ?? {};
  const entitySettings = participant.entity_settings as Record<string, unknown> | undefined;
  const entityName =
    participant.entity_name === ChatParticipantType.Applications &&
    entitySettings?.agent_type === ChatParticipantType.Pipelines
      ? ChatParticipantType.Pipelines
      : participant.entity_name;
  const body =
    participant.entity_name === Models
      ? `${(entityMeta.model_name as string) ?? ''}-${(entityMeta.integration_uid as string) ?? ''}`
      : ((entityMeta.id as string) ?? '');
  return `${String(entityName)}_${body}_${(entityMeta.project_id as string) ?? ''}`;
}

// ---------------------------------------------------------------------------
// getParticipantName — ported from participants.helpers.js:23-44
// ---------------------------------------------------------------------------

function nameFromEntityMetaOrMeta(entityMeta: Record<string, unknown> | undefined, meta: Record<string, unknown> | undefined): string {
  return (entityMeta?.name as string) || (meta?.name as string) || '';
}

type NameResolver = (
  entityMeta: Record<string, unknown> | undefined,
  meta: Record<string, unknown> | undefined,
  systemSenderName: string,
) => string;

/**
 * `participants.helpers.js:23-44`'s per-`entity_name` dispatch, as a lookup
 * table rather than a `switch` — behaviourally identical, but keeps
 * `getParticipantName` itself under the complexity budget (a `switch` with
 * this many cases plus per-case fallback chains does not fit under 12) —
 * same technique `entities/participant/model/selectors.ts`'s own
 * `NAME_RESOLVERS` already uses for the camelCase equivalent.
 */
const NAME_RESOLVERS: Readonly<Record<string, NameResolver>> = {
  [ChatParticipantType.Applications]: nameFromEntityMetaOrMeta,
  [ChatParticipantType.Pipelines]: nameFromEntityMetaOrMeta,
  [ChatParticipantType.Toolkits]: nameFromEntityMetaOrMeta,
  [Models]: (entityMeta) => (entityMeta?.model_name as string) || '',
  [ChatParticipantType.Users]: (_entityMeta, meta) => (meta?.user_name as string) || '',
  [ChatParticipantType.Dummy]: (_entityMeta, _meta, systemSenderName) => systemSenderName,
};

/**
 * Resolves a participant's display name on the raw snake_case wire shape.
 * Ported from `participants.helpers.js:23-44` via `NAME_RESOLVERS` above.
 */
export function getParticipantName(
  participant: Record<string, unknown> | undefined,
  systemSenderName: string,
): string {
  const entityName = participant?.entity_name as string | undefined;
  const resolve = entityName ? NAME_RESOLVERS[entityName] : undefined;
  if (!resolve) return '';
  return resolve(participant?.entity_meta as Record<string, unknown> | undefined, participant?.meta as Record<string, unknown> | undefined, systemSenderName);
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const DEFAULT_MAX_TOKENS = 4096;
const DEFAULT_TEMPERATURE = 0.7;
const DEFAULT_REASONING_EFFORT = 'medium';

// ---------------------------------------------------------------------------
// isParticipantOKForChat — ported verbatim from old-app line 8-13
// ---------------------------------------------------------------------------

/**
 * Returns `true` if the participant entity type is valid for inclusion in a
 * chat. Ported from `addParticipants.helpers.js:8-13`.
 */
/**
 * participants.helpers.js `isSkippedContainerParticipant` (EliteaUI, issue
 * #5778), on this feature's snake_case shape: an agent the chat model is not
 * offered on an ad-hoc turn, because nesting it one level below the chat model
 * would exceed `max_agent_nesting_tiers`. Main sends `is_container`,
 * `agent_subtree_tiers` and `max_agent_nesting_tiers` on the participant meta
 * from the same check its ad-hoc start applies (FU-244, PLAN §8.8). Without
 * the tier fields it falls back to the blunt `is_container` ban, as EliteaUI does.
 * Callers gate on `!isActive`: selected directly, it runs as the root.
 */
export function isSkippedContainerParticipant(participant: Record<string, unknown> | undefined): boolean {
  const meta = participant?.meta as Record<string, unknown> | undefined;
  if (meta?.is_container !== true) return false;
  if (participant?.entity_name !== ChatParticipantType.Applications) return false;
  const subtreeTiers = meta.agent_subtree_tiers;
  const maxTiers = meta.max_agent_nesting_tiers;
  if (typeof subtreeTiers === 'number' && typeof maxTiers === 'number') {
    return 1 + subtreeTiers > maxTiers;
  }
  return true;
}

export function isParticipantOKForChat(participant: { entity_name?: string }): boolean {
  return (
    participant.entity_name === ChatParticipantType.Users ||
    participant.entity_name === ChatParticipantType.Toolkits ||
    participant.entity_name === ChatParticipantType.Applications ||
    participant.entity_name === ChatParticipantType.Pipelines
  );
}

// ---------------------------------------------------------------------------
// canParticipantBeActiveInChat — ported verbatim from old-app line 15-19
// ---------------------------------------------------------------------------

/**
 * Returns `true` if the participant can be selected as the active (LLM)
 * participant in a chat. Ported from `addParticipants.helpers.js:15-19`.
 */
export function canParticipantBeActiveInChat(participant: { entity_name?: string }): boolean {
  return (
    participant.entity_name === ChatParticipantType.Users ||
    participant.entity_name === ChatParticipantType.Applications ||
    participant.entity_name === ChatParticipantType.Pipelines
  );
}

// ---------------------------------------------------------------------------
// extractVariables — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Extracts the variables array from a participant, checking multiple sources
 * in priority order.
 */
function extractVariables(
  participant: Record<string, unknown>,
  participantType: ChatParticipantType,
  providedVariables: unknown[] | undefined,
): unknown[] {
  return (
    providedVariables ||
    (participant.entity_settings as Record<string, unknown>)?.variables ||
    (participantType === ChatParticipantType.Applications
      ? (participant.version_details as Record<string, unknown>)?.variables
      : undefined) ||
    []
  );
}

// ---------------------------------------------------------------------------
// extractIconMeta — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Extracts icon metadata from a participant, returning an empty object for
 * toolkit participants.
 */
function extractIconMeta(participant: Record<string, unknown>, participantType: ChatParticipantType): Record<string, unknown> {
  if (participantType === ChatParticipantType.Toolkits) {
    return {};
  }
  return {
    ...((participant.entity_settings as Record<string, any>)?.icon_meta ||
    (participant.meta as Record<string, any>)?.icon_meta ||
    (participant.icon_meta as Record<string, any>)),
  };
}

// ---------------------------------------------------------------------------
// extractVersionId — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Extracts version_id from a participant, checking multiple sources in
 * priority order.
 *
 * Pipelines read `version_details` too. They did not, and the omission was
 * load-bearing: a participant with no `version_id` fails the resolver's join
 * on `entity_settings ->> 'version_id'`
 * (`services/elitea-main/internal/db/queries/agent_chat.sql:151-153`) and
 * every turn addressed to it answers 422. Pipelines and agents are the same
 * `application` entity behind one `agent_type` discriminator — old-app's own
 * `convertParticipantAndAddIt` (`apps/elitea-ui/src/pages/NewChat/
 * NewConversationView.jsx:441-446`) takes `details.version_details?.id` for
 * every non-toolkit participant, not just the ones typed `application`.
 */
function extractVersionId(
  participant: Record<string, unknown>,
  participantType: ChatParticipantType,
): { version_id?: string } | undefined {
  const esVersion = (participant.entity_settings as Record<string, any>)?.version_id;
  if (esVersion) {
    return { version_id: esVersion };
  }
  if (participantType === ChatParticipantType.Applications || participantType === ChatParticipantType.Pipelines) {
    const vdId = (participant.version_details as Record<string, unknown>)?.id;
    if (vdId) {
      return { version_id: vdId as string };
    }
  }
  return undefined;
}

// ---------------------------------------------------------------------------
// extractMc pServerUrl — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Extracts the MCP server URL from a participant, checking multiple sources
 * in priority order.
 */
function extractMcpServerUrl(participant: Record<string, unknown>): string | undefined {
  return (participant.settings as Record<string, unknown>)?.url ||
    (participant.entity_settings as Record<string, unknown>)?.mcp_server_url ||
    undefined;
}

// ---------------------------------------------------------------------------
// buildNonModelParticipant — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Builds a non-Model transformed participant.
 */
function buildNonModelParticipant(
  participantType: ChatParticipantType,
  participant: Record<string, unknown>,
  variables?: unknown[],
): TransformedParticipant {
  const entityName = participant.agent_type === 'pipeline'
    ? ChatParticipantType.Applications
    : participantType;

  const entityMeta: Record<string, unknown> = {};
  if (participant.id) entityMeta.id = participant.id;
  if (participant.name) entityMeta.name = participant.name;
  if (participant.project_id) entityMeta.project_id = participant.project_id;

  const entitySettings: Record<string, unknown> = {
    variables: extractVariables(participant, participantType, variables),
    icon_meta: extractIconMeta(participant, participantType),
    toolkit_type: participant.type,
    agent_type: participant.agent_type,
    mcp_server_url: extractMcpServerUrl(participant),
  };

  const versionId = extractVersionId(participant, participantType);
  if (versionId) {
    entitySettings.version_id = versionId.version_id;
  }

  return {
    entity_name: entityName,
    entity_meta: entityMeta,
    entity_settings: entitySettings,
    meta: {
      mcp: (participant.meta as Record<string, unknown>)?.mcp || undefined,
    },
  };
}

// ---------------------------------------------------------------------------
// buildModelParticipant — extracted from transformParticipant
// ---------------------------------------------------------------------------

/**
 * Builds a Model transformed participant (completely different wire shape).
 */
function buildModelParticipant(participant: Record<string, unknown>): TransformedParticipant {
  const modelMeta: Record<string, unknown> = {};
  if (participant.integration_uid) modelMeta.integration_uid = participant.integration_uid;
  if (participant.model_name) modelMeta.model_name = participant.model_name;

  return {
    entity_name: Models,
    entity_meta: modelMeta,
    entity_settings: {
      max_tokens: (participant.max_tokens as number) || DEFAULT_MAX_TOKENS,
      temperature: (participant.temperature as number) || DEFAULT_TEMPERATURE,
      reasoning_effort: (participant.reasoning_effort as string) || DEFAULT_REASONING_EFFORT,
    },
  };
}

// ---------------------------------------------------------------------------
// transformParticipant — orchestrator (complexity ≤ 4)
// ---------------------------------------------------------------------------

/**
 * Transforms a raw participant item from the candidate-browsing list into
 * the wire-shaped entity used by the chat conversation API.
 *
 * Ported from `addParticipants.helpers.js:21-81`.
 */
export function transformParticipant(
  participantType: ChatParticipantType,
  participant: Record<string, unknown>,
  _variables?: unknown[],
): TransformedParticipant {
  if (participantType === ChatParticipantType.Models) {
    return buildModelParticipant(participant);
  }
  return buildNonModelParticipant(participantType, participant, _variables);
}

// ---------------------------------------------------------------------------
// isParticipantsEqual — ported from old-app line 85-101
// ---------------------------------------------------------------------------

/**
 * Checks whether two participants represent the same entity.
 *
 * For Models: compares `entity_name`, `id`/`model_name`, and `integration_uid`.
 * For all other types: compares unique IDs (which include `project_id`).
 *
 * Ported from `addParticipants.helpers.js:85-101`.
 */
export function isParticipantsEqual(
  a: Record<string, unknown>,
  b: TransformedParticipant,
  type: ChatParticipantType,
  idFieldName: string,
): boolean {
  if (type === Models) {
    return (
      a.entity_name === b.entity_name &&
      String((a.entity_meta as Record<string, unknown>)?.[idFieldName] ?? a[idFieldName]) ===
        String(b.entity_meta[idFieldName as keyof typeof b.entity_meta]) &&
      // @ts-expect-error — 'a.entity_meta' is of type 'unknown'
      a.entity_meta.integration_uid === b.entity_meta.integration_uid
    );
  }

  // For all other types, use unique IDs to distinguish between public and custom entities
  const aId = getChatParticipantUniqueId(a);
  const bId = getChatParticipantUniqueId(b);
  return aId === bId;
}
