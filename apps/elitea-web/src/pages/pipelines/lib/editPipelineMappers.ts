import type { ApplicationCreationInput } from '@/entities/application-form';
import type { VersionSummary } from '@/entities/version';
import type { ConfigurationTabProps, PipelineGraphDraft } from '@/features/pipelines';
import { toLlmSettingsBody, type AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type {
  ApplicationDetail,
  ApplicationVersionDetail,
  ApplicationVersionSummary,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import type { EditPipelineVersionFields } from './useEditPipelineVersionFields';

/**
 * Pure mapping helpers for `EditPipeline.tsx`, split into their own file
 * purely to keep that page file under the §3.5 400-line budget — same
 * rationale (and, aside from the pinned `agent_type` and the
 * `pipeline_settings` handling, near-identical body) as
 * `pages/agents/lib/editApplicationMappers.ts`.
 */

export const EMPTY_FORM_VALUES: ApplicationCreationInput = {
  name: '',
  description: '',
  version_details: { conversation_starters: [], tags: [] },
};

function storedVersionTagNames(version: ApplicationVersionDetail | undefined): string[] {
  return (version?.tags ?? []).map((tag) => tag.name).filter((name): name is string => typeof name === 'string');
}

/** Generated `ApplicationVersionSummary[]` (snake_case) -> `entities/version`'s `VersionSummary[]` (camelCase) — needed only to satisfy `useIsVersionNotFound`'s parameter type. */
export function toVersionSummaries(versions: readonly ApplicationVersionSummary[]): VersionSummary[] {
  return versions.map((version) => ({
    id: version.id,
    name: version.name,
    status: version.status,
    agentType: version.agent_type,
    createdAt: version.created_at,
  }));
}

/**
 * This page works directly with the GENERATED (snake_case) response types
 * rather than `entities/application`'s normalisers — same
 * `exactOptionalPropertyTypes` mismatch `pages/agents/lib/
 * editApplicationMappers.ts` (Wave-2 unit A1g) already documents in full
 * (confirmed directly via `tsc`, TS2379).
 */
export function pipelineDetailDisplayName(detail: ApplicationDetail): string {
  return detail.name.trim() !== '' ? detail.name : 'Untitled';
}

export function toFormValues(detail: ApplicationDetail, version: ApplicationVersionDetail | undefined): ApplicationCreationInput {
  return {
    name: detail.name,
    description: detail.description,
    version_details: {
      conversation_starters: (version?.conversation_starters ?? []).filter((entry): entry is string => typeof entry === 'string'),
      tags: storedVersionTagNames(version),
    },
  };
}

/**
 * The `meta` blob a pipeline write sends, MERGED over the version's stored
 * one rather than replacing it.
 *
 * `versionFromBody` (`applications/handler.go:504`) takes `vBody["meta"]` as
 * the whole map and `insertVersion`/`UpdateVersion` persist it verbatim, so
 * sending only the keys this mapper knows about drops every other key the
 * version carried. `icon_meta` is the one that was measurably lost —
 * `toChatPipelineVersionDetails`, in this same file, reads it off a pipeline
 * version's `meta` and forwards it to the chat — and
 * `category`/`attachment_storage` went the same way.
 *
 * `variables` is DROPPED from the stored blob, and that is deliberate: it is
 * the one key both handlers rebuild from the body's TOP-LEVEL `variables`
 * list, so a copy carried inside `meta` can only contradict the authoritative
 * one — and on the create path it WINS, because `versionFromBody` folds the
 * list only when it is non-empty. The agents twin
 * (`pages/agents/lib/editApplicationMappers.ts`'s `toVersionMetaBody`) makes
 * exactly this cut, for a measured reason: forwarding it resurrected deleted
 * variables, secrets included.
 */
function toPipelineMetaBody(
  version: ApplicationVersionDetail,
  edits?: EditPipelineVersionFields,
): Pick<VersionWriteRequest, 'meta'> {
  const metaRecord: Record<string, unknown> = version.meta ?? {};
  const storedStepLimit = typeof metaRecord['step_limit'] === 'number' ? metaRecord['step_limit'] : 25;
  const internalToolsRaw = metaRecord['internal_tools'];
  const storedInternalTools = Array.isArray(internalToolsRaw)
    ? internalToolsRaw.filter((entry): entry is string => typeof entry === 'string')
    : [];
  const { variables: _storedVariables, ...storedMeta } = metaRecord;
  return {
    meta: {
      ...storedMeta,
      step_limit: edits?.stepLimit ?? storedStepLimit,
      // Always sent, never gated on being non-empty: turning the LAST module
      // off has to reach the wire, and an `undefined`-when-empty guard would
      // make exactly that one edit silently unsaveable.
      internal_tools: [...(edits?.internalTools ?? storedInternalTools)],
    },
  };
}

/**
 * The `llm_settings` key, or nothing at all. The live pick wins; with no pick
 * the stored blob is forwarded VERBATIM rather than re-read through
 * `toAgentLlmSettings`, because a stored `{model_name}` with no
 * `model_project_id` is a real, working shape that the strict read would
 * reject — silently moving the version onto a different model.
 */
function selectLlmSettings(
  version: ApplicationVersionDetail,
  edited: AgentLlmSettings | undefined,
): Pick<VersionWriteRequest, 'llm_settings'> {
  if (edited !== undefined) return { llm_settings: toLlmSettingsBody(edited) };
  return version.llm_settings === undefined ? {} : { llm_settings: version.llm_settings };
}

/**
 * The body a pipeline Save PUTs to `PUT /elitea_core/version/prompt_lib/
 * {projectId}/{applicationId}/{versionId}`.
 *
 * Every key below was read against `UpdateVersion`
 * (`services/elitea-main/internal/api/v2/applications/handler.go:891-1026`)
 * and `ApplicationsRepo.UpdateVersion`
 * (`internal/infra/db/repos/applications.go:590-640`) rather than against the
 * schema, because that schema is `additionalProperties: true` and would have
 * accepted anything: `name`, `agent_type`, `instructions`, `welcome_message`,
 * `llm_settings`, `conversation_starters`, `meta`, `variables`, `tags` and
 * `pipeline_settings` are the ten the handler actually writes.
 *
 * **What is NOT here, and why.** `tools` is accepted by the schema and read
 * by no branch of that handler — attach/detach goes through the
 * `entity_tool_mapping` relation endpoints, which is what
 * `../ui/EditPipelineToolsPanel.tsx` drives, independently of this Save. And
 * `notes` (the reference's EDITOR NOTES field) has no column, no schema
 * property and no handler branch at all.
 *
 * This function REPLACES `toVersionDraft`, which produced
 * `entities/application-form`'s camelCase `ApplicationVersionDraft` for
 * `useSaveApplicationVersion`. That path could not carry `tags` (its
 * `toVersionWriteRequest` writes no such key) and the page fed it no
 * `welcome_message`, so both fields were rendered from the server response
 * and dropped on save. The raw `VersionWriteRequest` goes to
 * `features/agents`' `useSaveVersion`, which is also the only hook that
 * issues the SECOND call the pipeline name/description need.
 *
 * `agent_type` is pinned to `'pipeline'`, never cloned:
 * `insertVersion`(`repos/applications.go:493-496`) substitutes the literal
 * `"openai"` for an empty `agent_type`, so an omitted key mints an OpenAI
 * agent out of a pipeline — the same rows, run by the wrong executor.
 *
 * `graph` is the live flow-editor state. When it is `undefined` — no flow
 * editor mounted, or its stores not seeded yet — the version's already-loaded
 * `instructions` are re-sent unchanged and no `pipeline_settings` key is
 * written, so a save can never blank a stored graph it was not showing.
 */
export function toPipelineVersionSaveBody(
  version: ApplicationVersionDetail,
  conversationStarters: readonly string[],
  edits: EditPipelineVersionFields,
  graph?: PipelineGraphDraft,
): VersionWriteRequest {
  return {
    name: version.name,
    agent_type: 'pipeline',
    // Baseline `useSaveVersion.js:96`: `instructions: !isFromPipeline ?
    // version_details.instructions : yamlCode` — the pipeline graph IS the
    // YAML, so a pipeline save writes the editor's live document.
    instructions: graph?.instructions ?? version.instructions ?? '',
    welcome_message: edits.welcomeMessage,
    ...selectLlmSettings(version, edits.llmSettings),
    conversation_starters: [...conversationStarters],
    variables: edits.variables.map((variable) => ({ name: variable.name, value: variable.value })),
    ...toPipelineMetaBody(version, edits),
    /*
     * ALWAYS sent, never gated on being non-empty: `UpdateVersion` reads the
     * key's PRESENCE (an absent key leaves the stored set alone, an empty
     * array clears it), so removing the last tag has to reach the wire. The
     * placeholder id `AgentTagEditor` gives a tag the user just typed is
     * stripped — the server matches by name and a negative id would be
     * fiction on the wire.
     */
    tags: edits.tags.map((tag) => ({
      ...(tag.id > 0 ? { id: tag.id } : {}),
      name: tag.name,
      ...(tag.data === null || tag.data === undefined ? {} : { data: tag.data }),
    })),
    ...(graph?.pipelineSettings === undefined ? {} : { pipeline_settings: { ...graph.pipelineSettings } }),
  };
}

/**
 * The generated `ApplicationVersionDetail` (snake_case GET-response shape)
 * -> `features/pipelines`' `ConfigurationTabProps['versionDetails']`
 * (`ChatPipelineVersionDetails`, `lib/hooks/usePipelineChat.hooks.ts`'s own
 * type) — needed to mount the real `ConfigurationTab` in `EditPipeline.tsx`
 * (this unit's own composition-gap fix; see that page's doc comment).
 *
 * Field-for-field compatible for everything both shapes carry (`llm_settings`/
 * `tools`/`variables`/`conversation_starters` all structurally match the
 * generated types already); `internal_tools` is read off `version.meta` as a
 * raw passthrough record — same `metaRecord['internal_tools']` treatment
 * `toVersionDraft` above already gives it, because the generated `VersionMeta`
 * zod schema has no typed `internal_tools` field of its own (verified: no
 * such key in `shared/api/generated/model/versionMeta.zod.ts`).
 * `exactOptionalPropertyTypes` forces every possibly-`undefined` field to be
 * conditionally spread rather than assigned `undefined` directly (this
 * codebase's own established convention, see `AgentEditor.tsx`'s doc comment
 * for the identical situation). `llm_settings` needs the same per-KEY
 * treatment (`buildChatLlmSettings` below), not just per-field: the
 * generated `LlmSettings` type's own optional keys (`model_name`, etc.) are
 * each individually possibly-`undefined`-valued under
 * `exactOptionalPropertyTypes`, so spreading the whole object verbatim
 * fails `tsc` (confirmed directly: TS2375) even though `version.llm_settings`
 * itself is conditionally spread at the top level.
 */
type ChatVersionDetails = NonNullable<ConfigurationTabProps['versionDetails']>;
type ChatLlmSettings = NonNullable<ChatVersionDetails['llm_settings']>;

/** See `toChatPipelineVersionDetails`'s own doc comment for why this can't just be `{ ...llmSettings }`. */
function buildChatLlmSettings(llmSettings: NonNullable<ApplicationVersionDetail['llm_settings']>): ChatLlmSettings {
  return {
    ...(llmSettings.model_name !== undefined ? { model_name: llmSettings.model_name } : {}),
    ...(llmSettings.model_project_id !== undefined ? { model_project_id: llmSettings.model_project_id } : {}),
    ...(llmSettings.temperature !== undefined ? { temperature: llmSettings.temperature } : {}),
    ...(llmSettings.max_tokens !== undefined ? { max_tokens: llmSettings.max_tokens } : {}),
  };
}

/** Split out purely to keep `toChatPipelineVersionDetails`'s own cyclomatic complexity under this codebase's oxlint gate. */
function buildDefinedVersionContentFields(
  version: ApplicationVersionDetail,
): Pick<ChatVersionDetails, 'instructions' | 'welcome_message' | 'variables' | 'tools'> {
  return {
    ...(version.instructions !== undefined ? { instructions: version.instructions } : {}),
    ...(version.welcome_message !== undefined ? { welcome_message: version.welcome_message } : {}),
    ...(version.variables !== undefined ? { variables: version.variables } : {}),
    ...(version.tools !== undefined ? { tools: version.tools } : {}),
  };
}

/** Split out purely to keep `toChatPipelineVersionDetails`'s own cyclomatic complexity under this codebase's oxlint gate. */
function buildDefinedVersionChatFields(
  version: ApplicationVersionDetail,
): Pick<ChatVersionDetails, 'agent_type' | 'conversation_starters' | 'llm_settings'> {
  return {
    ...(version.agent_type !== undefined ? { agent_type: version.agent_type } : {}),
    ...(version.conversation_starters !== undefined ? { conversation_starters: version.conversation_starters } : {}),
    ...(version.llm_settings !== undefined ? { llm_settings: buildChatLlmSettings(version.llm_settings) } : {}),
  };
}

export function toChatPipelineVersionDetails(version: ApplicationVersionDetail | undefined): ConfigurationTabProps['versionDetails'] {
  if (!version) return undefined;

  const metaRecord: Record<string, unknown> = version.meta ?? {};
  const iconMeta = metaRecord['icon_meta'];
  const internalToolsRaw = metaRecord['internal_tools'];
  const internalTools = Array.isArray(internalToolsRaw)
    ? internalToolsRaw.filter((entry): entry is string => typeof entry === 'string')
    : undefined;

  return {
    id: version.id,
    ...buildDefinedVersionContentFields(version),
    ...buildDefinedVersionChatFields(version),
    meta: {
      ...(iconMeta !== undefined ? { icon_meta: iconMeta } : {}),
      ...(internalTools !== undefined ? { internal_tools: internalTools } : {}),
    },
  };
}

/**
 * One entry of the pipeline editor's version dropdown. Structurally
 * `features/agents`' own `AgentPipelineVersionOption` (that type is
 * intra-slice and not on `features/agents`' 20/20 curated public API, so it
 * is matched structurally rather than imported — the same call
 * `features/agents/index.ts` already makes for `AgentEditorDeps`).
 */
export interface EditPipelineVersionOption {
  readonly id: number;
  readonly name: string;
  readonly created_at?: string | undefined;
  readonly status?: string | undefined;
  readonly is_default?: boolean | undefined;
}

/**
 * `ApplicationVersionSummary[]` (the detail response's `versions[]`) -> the
 * dropdown's options. `id` is narrowed to a NUMBER for the same reason
 * `pages/agents/lib/editApplicationMappers.ts`'s twin documents: the
 * selector compares it against `applicationVersionId` with `===`, and a
 * string on one side means the tick never renders.
 */
export function toVersionOptions(versions: readonly ApplicationVersionSummary[]): EditPipelineVersionOption[] {
  return versions.map((version) => ({
    id: Number(version.id),
    name: version.name,
    created_at: version.created_at,
    status: version.status,
    /*
     * The server's default-version flag, carried through so the pipeline
     * editor's version bar marks the default on first render like the agent
     * one does. Read off the wire rather than off the generated type: the Go
     * handler emits `versions[].is_default` but `ApplicationVersionSummary`
     * is generated from `api/openapi/v2.yaml`, which another stream owns —
     * see `pages/agents/lib/editApplicationMappers.ts`'s `readIsDefault` for
     * the full note, duplicated here rather than imported for the same
     * `no-sideways-*` reason this file already duplicates the option type.
     */
    is_default: (version as { readonly is_default?: unknown }).is_default === true,
  }));
}

/**
 * The body a pipeline "Save As Version" POST clones onto the new version
 * (`name` excluded — `SaveNewVersionButton`'s own dialog supplies it, and it
 * is the one field `CreateVersion` hard-requires).
 *
 * Read against `CreateVersion`/`versionFromBody`
 * (`services/elitea-main/internal/api/v2/applications/handler.go:496-525,
 * 785-800`) rather than against the schema, and it differs from the agents
 * twin on two keys that matter here:
 *
 *  - **`agent_type` is pinned to `'pipeline'`, never cloned.** `insertVersion`
 *    (`internal/infra/db/repos/applications.go:493-496`) substitutes
 *    `defaultAgentType` — the literal `"openai"` (:29) — for an empty
 *    `agent_type`. A save-as-version that omitted the key would mint an
 *    OPENAI AGENT out of a pipeline: the same rows, run by the wrong
 *    executor. `toVersionDraft` above pins it for the same reason.
 *  - **`meta` IS sent.** `features/agents/model/useSaveNewVersion.ts`'s doc
 *    comment states that "`meta` is NOT read from the request at all; the
 *    handler builds its own `meta` from a hardcoded `step_limit: 25`". That
 *    is no longer true of the handler it cites: `versionFromBody` reads
 *    `vBody["meta"]` and only DEFAULTS `step_limit` when the caller sent
 *    none (handler.go:504-510). Dropping the key would silently reset a
 *    pipeline's `step_limit` to 25 and lose its `internal_tools` on every
 *    save-as-version — the two `meta` fields `toVersionDraft` already
 *    round-trips on the ordinary Save. (The agents mapper still omits it;
 *    fixing that is a change to a page this unit does not own.)
 *
 * `instructions` is the version's STORED graph, not the live canvas. That is
 * deliberate and is only half the story: `versionFromBody` reads no
 * `pipeline_settings` key at all and `insertVersion`'s column list does not
 * carry it, so the POST cannot persist the laid-out geometry no matter what
 * it is given. `lib/carryPipelineGraphToVersion.ts` follows the create with
 * the PUT that CAN write both, so the live graph reaches the new version
 * through one mechanism rather than half through each.
 */
export function toNewPipelineVersionBody(
  version: ApplicationVersionDetail,
  conversationStarters: readonly string[],
  edits: EditPipelineVersionFields,
): Omit<VersionWriteRequest, 'name'> {
  return {
    agent_type: 'pipeline',
    instructions: version.instructions ?? '',
    /*
     * The LIVE welcome message, model, modules, step limit and variables —
     * not the stored ones. Taking a version used to clone the server's copy
     * of every one of them, so an author who edited the form and then reached
     * for "Save As Version" got a version without the edit they had just
     * made. `tags` stays out: `versionFromBody` reads no `tags` key, so only
     * the PUT writes them (`handler.go:532-534`).
     */
    welcome_message: edits.welcomeMessage,
    ...selectLlmSettings(version, edits.llmSettings),
    conversation_starters: [...conversationStarters],
    variables: edits.variables.map((variable) => ({ name: variable.name, value: variable.value })),
    ...toPipelineMetaBody(version, edits),
  };
}
