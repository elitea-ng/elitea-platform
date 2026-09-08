/**
 * How a field SPEC is read — which widget it wants, whether it is visible, and
 * what shape its values take.
 *
 * Split out of `ConfigurationSectionForm.tsx` when the Guardrails map editor
 * pushed that file past its line budget, and it is the right seam regardless:
 * everything here is a pure function of the server's spec with no MUI and no
 * React, which is why three of the four already had their own tests and one has
 * a non-test consumer (`useAdminConfigurationPage`).
 */
import type { ConfigListItemType } from './ConfigurationListEditor';
import type { AdminConfigField } from './api/adminConfigurationApi';

/**
 * Whether a field's `visible_when` is satisfied by the current values.
 *
 * The schema uses this to hide a dependent field — the OIDC fields behind
 * `auth_provider = oidc`, the whitelist behind `is_publish_blocked`. Absent
 * means always visible. An ARRAY of conditions means ALL of them, which is how
 * the schema's `litellm_db_name` reads.
 */
export function isFieldVisible(
  field: AdminConfigField,
  values: Readonly<Record<string, unknown>>,
): boolean {
  const condition = field.visible_when;
  if (condition === undefined) return true;
  // Declared as a union of one condition or many; `Array.isArray` on a readonly
  // array widens to `any[]`, so the narrowing is written out rather than
  // inferred.
  const conditions: ReadonlyArray<{ readonly field: string; readonly value: unknown }> =
    Array.isArray(condition)
      ? (condition as ReadonlyArray<{ readonly field: string; readonly value: unknown }>)
      : [condition as { readonly field: string; readonly value: unknown }];
  return conditions.every((entry) => values[entry.field] === entry.value);
}

/**
 * The widget a spec resolves to, decided once so the form and its tests cannot
 * disagree about it.
 *
 * `links` is matched on the KEY suffix rather than on the type, because the
 * schema types those fields as plain arrays — the `items` shape is what makes
 * them links, and the same suffix is what the server's validator keys on.
 */
export type ConfigWidget =
  | 'links'
  | 'list'
  | 'toolMap'
  | 'boolean'
  | 'select'
  | 'multiline'
  | 'html'
  | 'text'
  | 'number'
  | 'unavailable'
  | 'none';

export function widgetFor(field: AdminConfigField): ConfigWidget {
  // Checked FIRST, before the type. A field the server says cannot be set must
  // render as read-only whatever shape it has, and a later branch winning would
  // give it a working-looking control — the failure this whole unit removes.
  if (field.unavailable_reason !== undefined && field.unavailable_reason !== '') return 'unavailable';
  if (field.key.endsWith('_links')) return 'links';
  if (field.type === 'boolean') return 'boolean';
  if (field.type === 'string') return stringWidgetFor(field);
  if (field.type === 'integer' || field.type === 'number') return 'number';
  // An array whose element type the schema declares — the Features page's
  // `agent_categories` (strings) and `publish_whitelist_project_ids`
  // (integers). An array that declares NO element type still falls through to
  // `none`: the reference renders those as a free chips input, which invites an
  // operator to type values the consumer will drop on the floor.
  if (field.type === 'array' && listItemTypeFor(field) !== undefined) return 'list';
  // An object whose VALUES the schema types as a list of strings — the two
  // guardrail tool maps. An object that declares no `additionalProperties`
  // shape still falls through to `none`, for the same reason an untyped array
  // does: its values could be anything, and a control that accepted anything
  // would invite an operator to type what the consumer drops on the floor.
  if (field.type === 'object' && isToolMapField(field)) return 'toolMap';
  return 'none';
}

/** True for an object field whose values are declared as string lists. */
export function isToolMapField(field: AdminConfigField): boolean {
  const additional = field.additionalProperties;
  return additional?.type === 'array' && additional.items?.type === 'string';
}

/** The three shapes a `string` spec can take. Split out to keep `widgetFor` flat. */
function stringWidgetFor(field: AdminConfigField): ConfigWidget {
  if (field.enum !== undefined && field.enum.length > 0) return 'select';
  // `html` is a textarea WITH a rendered preview beside it. The distinction is
  // not cosmetic: markup an operator cannot see rendered is markup they only
  // find out about on the screen every refused user is looking at.
  if (field.format === 'html') return 'html';
  return field.format === 'textarea' ? 'multiline' : 'text';
}

/** The element type of an array field, when it is one this form can edit. */
export function listItemTypeFor(field: AdminConfigField): ConfigListItemType | undefined {
  const declared = field.items?.type;
  if (declared === 'string') return 'string';
  if (declared === 'integer') return 'integer';
  return undefined;
}
