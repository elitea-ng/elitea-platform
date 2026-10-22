import type { ToolFieldValue, ToolPropertySchemaItems, ToolSchema } from './types';

/**
 * Ported from `apps/elitea-ui/src/common/getToolInitialValueBySchema.js`'s
 * `getPropValue`/`getArrayOptions` (144 lines). NOT promoted anywhere in
 * this worktree (verified: `grep -rln getPropValue src` — zero hits outside
 * this file at port time) and not owned by any other Wave-2 sub-unit's
 * mission brief, so ported locally here per the workflow's own "port it
 * yourself, locally" instruction. Only `getPropValue`/`getArrayOptions` are
 * ported — `ToolBase.jsx`'s one real call site (`ToolBase.jsx:151-164`,
 * the `shouldInitRequiredFields` effect) never reaches
 * `genInitialToolSettings`/`getToolInitialValueBySchema` (the baseline's
 * other two exports, used only by a separate "new tool" seeding path that
 * is not part of this sub-unit's owned files).
 *
 * DISCLOSED SCOPE REDUCTION: the baseline's `defaultVectorStorage` (gone
 * with the pgvector configuration type)/`defaultEmbeddingModel`/
 * `defaultImageGenerationModel` parameters are dropped — `ToolBase.jsx`'s own call site never supplies them (they
 * default to `{}`/`''`/`''` there too), so the `configuration_types`/
 * `embedding_model`/`image_generation_model` branches below return their
 * baseline-equivalent fallback (`null`/`''`) rather than a caller-supplied
 * default.
 */

/** Ported verbatim from the baseline's `getArrayOptions` — resolves a `#/a/b/c` JSON-Pointer-shaped `itemRef` against `schema` and returns its `enum`. */
export function getArrayOptions(schema: ToolSchema | undefined, itemRef: string): readonly string[] {
  const paths = itemRef.replace('#/', '').split('/');
  let prop: unknown = schema;
  for (const path of paths) {
    if (prop && typeof prop === 'object') {
      prop = (prop as Record<string, unknown>)[path];
    } else {
      prop = undefined;
    }
  }
  const resolved = prop as { enum?: readonly string[] } | undefined;
  return resolved?.enum ?? [];
}

export interface GetPropValueParams {
  readonly schema: ToolSchema | undefined;
  readonly name: string;
  readonly type: string | undefined;
  readonly format?: string | undefined;
  readonly defaultValue?: unknown;
  readonly prefillValue?: unknown;
  readonly items?: ToolPropertySchemaItems | undefined;
  readonly configuration_types?: readonly string[] | undefined;
}

/** A single `getPropValue` type-branch handler — see `TYPE_HANDLERS` below. */
type TypeHandler = (params: GetPropValueParams, effectiveDefault: unknown) => ToolFieldValue;

const handleString: TypeHandler = (params, effectiveDefault) => {
  if (params.format === 'password') return null;
  return (effectiveDefault as string | undefined) || '';
};

const handleInteger: TypeHandler = (_params, effectiveDefault) =>
  effectiveDefault !== undefined ? (effectiveDefault as number) : undefined;

/** `selected_tools`'s own array-default resolution (`items.enum`/`.const`/`.itemRef`) — split out of `handleArray` to keep it under the §3.5 complexity budget. */
function resolveSelectedToolsDefault(schema: ToolSchema | undefined, items: ToolPropertySchemaItems, effectiveDefault: unknown): ToolFieldValue {
  if (items.enum) return items.enum;
  if (items.const) return [items.const];
  if (items.itemRef) return getArrayOptions(schema, items.itemRef);
  return (effectiveDefault as readonly string[] | undefined) || [];
}

const handleArray: TypeHandler = (params, effectiveDefault) => {
  if (params.name !== 'selected_tools') {
    return (effectiveDefault as readonly string[] | undefined) || [];
  }
  if (!params.items) return (effectiveDefault as readonly string[] | undefined) || [];
  return resolveSelectedToolsDefault(params.schema, params.items, effectiveDefault);
};

const handleBoolean: TypeHandler = (_params, effectiveDefault) => Boolean(effectiveDefault);

const handleObject: TypeHandler = (_params, effectiveDefault) => (effectiveDefault as Readonly<Record<string, unknown>> | undefined) || {};

const handleModelReference: TypeHandler = (_params, effectiveDefault) => (effectiveDefault as string | undefined) || '';

const handleDefault: TypeHandler = (params, effectiveDefault) => {
  if (params.configuration_types) {
    return (effectiveDefault as ToolFieldValue) || null;
  }
  if (effectiveDefault === null) return null;
  return (effectiveDefault as string | undefined) || '';
};

/**
 * Ported verbatim from the baseline's `getPropValue`'s `switch (type)`
 * (its doc comment on `prefill_value` vs `default` preserved below), as a
 * dispatch table instead of a `switch` — the baseline's own `switch`, kept
 * literally, scored an `eslint(complexity)` of 25 (§3.5 budget: 12); a
 * lookup table plus one handler function per case (each independently
 * simple) keeps `getPropValue` itself at complexity 2 while preserving
 * every branch's real logic unchanged.
 */
const TYPE_HANDLERS: Readonly<Record<string, TypeHandler>> = {
  string: handleString,
  integer: handleInteger,
  array: handleArray,
  boolean: handleBoolean,
  object: handleObject,
  embedding_model: handleModelReference,
  image_generation_model: handleModelReference,
};

/**
 * `prefill_value` is a UI hint that doesn't affect Pydantic validation. This
 * allows fields to be truly required (in `schema.required`) while still
 * showing a sensible initial value in the form. Called once per missing
 * required field in `ToolBase.tsx`'s mount effect to synthesize an initial
 * value from the field's own schema entry.
 */
export function getPropValue(params: GetPropValueParams): ToolFieldValue {
  const effectiveDefault = params.prefillValue ?? params.defaultValue;
  const handler = params.type !== undefined ? TYPE_HANDLERS[params.type] : undefined;
  return (handler ?? handleDefault)(params, effectiveDefault);
}
