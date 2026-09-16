import type { ReactNode } from 'react';

/**
 * Shared types for the `ToolBase` family (unit A4c) — ported from the
 * untyped prop shapes threaded through
 * `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/ToolBase/{ToolBase,
 * ToolBaseProperty,ToolSection,ArrayFieldInput,ToolActionsSelector}.jsx`.
 * The baseline never declares these shapes (plain JS + PropTypes-free); this
 * file makes the real, observed shape explicit so every ported component
 * shares one definition instead of five independently-drifting inline ones.
 */

/** A settings-field value as this form ever assigns it. Mirrors the baseline's dynamically-typed `settings[k]`. */
export type ToolFieldValue =
  | string
  | number
  | boolean
  | null
  | undefined
  | readonly string[]
  | Readonly<Record<string, unknown>>;

/**
 * `editField` — the baseline's Formik `setFieldValue`-derived callback,
 * threaded through every `ToolBase*` component unchanged
 * (`ToolBase.jsx:36`, `ToolBaseProperty.jsx:36`, `ToolSection.jsx:20`,
 * `ArrayFieldInput.jsx:15`). This app has no Formik (`entities/toolkit`'s own
 * "no ambient form context" precedent, e.g. `ToolConfigurationForm.tsx`'s
 * doc comment) — the caller (out of this unit's scope) owns the real
 * react-hook-form `setValue`/`useFieldArray` machinery and supplies this as
 * a plain callback prop, same shape the baseline already used positionally.
 * Two real baseline call shapes are preserved: the 3-arg
 * `editField(path, parsedValue, true)` (`ToolBaseProperty.jsx:126`, the
 * object-field JSON-parse handler — the baseline never documents what the
 * boolean means beyond "truthy for this one call site"; kept as an opaque
 * `skipValidation` flag a caller may choose to honour or ignore), and the
 * 4-arg `editField(path, value, undefined, options)`
 * (`ToolBaseProperty.jsx:501,534`, `CredentialsSelect`'s/
 * `EmbeddingModelSelect`'s own `onSelectConfiguration`/`onSelectModel`
 * 2-arg callback forwarded straight through) — surfaced here as `options`
 * so `CredentialLikeFieldContext.onChange` (`ToolBaseProperty.tsx`) can
 * forward it honestly instead of inventing a 3-arg shape that drops it.
 */
export type EditToolField = (
  fieldPath: string,
  value: ToolFieldValue,
  skipValidation?: boolean,
  options?: unknown,
) => void;

/** A field's validation state: `false` (no error), `true` (required-but-empty, no message), or a message string (e.g. an integer-constraint violation). Mirrors `toolErrors[k]` across the baseline. */
type ToolErrorValue = boolean | string;
export type ToolErrors = Readonly<Record<string, ToolErrorValue>>;
export type SetToolErrors = (updater: (previous: ToolErrors) => ToolErrors) => void;

/** One `anyOf`/`oneOf` branch, or a top-level property schema when nested one level. */
interface ToolPropertySchemaBranch {
  readonly type?: string;
  readonly format?: string;
  readonly secret?: boolean;
  readonly default?: unknown;
  readonly exclusiveMinimum?: number;
  readonly minimum?: number;
  readonly exclusiveMaximum?: number;
  readonly maximum?: number;
}

export interface ToolPropertySchemaItems {
  readonly enum?: readonly string[];
  readonly const?: string;
  readonly itemRef?: string;
}

interface ToolPropertyVisibleWhen {
  readonly field: string;
  readonly value: unknown;
}

/**
 * One JSON-Schema property, as this form's own dynamic-field renderer reads
 * it — the union of every field any `ToolBase*` component destructures from
 * `v` across the 9-file baseline (`ToolBase.jsx`, `ToolBaseProperty.jsx`,
 * `ToolSection.jsx`). Not the full JSON-Schema spec — only the subset this
 * app's own backend schema generator (Pydantic model -> JSON schema) emits
 * and this renderer consumes.
 */
export interface ToolPropertySchema extends ToolPropertySchemaBranch {
  readonly title?: string;
  readonly description?: string;
  readonly enum?: readonly string[];
  readonly code_language?: string;
  readonly lines?: number | string;
  readonly anyOf?: readonly ToolPropertySchemaBranch[];
  readonly oneOf?: readonly ToolPropertySchemaBranch[];
  readonly max_toolkit_length?: number;
  readonly ui_component?: string;
  readonly visible_when?: ToolPropertyVisibleWhen;
  readonly placeholder?: string;
  readonly hidden?: boolean;
  readonly configuration?: boolean;
  readonly configuration_types?: readonly string[];
  readonly configuration_sections?: readonly string[];
  readonly section?: string;
  readonly options?: unknown;
  readonly items?: ToolPropertySchemaItems;
  readonly args_schemas?: Readonly<Record<string, unknown>>;
  /** Tool name → group id, served at `properties.selected_tools.tool_groups` (`internal/api/v2/toolkits/tool_groups.go`). Absent means "this type's tools are not classified" — see `ToolActionsSelector`'s own prop doc. */
  readonly tool_groups?: Readonly<Record<string, string>>;
  /** The served fixed group order that goes with `tool_groups`. */
  readonly tool_group_order?: readonly string[];
  readonly toolkit_filter?: unknown;
  readonly agent_filter?: unknown;
  readonly pipeline_filter?: unknown;
  readonly originalType?: string;
}

/** The tool/toolkit-type's whole JSON schema — `schema` throughout the baseline. */
export interface ToolSchema {
  readonly title?: string;
  readonly required?: readonly string[];
  readonly properties?: Readonly<Record<string, ToolPropertySchema | undefined>>;
}

/**
 * The in-progress tool/toolkit form value — `editToolDetail` throughout the
 * baseline (`ToolBase.jsx:28`). `settings` is intentionally
 * `Record<string, unknown>`, not further typed: its shape is exactly
 * `schema.properties`, which is per-toolkit-type and only known at runtime.
 */
export interface EditToolDetail {
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly description?: string;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly enableEditEliteaTitle?: boolean;
  readonly meta?: { readonly mcp_options?: Readonly<Record<string, unknown>> };
  readonly type?: string;
  /** Per-field preset option lists — `editToolDetail.options?.[k]`, `ToolBase.jsx:285`. */
  readonly options?: Readonly<Record<string, unknown>>;
  readonly id?: string;
}

export type SetEditToolDetail = (updater: (previous: EditToolDetail) => EditToolDetail) => void;

/**
 * A caller-injected slot rendered in place of a not-yet-portable or
 * cross-slice-forbidden widget. See `ToolBase.tsx`'s own module doc comment
 * for the full inventory of which baseline sub-component each slot replaces
 * and why (R-L1 `no-sideways-features`, or an intra-slice sibling not yet
 * landed).
 */
export type ToolSlotRenderer<TContext> = (context: TContext) => ReactNode;

/** `onCredentialReload` — threaded through unchanged from the baseline (`ToolBase.jsx:56`), an opaque caller-owned refresh callback for a credential list. */
export type OnCredentialReload = () => void;

/** `validationErrorMessages` — a caller-supplied field-key -> message override map (`ToolBase.jsx:54`). */
export type ValidationErrorMessages = Readonly<Record<string, string>>;

/**
 * The 7 baseline `ToolBaseProperty.jsx` branches that render a "smart"
 * picker backed by cross-domain data this app has no in-scope port for
 * today: `type === 'configuration'` (`CredentialsSelect`, `features/
 * credentials/ui` — cross-slice-forbidden, `no-sideways-features`) and the
 * 6 `@/components/*` legacy "smart select" components (`AgentSelect`,
 * `EmbeddingModelSelect`, `ImageGenerationModelSelect`, `LlmModelSelect`,
 * `ToolkitSelect` used twice, for `toolkit_reference`/agent/pipeline
 * references) — none promoted, none owned by this sub-unit's mission
 * brief. See `ToolBaseProperty.tsx`'s own module doc comment for the full
 * disclosure.
 */
export type CredentialLikeFieldKind =
  | 'configuration'
  | 'llm_model'
  | 'embedding_model'
  | 'image_generation_model'
  | 'toolkit_reference'
  | 'agent_reference'
  | 'pipeline_reference';

/** Everything a `renderCredentialLikeField` slot implementation needs — the union of what each of the 7 baseline branches read from its own closure. */
export interface CredentialLikeFieldContext {
  readonly kind: CredentialLikeFieldKind;
  readonly propertyKey: string;
  readonly schema: ToolPropertySchema;
  readonly label: string;
  readonly required: boolean;
  readonly disabled: boolean;
  readonly error: boolean;
  readonly helperText: string | undefined;
  readonly value: unknown;
  /** `options` is the baseline's 4th `editField` positional argument (`CredentialsSelect`'s/`EmbeddingModelSelect`'s own `onSelectConfiguration`/`onSelectModel` 2nd callback argument). */
  readonly onChange: (value: unknown, options?: unknown) => void;
  readonly specifiedProjectId: string | number | undefined;
  readonly presetOptions: unknown;
  readonly onCredentialReload: OnCredentialReload | undefined;
}

/**
 * The baseline's `uiComponent === 'openapi_spec'` branch
 * (`ToolBaseProperty.jsx:231-276`): a schema-text input (`ToolkitForm.
 * OpenAPISchemaInput`) composed with a derived actions table
 * (`ToolkitForm.OpenAPIActions`) that toggles `selected_tools` membership.
 * Neither sibling widget has landed in this worktree (both intra-slice,
 * owned by a different Wave-2 `toolkits` sub-unit) — delegated here as one
 * opaque slot rather than a forward `import` of files that do not exist yet
 * (would break this unit's own build), matching `AgentEditor.tsx`'s
 * established "not-yet-landed sibling -> slot" precedent.
 */
export interface OpenApiSpecFieldContext {
  readonly value: string;
  readonly onSchemaChange: (schemaText: string) => void;
  readonly selectedTools: readonly string[];
  readonly onSelectionChange: (toolName: string, enabled: boolean) => void;
  readonly error: boolean;
  readonly helperText: string | undefined;
  readonly disabled: boolean;
  /** The baseline's `OpenAPISchemaInput`'s own `setToolErrors` prop (`ToolBaseProperty.jsx:262`, `|| (() => {})`-guarded there) — `undefined` when the caller (`ToolBaseProperty.tsx`) was not given one. */
  readonly setToolErrors: SetToolErrors | undefined;
}
