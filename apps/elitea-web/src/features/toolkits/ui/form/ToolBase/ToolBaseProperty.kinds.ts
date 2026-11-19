import type { CredentialLikeFieldKind, ToolErrors, ToolPropertySchema, ValidationErrorMessages } from './types';

/**
 * Pure, single-purpose classification helpers extracted out of
 * `ToolBaseProperty.tsx`'s dispatch chain — each one keeps its own
 * `eslint(complexity)` score low (§3.5 budget: 12) by owning exactly one
 * `anyOf`-aware type check, rather than all of them living inline in one
 * 700-line function the way the baseline (`ToolBaseProperty.jsx`) has them.
 * A file-organization change only, no behaviour change.
 */

function anyOfHasType(anyOf: ToolPropertySchema['anyOf'], type: string): boolean {
  return Boolean(anyOf?.some((branch) => branch.type === type));
}

function isArrayKind(key: string, schema: ToolPropertySchema): boolean {
  if (schema.type === 'array') return true;
  return key === 'scopes' && anyOfHasType(schema.anyOf, 'array');
}

function isObjectKind(schema: ToolPropertySchema): boolean {
  return schema.type === 'object' || anyOfHasType(schema.anyOf, 'object');
}

function isBooleanKind(schema: ToolPropertySchema): boolean {
  return schema.type === 'boolean' || anyOfHasType(schema.anyOf, 'boolean');
}

function isStringKind(schema: ToolPropertySchema): boolean {
  return schema.type === 'string' || anyOfHasType(schema.anyOf, 'string');
}

export function isIntegerKind(schema: ToolPropertySchema): boolean {
  return schema.type === 'integer' || anyOfHasType(schema.anyOf, 'integer');
}

const CREDENTIAL_LIKE_TYPES: ReadonlySet<CredentialLikeFieldKind> = new Set([
  'configuration',
  'llm_model',
  'embedding_model',
  'image_generation_model',
  'toolkit_reference',
  'agent_reference',
  'pipeline_reference',
]);

export function isCredentialLikeKind(type: string | undefined): type is CredentialLikeFieldKind {
  return type !== undefined && CREDENTIAL_LIKE_TYPES.has(type as CredentialLikeFieldKind);
}

/** `schema.default`, falling back to the first `anyOf` branch that declares one — the `Optional[X] = default` Pydantic shape. */
export function resolveAnyOfDefault(schema: ToolPropertySchema): unknown {
  if (schema.default !== undefined) return schema.default;
  return schema.anyOf?.find((branch) => branch.default !== undefined)?.default;
}

export type FieldKind =
  | 'openapiSpec'
  | 'oauthAppSelect'
  | 'selectedTools'
  | 'array'
  | 'secret'
  | 'object'
  | 'boolean'
  | 'enum'
  | 'codeLanguage'
  | 'multiline'
  | 'credentialLike'
  | 'default';

export interface ResolveFieldKindParams {
  readonly key: string;
  readonly schema: ToolPropertySchema;
  readonly isSecret: boolean;
}

function hasMultipleLines(schema: ToolPropertySchema): boolean {
  if (schema.lines === undefined) return false;
  return parseInt(String(schema.lines), 10) > 1;
}

/** The 3 string-typed sub-kinds (`enum`/`codeLanguage`/`multiline`), split out of `resolveFieldKind` to keep both functions under the §3.5 complexity budget. `undefined` means "plain string — fall through to `default`". */
function resolveStringFieldKind(schema: ToolPropertySchema): FieldKind | undefined {
  if (schema.enum?.length) return 'enum';
  if (schema.code_language !== undefined) return 'codeLanguage';
  if (hasMultipleLines(schema)) return 'multiline';
  return undefined;
}

/**
 * The baseline's top-level `if (uiComponent === 'openapi_spec') {...} else
 * if (k === 'selected_tools') {...} else if (type === 'array' || ...` chain
 * (`ToolBaseProperty.jsx:231-641`), reduced to a pure classification with
 * no rendering — `ToolBaseProperty.tsx` looks the returned kind up in a
 * table instead of re-testing each condition inline.
 */
export function resolveFieldKind({ key, schema, isSecret }: ResolveFieldKindParams): FieldKind {
  if (schema.ui_component === 'openapi_spec') return 'openapiSpec';
  if (schema.ui_component === 'oauth_app_select') return 'oauthAppSelect';
  if (key === 'selected_tools') return 'selectedTools';
  if (isArrayKind(key, schema)) return 'array';
  if (isSecret) return 'secret';
  if (isObjectKind(schema)) return 'object';
  if (isBooleanKind(schema)) return 'boolean';
  const stringKind = isStringKind(schema) ? resolveStringFieldKind(schema) : undefined;
  if (stringKind) return stringKind;
  if (isCredentialLikeKind(schema.type)) return 'credentialLike';
  return 'default';
}

export interface FieldVisibilityParams {
  readonly key: string;
  readonly schema: ToolPropertySchema;
  readonly required: boolean;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly showOnlyConfigurationFields: boolean;
  readonly showOnlyRequiredFields: boolean;
  readonly disableConfigFields: boolean;
}

/** `visible_when`'s case-insensitive-for-strings match — split out of `shouldRenderField` to keep both functions under the §3.5 complexity budget. */
function matchesVisibleWhen(schema: ToolPropertySchema, settings: Readonly<Record<string, unknown>>): boolean {
  if (!schema.visible_when) return true;
  const currentFieldValue = settings[schema.visible_when.field];
  const conditionValue = schema.visible_when.value;
  if (typeof currentFieldValue === 'string' && typeof conditionValue === 'string') {
    return currentFieldValue.toLowerCase() === conditionValue.toLowerCase();
  }
  return currentFieldValue === conditionValue;
}

/** The `disableConfigFields` branch's own "has a real value, or is elitea_title" check — split out of `shouldRenderField`. */
function isVisibleWhileConfigFieldsDisabled(key: string, settings: Readonly<Record<string, unknown>>): boolean {
  if (key === 'elitea_title') return true;
  const value = settings[key];
  return value !== null && value !== undefined && value !== '';
}

/**
 * The baseline's three visibility gates (`hidden`, `visible_when`, then the
 * `disableConfigFields`/`showOnlyConfigurationFields`/`showOnlyRequiredFields`
 * cascade, `ToolBaseProperty.jsx:194-228`), extracted to its own function
 * for the same complexity-budget reason as `resolveFieldKind`.
 */
export function shouldRenderField({
  key,
  schema,
  required,
  settings,
  showOnlyConfigurationFields,
  showOnlyRequiredFields,
  disableConfigFields,
}: FieldVisibilityParams): boolean {
  if (schema.hidden) return false;
  if (!matchesVisibleWhen(schema, settings)) return false;

  if (disableConfigFields) return isVisibleWhileConfigFieldsDisabled(key, settings);
  if (showOnlyConfigurationFields && !schema.configuration) return false;
  if (showOnlyRequiredFields && !required) return false;
  return true;
}

export interface FieldErrorState {
  readonly toastError: boolean;
  readonly errorText: string | undefined;
}

/**
 * The baseline's `isIntegerConstraintError`/`toastError`/`errorText` trio
 * (`ToolBaseProperty.jsx:94-103`), extracted to its own function for the
 * same complexity-budget reason as `resolveFieldKind`/`shouldRenderField`.
 */
export function deriveErrorState(
  toolErrors: ToolErrors,
  key: string,
  showValidation: boolean,
  validationErrorMessages: ValidationErrorMessages | undefined,
): FieldErrorState {
  const rawError = toolErrors[key];
  const isIntegerConstraintError = typeof rawError === 'string' && rawError !== '';
  const validationMessage = validationErrorMessages?.[key];
  const toastError = isIntegerConstraintError || Boolean(showValidation && (rawError || validationMessage));
  const errorText = toastError ? (typeof rawError === 'string' ? rawError : (validationMessage ?? 'Field is required')) : undefined;
  return { toastError, errorText };
}
