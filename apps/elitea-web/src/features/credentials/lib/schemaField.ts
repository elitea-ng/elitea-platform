/**
 * lib/schemaField.ts — classifies one `ConfigSchemaNode` property into a
 * form-widget kind, and computes its initial value (unit A7).
 *
 * DISCLOSED SIMPLIFICATION: the baseline's per-type credential form is
 * rendered by the toolkits domain's generic JSON-schema form machinery
 * (`features/toolkits/ui/form/ToolBase/**`, `common/getToolInitialValueBySchema.js`,
 * `ToolBaseHelpers.isSecretField`) — all in the toolkits slice (unit A4),
 * out of this unit's ownership fence (R-L1: sibling `features/*` slices do
 * not import each other; `features/credentials` may not reach into
 * `features/toolkits`). This file is a NEW, narrower, credentials-scoped
 * reimplementation covering the field kinds this domain's credential
 * "data" schemas actually use (string/secret/boolean/number/enum) — not a
 * port of A4's full recursive object/array-field renderer. A credential
 * type whose schema needs nested objects or arrays will render those
 * sub-properties as a best-effort flat string field rather than the
 * baseline's dedicated `CommonObjectField`/`CommonArrayField` widgets.
 */
import type { ConfigSchemaNode } from '../api/configurations';

export type SchemaFieldKind = 'secret' | 'boolean' | 'number' | 'enum' | 'string' | 'configuration';

/**
 * WORDS, not substrings. The heuristic used to be one alternation run with
 * `RegExp.test` over the raw key — `/password|secret|token|api_key|.../i` —
 * which matches ANYWHERE in the key. `max_output_tokens` contains `token`,
 * so the LLM-model schema's `max_output_tokens: {type: 'integer'}` was
 * classified `secret`, rendered as `<input type="password">` and serialised
 * as the string `"16000"`. The server's model reader then dropped the whole
 * row ("skipping a malformed model configuration"), so a model saved through
 * the AI-Configuration form never appeared in any model picker.
 *
 * Two independent corrections keep that from recurring:
 *   1. `classifySchemaField` asks the SCHEMA first — a declared
 *      boolean/integer/number type wins over any name guess (a name guess is
 *      only ever a fallback for a schema that declares nothing).
 *   2. This heuristic matches whole words and known composite key names, so
 *      `max_output_tokens`, `max_tokens` and `token_limit` no longer look
 *      like secrets even when the schema declares no type at all.
 */

/**
 * Key names that are secrets in their entirety, matched on the key with its
 * separators removed — so `api_key`, `apiKey` and `apikey` are all one entry.
 */
const SECRET_WHOLE_KEYS: ReadonlySet<string> = new Set([
  'apikey',
  'apisecret',
  'apitoken',
  'authorization',
  'bearer',
  'pat',
]);

/** A single word that makes the whole key a secret wherever it appears. */
const SECRET_WORDS: ReadonlySet<string> = new Set(['password', 'passwd', 'passphrase', 'pwd', 'secret']);

/**
 * Qualifiers that turn a trailing `key` into a credential. Bare `key` is
 * deliberately NOT enough: `sort_key`, `partition_key` and `cache_key` are
 * ordinary strings.
 */
const SECRET_KEY_QUALIFIERS: ReadonlySet<string> = new Set([
  'api',
  'app',
  'access',
  'auth',
  'client',
  'consumer',
  'encryption',
  'private',
  'secret',
  'service',
  'signing',
  'ssh',
]);

/**
 * Splits a property key into lower-case words on `_`, `-`, `.`, whitespace
 * and camelCase boundaries: `maxOutputTokens` and `max_output_tokens` both
 * become `['max', 'output', 'tokens']`.
 */
function keyWords(key: string): readonly string[] {
  return key
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .split(/[^A-Za-z0-9]+/)
    .flatMap((part) => part.split(' '))
    .filter((part) => part.length > 0)
    .map((part) => part.toLowerCase());
}

/**
 * True when the property KEY names a secret.
 *
 * `token` counts only as the LAST word and only in the singular, which is
 * what separates a credential (`api_token`, `access_token`, `token`) from a
 * budget (`max_output_tokens`, `max_tokens`) or a bound (`token_limit`).
 */
function isSecretKeyName(key: string): boolean {
  const words = keyWords(key);
  if (words.length === 0) return false;
  const joined = words.join('');
  if (SECRET_WHOLE_KEYS.has(joined)) return true;
  if (words.some((word) => SECRET_WORDS.has(word))) return true;
  const last = words[words.length - 1] ?? '';
  const previous = words.length > 1 ? (words[words.length - 2] ?? '') : '';
  if (last === 'token') return true;
  if (last === 'key' && SECRET_KEY_QUALIFIERS.has(previous)) return true;
  return false;
}

/**
 * Type-guards one array entry of a schema's `anyOf`/`oneOf` list. Those
 * fields aren't declared on `ConfigSchemaNode` (they're read through its
 * `[key: string]: unknown` index signature — adding them to the interface
 * itself is a change to `../api/configurations.ts`, outside this cluster's
 * file scope per this unit's fence), so narrow at the point of use instead
 * of widening the shared type. Mirrors the baseline's own untyped duck-typed
 * read (`schemas.some(schema => schema.format === 'password' || ...)`).
 */
function asSchemaNode(value: unknown): ConfigSchemaNode | undefined {
  return typeof value === 'object' && value !== null ? (value as ConfigSchemaNode) : undefined;
}

function asSchemaNodeArray(value: unknown): readonly ConfigSchemaNode[] {
  if (!Array.isArray(value)) return [];
  const nodes: ConfigSchemaNode[] = [];
  for (const entry of value) {
    const node = asSchemaNode(entry);
    if (node) nodes.push(node);
  }
  return nodes;
}

/**
 * True when one of `property`'s `anyOf`/`oneOf` branches itself carries a
 * `format: 'password'`/`secret: true` marker — the standard Pydantic
 * `Optional[SecretStr]` shape (`anyOf: [{type: 'string', format:
 * 'password'}, {type: 'null'}]`). This is the nested-schema check
 * `ToolBaseHelpers.isSecretField` performs (baseline:
 * `fullSchema.anyOf || fullSchema.oneOf`, then `.some(...)`) — checked here
 * as the union of BOTH keywords (not "anyOf if present, else oneOf" like
 * the baseline) so a schema that happens to use `oneOf` alongside an
 * unrelated `anyOf` is never under-classified; being more inclusive here
 * only ever routes a field to the masked widget, never away from it.
 */
function hasNestedSecretMarker(property: ConfigSchemaNode | undefined): boolean {
  const branches = [...asSchemaNodeArray(property?.anyOf), ...asSchemaNodeArray(property?.oneOf)];
  return branches.some((branch) => branch.format === 'password' || branch.secret === true);
}

/**
 * A field is secret-shaped when the schema says so explicitly
 * (`format: 'password'` or `secret: true` — both real markers this
 * domain's schemas use, per `toolBase.helpers.js`'s `isSecretField`), when
 * one of its `anyOf`/`oneOf` branches carries either marker (the
 * `Optional[SecretStr]` shape — see `hasNestedSecretMarker`), OR its
 * property key matches a common secret-naming convention. This is the
 * local stand-in for the toolkits domain's `ToolBaseHelpers.isSecretField`
 * (see this module's doc comment).
 */
export function isLikelySecretField(key: string, property: ConfigSchemaNode | undefined): boolean {
  if (hasExplicitSecretMarker(property)) return true;
  return isSecretKeyName(key);
}

/** The schema's own, unambiguous "this is a secret" markers. */
function hasExplicitSecretMarker(property: ConfigSchemaNode | undefined): boolean {
  if (property?.format === 'password') return true;
  if (property?.secret === true) return true;
  return hasNestedSecretMarker(property);
}

/**
 * The sections a "pick an existing configuration" field draws its options
 * from, or `undefined` when the property is not one.
 *
 * The registry marks such a property with `configuration_sections`, e.g. the
 * `llm_model` schema's `ai_credentials`:
 * `{anyOf: [{$ref: '#/$defs/AiCredentials'}, {type: 'null'}],
 *   configuration_sections: ['ai_credentials'], default: null}`.
 * It is a REFERENCE to another stored row, never free text — see
 * `CredentialFormFields.tsx` for the widget and the object shape it writes.
 */
export function configurationSectionsOf(property: ConfigSchemaNode | undefined): readonly string[] | undefined {
  const branches = [property, ...asSchemaNodeArray(property?.anyOf), ...asSchemaNodeArray(property?.oneOf)];
  for (const branch of branches) {
    const sections = branch?.['configuration_sections'];
    if (!Array.isArray(sections)) continue;
    const names = sections.filter((entry): entry is string => typeof entry === 'string' && entry.length > 0);
    if (names.length > 0) return names;
  }
  return undefined;
}

/**
 * Classification order is deliberate and is itself the fix for the
 * `max_output_tokens` defect (see `SECRET_WHOLE_KEYS`' comment above):
 *
 *   1. the schema's explicit secret markers — the only unambiguous signal;
 *   2. a `configuration_sections` reference field;
 *   3. the DECLARED type (boolean, then number/integer);
 *   4. only then the secret-NAME heuristic, which is a guess;
 *   5. enum, then string.
 *
 * A declared `type: 'integer'` can therefore never render as a masked text
 * box, whatever the property is called.
 */
export function classifySchemaField(key: string, property: ConfigSchemaNode | undefined): SchemaFieldKind {
  if (hasExplicitSecretMarker(property)) return 'secret';
  if (configurationSectionsOf(property) !== undefined) return 'configuration';
  const branches = [property, ...asSchemaNodeArray(property?.anyOf), ...asSchemaNodeArray(property?.oneOf)].filter(
    (branch): branch is ConfigSchemaNode => branch !== undefined,
  );
  if (branches.some((branch) => branch.type === 'boolean')) return 'boolean';
  if (branches.some((branch) => branch.type === 'number' || branch.type === 'integer')) return 'number';
  if (isSecretKeyName(key)) return 'secret';
  if (branches.some((branch) => Array.isArray(branch.enum) && branch.enum.length > 0)) return 'enum';
  return 'string';
}

/**
 * Best-effort port of `getToolInitialValueBySchema`'s per-property default
 * resolution: the schema's own `default`, else a type-appropriate empty
 * value.
 *
 * SECURITY: a secret-shaped field (anything `classifySchemaField` routes to
 * the masked `SecretManagementInput`)
 * MUST NOT be pre-filled from the schema's own `default`, checked before
 * the generic `default` branch below. This mirrors the baseline's
 * `getToolInitialValueBySchema.js`'s `getPropValue`, which special-cases
 * `format === 'password'` to unconditionally `return null` regardless of
 * `effectiveDefault`. Widened here to every marker `classifySchemaField`
 * accepts (not just the literal `format: 'password'` case the baseline
 * special-cases) so the two stay internally consistent:
 * every field the UI renders as masked must also always start empty,
 * whatever marker — `format`, `secret: true`, a nested `anyOf`/`oneOf`
 * branch, or the key-name heuristic — triggered that classification. A
 * credential type whose schema ships a non-null `default` on such a field
 * (e.g. a placeholder API key) never leaks it into the initial form state.
 */
export function initialValueForSchemaField(key: string, property: ConfigSchemaNode | undefined): unknown {
  const kind = classifySchemaField(key, property);
  // Asks the CLASSIFIER, not the name heuristic directly: every field the
  // form renders masked must start empty, and only the classifier knows
  // which those are now that a declared type outranks the name guess.
  if (kind === 'secret') return '';
  // A reference to another stored row starts as "nothing chosen". An empty
  // string here would be written to the server as a bare string, which is
  // the second half of the defect this file's header describes: every reader
  // of `ai_credentials` expects `{elitea_title, private}` or null.
  if (kind === 'configuration') return null;
  if (property?.default !== undefined) return property.default;
  if (property?.type === 'boolean') return false;
  // number/integer/string/undefined all default to an empty string — an
  // empty numeric input is represented as `''`, not `0` or `NaN` (matches
  // `CommonNumberField`'s own `value: number | null` contract at the
  // `null` end, and avoids seeding a spurious `0` into a brand-new form).
  return '';
}

/**
 * Builds the initial `data` object for a newly-selected credential type —
 * one entry per schema property, defaulted per `initialValueForSchemaField`.
 * Hidden properties (`metadata.hidden`) are still included (the baseline
 * only hides them from the FORM, not from the submitted payload).
 */
export function initialDataForSchema(schema: ConfigSchemaNode | undefined): Record<string, unknown> {
  const properties = schema?.properties ?? {};
  const result: Record<string, unknown> = {};
  for (const [key, property] of Object.entries(properties)) {
    result[key] = initialValueForSchemaField(key, property);
  }
  return result;
}
