import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import type { PipelineToolEntry } from '../ui/select/pipelineToolEntry.types';

/**
 * Closes the real plumbing gap `FlowWrapper.tsx`'s own module doc comment
 * names: `versionTools`/`llmSettings` reach `FlowWrapper`'s props now (see
 * `EditorPanel.tsx`), but the two real callers of `EditorPanel`
 * (`ConfigurationTab.tsx`, `PipelineEditorParts.tsx`'s `PipelineEditorBody`)
 * only have a version's WIRE-shaped `tools`/`llm_settings` in scope, not
 * `PipelineToolEntry[]`/`AiAssistantLlmSettings`. This file is the one place
 * both callers map from.
 *
 * `DEFAULT_TEMPERATURE`/`DEFAULT_MAX_TOKENS` moved here from
 * `ConfigurationTab.tsx` (still not ported to `shared/lib/`, see that file's
 * own remaining doc comment) so both callers apply the identical "no LLM
 * configured yet" fallback instead of one of them inventing its own.
 */
export const DEFAULT_TEMPERATURE = 0.6;
export const DEFAULT_MAX_TOKENS = -1;

/**
 * The Go-generated `VersionToolRef` zod schema (`shared/api/generated/model/
 * versionToolRef.zod.ts`) only commits to `id`/`tool_id`/`entity_type`/
 * `selected_tools`/`name`/`type`/`config`/`settings`/`author_id`/
 * `project_id` — its own doc comment: two DB row shapes merged into one
 * array, "only `id` is common to all". The richer toolkit metadata
 * `PipelineToolEntry`'s real consumers read (`ToolSelect.tsx`/
 * `ToolkitsSelect.tsx`/`LoopToolSelect.tsx` — `toolkit_name`/`description`/
 * `agent_type`/`meta.mcp`/a static `tools` name list) is exactly "the joined
 * elitea_tools payload" that schema's own doc comment says lands inside the
 * opaque, passthrough-marked `config`/`settings` blob (`ToolSettings` —
 * `zod.unknown()`) — NOT reliably at the top level. Reads both locations
 * defensively rather than assuming either one: the wire schema itself does
 * not commit to which endpoint populates which.
 */
interface RawVersionTool {
  readonly id?: string | number;
  readonly type?: string;
  readonly name?: string;
  readonly entity_type?: string;
  readonly selected_tools?: unknown;
  readonly toolkit_name?: unknown;
  readonly description?: unknown;
  readonly agent_type?: unknown;
  readonly meta?: { readonly mcp?: unknown } | null;
  readonly tools?: unknown;
  readonly config?: unknown;
  readonly settings?: unknown;
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function asBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}

function asToolNameList(value: unknown): readonly (string | { readonly name?: string })[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.filter((entry): entry is string | { readonly name?: string } => typeof entry === 'string' || (typeof entry === 'object' && entry !== null));
}

function asStringArray(value: unknown): readonly string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.filter((entry): entry is string => typeof entry === 'string');
}

/** Narrows an unknown `tools[]` entry (or its `config`/`settings` blob) to a readable `RawVersionTool` -- split out purely to keep its two callers' own complexity under the §3.5 budget (12). */
function asRawVersionTool(value: unknown): RawVersionTool {
  return value && typeof value === 'object' ? value : {};
}

/** The opaque `config`/`settings` blob, narrowed just enough to read the field names above off it — never trusted beyond that. */
function opaqueToolBlob(tool: RawVersionTool): RawVersionTool {
  return asRawVersionTool(tool.config ?? tool.settings);
}

/** Resolved fields for one `tools[]` entry -- split out of {@link toPipelineToolEntry} purely to keep that function's own complexity under the §3.5 budget (12); this one, the field-by-field top-level-vs-opaque-blob resolution, is the half with all the `??` fallbacks. */
interface ResolvedToolFields {
  readonly id: string | undefined;
  readonly type: string | undefined;
  readonly name: string | undefined;
  readonly toolkitName: string | undefined;
  readonly description: string | undefined;
  readonly agentType: string | undefined;
  readonly toolNames: readonly (string | { readonly name?: string })[] | undefined;
  readonly mcp: boolean | undefined;
  readonly selectedTools: readonly string[] | undefined;
}

/**
 * An EMPTY mapping-level `selected_tools` is "no narrowing", not "no tools":
 * `entity_tool_mapping.selected_tools` defaults to `[]` for every attach that
 * names no selection, and the runtime's own read (`agent_chat.sql`) only lets
 * the mapping win when it is non-empty. Letting `[]` win here (a `??` on a
 * non-nullish empty array) emptied every node's tool list for a toolkit that
 * had its tools enabled on its own page (#2003). When both are present the
 * mapping narrows the toolkit's own list, falling back to the mapping when
 * the two do not intersect (same as the runtime).
 */
function resolveSelectedTools(mapping: readonly string[] | undefined, toolkit: readonly string[] | undefined): readonly string[] | undefined {
  const narrowed = mapping !== undefined && mapping.length > 0 ? mapping : undefined;
  if (narrowed === undefined) return toolkit;
  if (toolkit === undefined || toolkit.length === 0) return narrowed;
  const intersection = narrowed.filter(name => toolkit.includes(name));
  return intersection.length > 0 ? intersection : narrowed;
}

function resolveToolFields(raw: unknown): ResolvedToolFields {
  const tool = asRawVersionTool(raw);
  const blob = opaqueToolBlob(tool);
  return {
    id: tool.id === undefined ? undefined : String(tool.id),
    type: tool.type,
    name: tool.name,
    toolkitName: asString(tool.toolkit_name) ?? asString(blob.toolkit_name),
    description: asString(tool.description) ?? asString(blob.description),
    agentType: asString(tool.agent_type) ?? asString(blob.agent_type) ?? tool.entity_type,
    toolNames: asToolNameList(tool.tools) ?? asToolNameList(blob.tools),
    mcp: asBoolean(tool.meta?.mcp) ?? asBoolean(blob.meta?.mcp),
    selectedTools: resolveSelectedTools(asStringArray(tool.selected_tools), asStringArray(blob.selected_tools)),
  };
}

/**
 * `readonly field?: T` (no `| undefined`) on every `PipelineToolEntry`
 * field, PLUS `readonly` itself, means `exactOptionalPropertyTypes` rejects
 * both `{field: undefined}` and post-construction assignment -- built up
 * via one object literal with a conditional spread per optional field
 * (same idiom `aiContentGenerationStreaming.helpers.ts`'s own
 * `buildLlmSettings` already uses for `integration_uid`), not a generic
 * `assignDefined(obj, key, value)` helper (writing through a generic
 * `keyof T` index is exactly the pattern TypeScript's checker cannot
 * verify soundly under strict mode, `readonly` aside).
 */
function toPipelineToolEntry(raw: unknown): PipelineToolEntry {
  const { id, type, name, toolkitName, description, agentType, toolNames, mcp, selectedTools } = resolveToolFields(raw);
  return {
    ...(id !== undefined ? { id } : {}),
    ...(type !== undefined ? { type } : {}),
    ...(name !== undefined ? { name } : {}),
    ...(toolkitName !== undefined ? { toolkit_name: toolkitName } : {}),
    ...(description !== undefined ? { description } : {}),
    ...(agentType !== undefined ? { agent_type: agentType } : {}),
    ...(toolNames !== undefined ? { tools: toolNames } : {}),
    ...(mcp !== undefined ? { meta: { mcp } } : {}),
    ...(selectedTools !== undefined ? { settings: { selected_tools: selectedTools } } : {}),
  };
}

/** A version's `tools[]` (wire-shaped, `readonly unknown[]` in `ChatPipelineVersionDetails`, `readonly VersionToolRef[]` in `ApplicationVersionDetail`) -> `FlowWrapperProps['versionTools']`. */
export function toPipelineToolEntries(tools: readonly unknown[] | undefined): readonly PipelineToolEntry[] {
  return (tools ?? []).map(toPipelineToolEntry);
}

/** The subset of a version's wire `llm_settings` (`LlmSettings`/`ChatPipelineVersionDetails['llm_settings']`) this mapping reads — both decline to require any field. */
export interface WireLlmSettingsLike {
  readonly model_name?: string | undefined;
  readonly temperature?: number | undefined;
  readonly max_tokens?: number | undefined;
}

/**
 * A version's `llm_settings` -> `AiAssistantLlmSettings`. NOT a bare
 * pass-through: `AiAssistantLlmSettings.model_name`/`temperature`/
 * `max_tokens` are required, while every field here is optional on the
 * wire. `model_name` defaults to `''` (matching `readGenerationBlocker`'s
 * own `!modelName` check, `aiContentGenerationStreaming.helpers.ts` — an
 * empty name legitimately means "no LLM configured for this pipeline
 * version yet", not a bug to paper over). `temperature`/`max_tokens` get
 * the same `DEFAULT_TEMPERATURE`/`DEFAULT_MAX_TOKENS` fallback
 * `ConfigurationTab.tsx` already resolves for `ChatPanel`'s settings.
 * `integration_uid` is omitted outright (not `undefined` --
 * `exactOptionalPropertyTypes`): no wire shape in this app carries it.
 */
export function toAiAssistantLlmSettings(llmSettings: WireLlmSettingsLike | undefined): AiAssistantLlmSettings {
  return {
    model_name: llmSettings?.model_name ?? '',
    temperature: llmSettings?.temperature ?? DEFAULT_TEMPERATURE,
    max_tokens: llmSettings?.max_tokens ?? DEFAULT_MAX_TOKENS,
  };
}
