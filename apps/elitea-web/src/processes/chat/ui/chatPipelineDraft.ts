/**
 * The draft the chat editor's agent/pipeline form edits, and its conversions
 * to and from the stored version. Split out of `useChatPipelineConfig.tsx`,
 * which owns the queries, the save and the dirty state.
 */
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

/**
 * The placeholder id a tag typed in the editor carries.
 *
 * `entities/tag`'s `Tag` requires `id` and `data`, and a tag the user has just
 * typed has neither — the server matches by NAME on write
 * (`VersionWriteRequest.tags`, api/openapi/v2.yaml), which is why
 * `AgentTagEditor`'s own doc comment already describes a negative placeholder
 * for exactly this case. Every tag this hook rebuilds from stored names gets
 * one, since the draft keeps names and not rows.
 */
export const TAG_PLACEHOLDER_ID = -1;

interface DraftVariable {
  readonly name: string;
  readonly value: string;
}

/**
 * The draft `CreateAgentForm` reads and the save body writes. Its field set is
 * the INTERSECTION of what that form renders and what the version PUT (plus
 * the application PATCH `useSaveVersion` folds in) can actually store, so
 * every control on screen corresponds to something Save persists.
 */
export interface ChatPipelineDraft {
  readonly name: string;
  readonly description: string;
  readonly version_details: {
    readonly id?: number | undefined;
    readonly instructions?: string | undefined;
    readonly welcome_message?: string | undefined;
    readonly conversation_starters?: readonly string[] | undefined;
    readonly tags?: readonly string[] | undefined;
    readonly variables?: readonly DraftVariable[] | undefined;
    readonly meta?: { readonly step_limit?: number | undefined } | undefined;
  };
}
/** `tags` reaches the browser as objects and the form's editor works in objects; the write body wants names. Kept as names in the draft so there is one representation to reason about. */
function tagNames(version: ApplicationVersionDetail | undefined): readonly string[] {
  const tags: unknown = version?.tags;
  if (!Array.isArray(tags)) return [];
  return tags
    .map((tag) => (typeof tag === 'string' ? tag : (tag as { readonly name?: unknown })?.name))
    .filter((name): name is string => typeof name === 'string' && name.trim() !== '');
}

function draftVariables(version: ApplicationVersionDetail | undefined): readonly DraftVariable[] {
  const variables: unknown = version?.variables;
  if (!Array.isArray(variables)) return [];
  return variables.map((variable) => {
    const row = variable as { readonly name?: unknown; readonly value?: unknown };
    return {
      name: typeof row.name === 'string' ? row.name : '',
      value: typeof row.value === 'string' ? row.value : '',
    };
  });
}

function draftStarters(version: ApplicationVersionDetail | undefined): readonly string[] {
  const starters: unknown = version?.conversation_starters;
  if (!Array.isArray(starters)) return [];
  return starters.filter((entry): entry is string => typeof entry === 'string');
}

function draftStepLimit(version: ApplicationVersionDetail | undefined): number | undefined {
  const meta: Record<string, unknown> = (version?.meta as Record<string, unknown> | undefined) ?? {};
  const limit = meta['step_limit'];
  return typeof limit === 'number' ? limit : undefined;
}

/**
 * Applies one `setFieldValue(path, value)` write.
 *
 * Only the two path depths the shared form emits are handled — a top-level
 * key and one `version_details.*` key — and an unknown path is DROPPED rather
 * than stored under a made-up key. Silently storing it would put a value in
 * the draft that the save body never reads, which is how a control comes to
 * look like it works.
 */
export function applyFieldChange(draft: ChatPipelineDraft, path: string, value: unknown): ChatPipelineDraft {
  if (path === 'name') return { ...draft, name: typeof value === 'string' ? value : '' };
  if (path === 'description') return { ...draft, description: typeof value === 'string' ? value : '' };
  if (!path.startsWith('version_details.')) return draft;
  const key = path.slice('version_details.'.length);
  if (key.includes('.')) return draft;
  return { ...draft, version_details: { ...draft.version_details, [key]: value } };
}

export const EMPTY_DRAFT: ChatPipelineDraft = { name: '', description: '', version_details: {} };

/** The stored version and application, as the form's draft. Its own function for the oxlint complexity budget (12). */
export function seedDraft(
  version: ApplicationVersionDetail,
  versionId: number | undefined,
  application: { readonly name?: string; readonly description?: string } | undefined,
): ChatPipelineDraft {
  return {
    name: application?.name ?? '',
    description: application?.description ?? '',
    version_details: {
      ...(versionId !== undefined ? { id: versionId } : {}),
      instructions: typeof version.instructions === 'string' ? version.instructions : '',
      welcome_message: typeof version.welcome_message === 'string' ? version.welcome_message : '',
      conversation_starters: draftStarters(version),
      tags: tagNames(version),
      variables: draftVariables(version),
      meta: { step_limit: draftStepLimit(version) },
    },
  };
}

/** The draft in version-write shape. Own function for the same budget reason. */
export function toVersionBody(draft: ChatPipelineDraft, versionName: string | undefined, storedMeta?: Record<string, unknown>) {
  const details = draft.version_details;
  return {
    name: versionName ?? 'latest',
    instructions: details.instructions ?? '',
    welcome_message: details.welcome_message ?? '',
    conversation_starters: [...(details.conversation_starters ?? [])],
    variables: (details.variables ?? []).map((variable) => ({ name: variable.name, value: variable.value })),
    tags: (details.tags ?? []).map((name) => ({ name })),
    meta: { ...storedMeta, step_limit: details.meta?.step_limit ?? 25 },
  };
}
