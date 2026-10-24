import type { ToolPropertySchema, ToolSchema } from './types';

/**
 * Pure list-building helpers extracted from `ToolBase.tsx`'s four
 * near-identical field-rendering passes (priority / main / advanced /
 * bottom-of-form) to (a) stay under the §3.5 400-line/12-complexity budgets
 * and (b) not repeat the same filter predicate four times, a simplification
 * this port takes but the baseline (`ToolBase.jsx`) does not — the baseline
 * literally repeats the ~15-line filter/JSX block four times
 * (`ToolBase.jsx:246-289`, `292-351`, `364-404`, `441-484`).
 */

export interface ToolBaseFieldEntry {
  readonly key: string;
  readonly schema: ToolPropertySchema;
}

/** All of `schema.properties`, as ordered `[key, schema]` entries — `Object.entries` order, matching the baseline. */
export function schemaEntries(schema: ToolSchema | undefined): readonly ToolBaseFieldEntry[] {
  const properties = schema?.properties ?? {};
  const entries: ToolBaseFieldEntry[] = [];
  for (const [key, value] of Object.entries(properties)) {
    if (value) entries.push({ key, schema: value });
  }
  return entries;
}

/** Look up specific field keys, in the order `keys` lists them, skipping any not present in `schema.properties`. Used for `priorityFieldsOrder`/`fieldNeedToRenderAtBottom`/`advancedFields`, which are all "named field, specific order" lists in the baseline. */
export function pickSchemaFields(schema: ToolSchema | undefined, keys: readonly string[]): readonly ToolBaseFieldEntry[] {
  const properties = schema?.properties ?? {};
  const entries: ToolBaseFieldEntry[] = [];
  for (const key of keys) {
    const value = properties[key];
    if (value) entries.push({ key, schema: value });
  }
  return entries;
}

/** True when `key` is handled by a metadata section (`sectionProps`) or is the tools chip picker (`selected_tools`) — excluded from every plain-field pass, matching the baseline's repeated `sectionProps.includes(k) || k === 'selected_tools'` guard. */
export function isSectionOrToolsField(key: string, sectionProps: readonly string[]): boolean {
  return sectionProps.includes(key) || key === 'selected_tools';
}

/** The main-pass filter (`ToolBase.jsx:292-318`): exclude section/tools fields, priority fields, bottom fields, explicitly-excluded fields, and advanced fields. */
export function isMainPassField(
  key: string,
  sectionProps: readonly string[],
  priorityFieldsOrder: readonly string[],
  fieldNeedToRenderAtBottom: readonly string[],
  excludedFields: readonly string[],
  advancedFields: readonly string[],
): boolean {
  if (isSectionOrToolsField(key, sectionProps)) return false;
  if (priorityFieldsOrder.includes(key)) return false;
  if (fieldNeedToRenderAtBottom.includes(key)) return false;
  if (excludedFields.includes(key)) return false;
  if (advancedFields.includes(key)) return false;
  return true;
}

export function resolveRequired(key: string, schemaRequired: readonly string[] | undefined): boolean {
  return schemaRequired?.includes(key) ?? false;
}
