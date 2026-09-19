import type { ApplicationCreationInput, ApplicationVersionDraft } from '@/entities/application-form';
import type { VersionSummary } from '@/entities/version';
import { toAgentLlmSettings, toLlmSettingsBody, type AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type {
  ApplicationDetail,
  ApplicationVersionDetail,
  ApplicationVersionSummary,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import type { EditApplicationVersionFields } from './useEditApplicationVersionFields';

/**
 * Pure mapping helpers for `EditApplication.tsx` (this unit, A1g), split
 * into their own file purely to keep that page file under the §3.5 400-line
 * budget — no behavioural reason, every function here is exercised only
 * from that page.
 */

export const EMPTY_FORM_VALUES: ApplicationCreationInput = {
  name: '',
  description: '',
  version_details: { conversation_starters: [] },
};

/**
 * Generated `ApplicationVersionSummary[]` (snake_case: `agent_type`,
 * `created_at`) -> `entities/version`'s `VersionSummary[]` (camelCase) —
 * needed only to satisfy this unit's own `useIsVersionNotFound`'s parameter
 * type; that hook's actual predicate only ever reads `.id`, but TypeScript
 * checks the full declared shape regardless of what a function body uses.
 */
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
 * Generated `ApplicationVersionSummary[]` -> the dropdown-option shape
 * `features/agents`' `AgentVersionControls` (and, behind it,
 * `AgentPipelineVersionSelector`) takes. Distinct from `toVersionSummaries`
 * above, which produces `entities/version`'s camelCase `VersionSummary` for
 * the 404 check — the selector was ported against the baseline's raw
 * snake_case `applicationData.versions[]` entry and reads `created_at`
 * directly (`features/agents/lib/types.ts`'s `AgentPipelineVersionOption`).
 * Both mappings are one-liners over the same source; converging them would
 * mean re-shaping one consumer's contract for no behavioural gain.
 *
 * `EditApplicationVersionOption` is declared here rather than imported from
 * `features/agents` because that slice's curated public API (§3.3, ≤20
 * symbols) is full and this page does not need a second slot spent on a
 * five-field record: TypeScript checks the two structurally at the
 * `<AgentVersionControls versions={…}/>` call site in `EditApplication.tsx`,
 * so any drift in `AgentPipelineVersionOption` fails the build there rather
 * than passing silently.
 */
export interface EditApplicationVersionOption {
  readonly id: number;
  readonly name: string;
  readonly created_at?: string | undefined;
  readonly status?: string | undefined;
  readonly is_default?: boolean | undefined;
  readonly author?: { readonly id?: string; readonly email?: string; readonly name?: string } | undefined;
}

/**
 * `versions[].is_default`, read off the wire rather than off the generated
 * type.
 *
 * The Go handler emits it (`applications/handler.go`'s `getVersions`, derived
 * from `applications.meta.default_version_id`), but
 * `ApplicationVersionSummary` is generated from `api/openapi/v2.yaml`, which
 * another stream owns — editing that spec ripples through two codegens and
 * several pinned gates — so the field is on the response before it is on the
 * type. A narrow, single-purpose read here is the honest way to say that:
 * it names exactly which key is being trusted, and `=== true` means a wire
 * that stops sending it degrades to "not the default" rather than to a
 * truthiness accident. Delete this helper and read `version.is_default`
 * directly once the schema carries it.
 */
function readIsDefault(version: ApplicationVersionSummary): boolean {
  return (version as { readonly is_default?: unknown }).is_default === true;
}

/**
 * `versions[].author`, read off the wire the same narrow-cast way
 * `readIsDefault` above reads `is_default` — see that function's own doc
 * comment, and `AgentPipelineVersionOption.author`'s, for why this rides on
 * the response ahead of the generated schema.
 */
function readAuthor(version: ApplicationVersionSummary): EditApplicationVersionOption['author'] {
  const author = (version as { readonly author?: { readonly id?: string; readonly email?: string; readonly name?: string } }).author;
  return author === undefined ? undefined : author;
}

export function toVersionOptions(
  versions: readonly ApplicationVersionSummary[],
): EditApplicationVersionOption[] {
  return versions.map((version) => ({
    // `ApplicationVersionSummary.id` is "numeric id serialized as string
    // (strconv.Itoa)" per the generated schema's own description, while the
    // selector's option id (and its `applicationVersionId` comparison) is a
    // number — the same `Number(version.id)` narrowing `useEditApplicationForm`
    // already applies to `activeVersion.id`. Both sides of the "is this the
    // selected version" check must be the same primitive or the tick never
    // renders and the trigger falls back to the first option's label.
    id: Number(version.id),
    name: version.name,
    created_at: version.created_at,
    status: version.status,
    is_default: readIsDefault(version),
    author: readAuthor(version),
  }));
}

/**
 * The active version's own fields, as the body a "Save As Version" POST
 * clones onto the new version (`name` excluded — `SaveNewVersionButton`'s
 * dialog supplies it). Old app: `SaveNewVersionButton.jsx` sends
 * `{...values.version_details, id: undefined, name: newVersion}` — i.e. the
 * CURRENTLY EDITED form state, not the server's last-saved copy, which is
 * why `conversationStarters` is threaded in from the live form here rather
 * than read off `version`.
 *
 * #307: `edits` threads the OTHER live version fields in for exactly the
 * same reason. While those fields were routed nowhere they could not
 * diverge from the server's copy, so reading them off `version` was
 * harmless; now that they are editable, a "Save As Version" taken after
 * typing new instructions would have cloned the STORED instructions and
 * dropped the edit without saying so. Optional, so a caller with no live
 * editor state still gets the stored values.
 *
 * Only the keys the Go `CreateVersion` handler actually reads are sent
 * (`agent_type`/`instructions`/`welcome_message`/`llm_settings`/
 * `conversation_starters`/`variables`, handler.go:723-747) — see
 * `features/agents/model/useSaveNewVersion.ts`'s doc comment for the full
 * trace, including why `meta`/`tags`/`tools` are deliberately omitted
 * (the handler discards them).
 */
/**
 * The `llm_settings` key of the write body, or nothing at all — a separate
 * function both to keep `toVersionWriteBody`'s own cyclomatic complexity
 * under this codebase's oxlint gate and because the choice needs explaining.
 *
 * The live edit wins over the stored copy, exactly as `instructions` does and
 * for the same #307 reason: a Save-As-Version taken after picking a different
 * model used to clone the OLD model onto the new version and say nothing.
 *
 * With NO edit the stored blob is forwarded VERBATIM rather than re-read
 * through `toAgentLlmSettings`. A stored `{model_name}` with no
 * `model_project_id` is a real, working shape — elitea-main's freeze fills
 * the project id in from the catalogue row it resolves
 * (`internal/application/agentexecution/tools.go`,
 * `resolveCurrentAgentModel`) — and the strict read would reject it and
 * silently move the cloned version onto a different model.
 */
function selectLlmSettings(
  version: ApplicationVersionDetail,
  edited: AgentLlmSettings | undefined,
): Pick<VersionWriteRequest, 'llm_settings'> {
  if (edited !== undefined) return { llm_settings: toLlmSettingsBody(edited) };
  return version.llm_settings === undefined ? {} : { llm_settings: version.llm_settings };
}

/**
 * The `meta` blob both write paths send, MERGED over the version's stored
 * one rather than replacing it: the Go handler assigns the whole map it
 * receives (`applications/handler.go:826-828` on the PUT,
 * `versionFromBody` on the POST), so sending `{step_limit}` alone would
 * drop `internal_tools` and every other key the version already carries.
 *
 * Shared by `toVersionWriteBody` and `toVersionSaveBody` so a Save and a
 * Save-As-Version cannot disagree about it — which is the same reason those
 * two already share their field selection.
 */
function toVersionMetaBody(
  version: ApplicationVersionDetail,
  edits?: EditApplicationVersionFields,
): Pick<VersionWriteRequest, 'meta'> {
  // `variables` is DROPPED from the stored blob, and this is load-bearing.
  //
  // Both handlers fold the body's TOP-LEVEL `variables` list into `meta`
  // themselves, so a copy carried inside `meta` can only ever contradict the
  // authoritative one — and on the create path it wins, because
  // `versionFromBody` folds only when the top-level list is NON-EMPTY
  // (`applications/handler.go:509-511`). Forwarding it there resurrected
  // deleted variables: delete an agent's last variable, Save As Version, and
  // the new version was inserted with `meta.variables` still holding it while
  // `application_variables` held no row. `agent_chat.sql` COALESCEs the empty
  // table onto `meta -> 'variables'`, and the Rust worker applies the meta
  // copy LAST (`agents/variables.rs`), so the deleted value — a secret, in the
  // case that matters — was substituted into every turn while the editor
  // showed no variables at all.
  //
  // The update path folds on key PRESENCE and so was never exposed; dropping
  // the key is correct for both, because both rebuild it from the list.
  const { variables: _storedVariables, ...storedMeta } = (version.meta ?? {}) as Record<string, unknown>;
  if (edits === undefined) return { meta: { ...storedMeta } };
  return {
    meta: {
      ...storedMeta,
      ...(edits.stepLimit === undefined ? {} : { step_limit: edits.stepLimit }),
      /*
       * #307 — `internal_tools` is the Tools panel's internal-tool switches.
       * Always sent (not gated on being non-empty): turning the LAST one off
       * has to reach the wire, and an `undefined`-when-empty guard would make
       * exactly that one edit silently unsaveable.
       */
      internal_tools: [...edits.internalTools],
      /*
       * #898 — the Editor Notes field. Always sent, for the same reason
       * `internal_tools` is: clearing the box has to reach the wire, and an
       * `undefined`-when-empty guard would make exactly that edit
       * unsaveable. It travels INSIDE `meta` because that is where the field
       * is stored on both sides (pylon's elitea_issues #5410 decision, and
       * `notesMetaKey` in the Go handler) — there is no `notes` column.
       */
      notes: edits.notes,
    },
  };
}

export function toVersionWriteBody(
  version: ApplicationVersionDetail,
  conversationStarters: readonly string[],
  edits?: EditApplicationVersionFields,
): Omit<VersionWriteRequest, 'name'> {
  return {
    ...(version.agent_type === undefined ? {} : { agent_type: version.agent_type }),
    instructions: edits?.instructions ?? version.instructions ?? '',
    welcome_message: edits?.welcomeMessage ?? version.welcome_message ?? '',
    ...selectLlmSettings(version, edits?.llmSettings),
    conversation_starters: [...conversationStarters],
    variables: (edits?.variables ?? version.variables ?? []).map((variable) => ({
      name: variable.name ?? '',
      value: variable.value ?? '',
    })),
    /*
     * `meta` REACHES THE CREATE PATH — the comment that used to say otherwise
     * here was wrong, and so is `VersionWriteRequest.tags`' generated
     * description ("the two create paths still ignore the key, exactly as
     * they ignore `meta`"). Traced end to end: `CreateVersion`
     * (`applications/handler.go:811`) calls `versionFromBody`, which reads
     * `vBody["meta"]` (`:504`) and only DEFAULTS `step_limit` when the key is
     * absent; `insertVersion` (`repos/applications.go:517-525`) then persists
     * it as the tenth column. Omitting it made every Save-As-Version reset
     * `step_limit` to the default and drop `internal_tools` — the two keys
     * the native Rust runtime gates admission on, so a cloned agent could
     * stop running.
     *
     * `tags` stays omitted: that half of the old comment is correct.
     * `versionFromBody` reads no `tags` key, so only the PUT writes them.
     */
    ...toVersionMetaBody(version, edits),
  };
}

/**
 * The body a normal Save PUT sends for the active version (#307). Shares
 * `toVersionWriteBody`'s field selection so a Save and a Save-As-Version
 * cannot drift apart, and adds the two keys only the UPDATE path carries:
 * the version's own `name` (unchanged — `UpdateVersion` writes whatever
 * `name` it is given, so omitting it is fine but sending the current one is
 * closer to the baseline's whole-object PUT) and `tags`, which
 * `toVersionWriteBody` omits because `versionFromBody` reads no `tags` key
 * on the create path. `meta` is NOT in that list: it comes through the
 * shared `toVersionMetaBody`, because the create path reads it too.
 *
 * #345 — `tags` is ALWAYS sent, never gated on being non-empty: removing
 * the last tag has to reach the wire, and the handler reads the key's
 * presence (an absent key leaves the stored set alone, an empty array
 * clears it). The placeholder id `AgentTagEditor` gives a tag the user
 * just typed is stripped: the server matches by name and a negative id
 * would be fiction on the wire.
 *
 * See `toVersionMetaBody` for why `meta` is merged rather than replaced.
 */
export function toVersionSaveBody(
  version: ApplicationVersionDetail,
  conversationStarters: readonly string[],
  edits: EditApplicationVersionFields,
): VersionWriteRequest {
  return {
    name: version.name,
    ...toVersionWriteBody(version, conversationStarters, edits),
    tags: edits.tags.map((tag) => ({
      ...(tag.id > 0 ? { id: tag.id } : {}),
      name: tag.name,
      ...(tag.data === null || tag.data === undefined ? {} : { data: tag.data }),
    })),
  };
}

/**
 * This page works directly with the GENERATED (snake_case) response types
 * rather than funnelling them through `entities/application`'s
 * `normaliseApplicationDetail`/`normaliseApplicationVersionDetail` — same
 * choice `features/apps/api/useAppDetail.ts` already made for the sibling
 * `useGetApplication` endpoint. Reason (real, verified, not a style
 * preference): the generated zod schemas mark optional fields
 * `field?: T | undefined` (zod's `.optional()` puts `undefined` INTO the
 * value-type union), while `entities/application`'s hand-authored `*Wire`
 * input interfaces declare the same fields `field?: T` (no explicit
 * `| undefined` in the value type). Under this project's
 * `exactOptionalPropertyTypes: true`, those two shapes are NOT assignable to
 * each other (confirmed directly: `tsc` rejects passing the generated
 * `ApplicationDetail`/`ApplicationVersionDetail` types straight into either
 * normaliser, TS2379, "Consider adding 'undefined' to the types of the
 * target's properties") — casting through would require a second, bespoke
 * re-shaping step for no benefit, since this page only ever reads a handful
 * of fields back out.
 */
export function applicationDetailDisplayName(detail: ApplicationDetail): string {
  return detail.name.trim() !== '' ? detail.name : 'Untitled';
}

export function toFormValues(
  detail: ApplicationDetail,
  version: ApplicationVersionDetail | undefined,
): ApplicationCreationInput {
  return {
    name: detail.name,
    description: detail.description,
    version_details: {
      conversation_starters: (version?.conversation_starters ?? []).filter(
        (entry): entry is string => typeof entry === 'string',
      ),
    },
  };
}

/**
 * The generated `ApplicationVersionDetail` (snake_case GET-response shape)
 * -> `ApplicationVersionDraft` (`entities/application-form`, the camelCase
 * shape `useSaveApplicationVersion` sends on write). `meta`'s
 * `step_limit`/`internal_tools` are read defensively from the opaque
 * passthrough blob with the SAME defaults `useCreateApplicationInitialValues`
 * seeds a brand-new draft with (`{ step_limit: 25, internal_tools: [] }`,
 * `entities/application-form/model/initialValues.ts:131`) when the existing
 * version's `meta` does not already carry them in the expected shape.
 *
 * **`internal_tools` falls back to EMPTY, not `['internal_mcp']`.** The
 * fallback here is more dangerous than a create-time default, because it
 * rewrites an agent that already works: a stored version with no
 * `internal_tools` key is admitted by the chat query's own COALESCE
 * (`services/elitea-main/internal/db/queries/agent_chat.sql:359-362` reads
 * `COALESCE(... -> 'internal_tools', '[]') IN ('[]', '["ask_user"]')`), so
 * such an agent answers turns today — but the old `['internal_mcp']`
 * fallback injected that name the first time a user saved ANY unrelated
 * edit, after which every send came back 422 and the native runtime refused
 * the name too (`services/elitea-worker-rust/src/agents/
 * internal_tools.rs:47-61`). An explicitly stored array is still returned
 * verbatim, `internal_mcp` included, so a deliberate opt-in round-trips
 * through the editor unchanged — losing a setting on load would be a worse
 * defect than the one this fixes.
 */
export function toVersionDraft(
  version: ApplicationVersionDetail,
  conversationStarters: readonly string[],
): ApplicationVersionDraft {
  const metaRecord: Record<string, unknown> = version.meta ?? {};
  const stepLimit = typeof metaRecord['step_limit'] === 'number' ? metaRecord['step_limit'] : 25;
  const internalToolsRaw = metaRecord['internal_tools'];
  const internalTools = Array.isArray(internalToolsRaw)
    ? internalToolsRaw.filter((entry): entry is string => typeof entry === 'string')
    : [];
  return {
    name: version.name,
    agentType: version.agent_type === 'pipeline' ? 'pipeline' : undefined,
    instructions: version.instructions ?? '',
    conversationStarters,
    variables: (version.variables ?? []).map((variable) => ({
      name: variable.name ?? '',
      value: variable.value ?? '',
    })),
    meta: { step_limit: stepLimit, internal_tools: internalTools },
    // Read back so a save round-trips the model the version already names.
    // `undefined` when the stored blob is `{}` — every version written before
    // the model picker existed — and `toVersionWriteRequest` then omits the
    // key, leaving that version on the catalogue-default fallback it works on
    // today rather than pinning it to a model this mapper guessed.
    llmSettings: toAgentLlmSettings(version.llm_settings),
    tags: (version.tags ?? [])
      .map((tag) => tag.name)
      .filter((name): name is string => typeof name === 'string'),
    tools: version.tools ?? [],
    // This page (A1g, the agents domain) never carries a pipeline's
    // node/edge layout — the pipelines domain (A2) has its own edit page.
    pipelineSettings: undefined,
  };
}
