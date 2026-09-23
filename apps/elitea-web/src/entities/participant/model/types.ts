/**
 * Participant domain type — an entity attached to a chat conversation
 * (an agent, toolkit, model, pipeline, skill or user).
 *
 * Settings sub-shape mirrors OpenAPI schema `ParticipantSettingsRequest`
 * (services/elitea-main/api/openapi/v2.yaml:1819-1831, unit W2): decoded into
 * `map[string]any` and stored verbatim as the participant's entity settings
 * (internal/api/v2/conversations/handler.go:585-624); only `llmSettings` and
 * `versionId` are ever inspected server-side.
 *
 * The participant envelope itself (`entity_name`/`entity_meta`/`meta`) has no
 * OpenAPI schema — it is chat-domain, socket/conversation-detail-sourced.
 * Evidence: apps/elitea-ui/src/[fsd]/features/chat/api/chat.api.js:124-184
 * (add/delete/settings endpoints); apps/elitea-ui/src/common/constants.js:
 * 950-958 (`ChatParticipantType` enum); apps/elitea-ui/src/[fsd]/features/
 * chat/participants/lib/helpers/participants.helpers.js (field usage).
 */

/** `ChatParticipantType` — apps/elitea-ui/src/common/constants.js:950-958. */
export type ParticipantType = 'application' | 'toolkit' | 'llm' | 'user' | 'pipeline' | 'skill' | 'dummy';

/** Opaque per-participant settings blob — see the module doc for the schema citation. */
export interface ParticipantSettings {
  /** Opaque DB-jsonb passthrough; only inspected for a handful of known keys server-side. */
  readonly llmSettings?: Readonly<Record<string, unknown>>;
  readonly versionId?: string | number;
  readonly variables?: unknown;
  readonly iconMeta?: unknown;
  readonly toolkitType?: string;
  readonly agentType?: string;
  readonly mcpServerUrl?: string;
}

/**
 * `entity_meta` — fields read across participants.helpers.js:23-44 (name
 * derivation) and addParticipants.helpers.js (equality/transform).
 */
export interface ParticipantEntityMeta {
  readonly id?: string;
  readonly name?: string;
  readonly projectId?: string;
  readonly modelName?: string;
  readonly integrationUid?: string;
}

/**
 * `meta` — fields read across participants.helpers.js:23-77 (name/liveness
 * derivation) and useCanvasSocket/CanvasEditor.jsx (`is_container`, `mcp`).
 *
 * `id` (unit C1 addition): read by `hooks/chat/
 * useIsActiveParticipantBeingEdited.js:33` (`activeParticipant.meta?.id`) as
 * one of four candidate id fields matched against the `edited_participant_id`
 * URL param — not previously modeled because no earlier consumer needed it.
 */
export interface ParticipantMeta {
  readonly id?: string;
  readonly name?: string;
  readonly userName?: string;
  readonly userAvatar?: string;
  readonly isContainer?: boolean;
  readonly mcp?: boolean;
  /**
   * #972 — the SERVER's answer to "has the version this conversation is bound
   * to been withdrawn from the catalogue?", resolved by
   * `repos.ConversationsRepo.enrichAgentParticipantPublication` on the
   * conversation read.
   *
   * `undefined` means NOT RESOLVED (a version that is gone, a tenant without
   * the table), which is a different state from `false` and must not be
   * rendered as "still published".
   */
  readonly versionWithdrawn?: boolean;
  /** #972 — the raw `application_versions.status` beside the flag above. */
  readonly versionStatus?: string;
}

export interface Participant {
  readonly id: string;
  readonly entityName: ParticipantType;
  readonly entityMeta?: ParticipantEntityMeta;
  readonly meta?: ParticipantMeta;
  readonly entitySettings?: ParticipantSettings;
  /**
   * Top-level fallback read by `isSkippedContainerParticipant`
   * (participants.helpers.js:60) ALONGSIDE `entitySettings.agentType` —
   * both are checked, not just the nested one.
   */
  readonly agentType?: string;
  /**
   * Top-level `entity_id` (unit C1 addition): read by `hooks/chat/
   * useIsActiveParticipantBeingEdited.js:34` as a fourth candidate id field.
   * Not present in the `conversations.Participant` wire shape this app's
   * backend actually emits (`services/elitea-main/internal/api/v2/
   * conversations/handler.go:59-65`: `id`/`entity_name`/`entity_meta`/
   * `entity_settings`/`meta` only) — modeled anyway, always absent on real
   * data, for byte-for-byte parity with the old hook's defensive check.
   */
  readonly entityId?: string;
}
