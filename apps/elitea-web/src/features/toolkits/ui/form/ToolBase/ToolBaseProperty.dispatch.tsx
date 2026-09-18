import type { ChangeEvent, ReactNode } from 'react';

import { load as loadYaml } from 'js-yaml';

import { openAPIExtract } from '../../../lib/helpers/openApi.helpers';
import type { ExtractedOpenApiOperation, OpenApiDocument } from '../../../lib/helpers/openApi.helpers';
import { OpenAPIActions } from '../ToolOpenAPI/OpenAPIActions';
import type { OpenAPIAction } from '../ToolOpenAPI/OpenAPIActionsTable';
import { OpenAPISchemaInput } from '../ToolOpenAPI/OpenAPISchemaInput';

import { ArrayFieldInput } from './ArrayFieldInput';
import { ToolActionsSelector } from './ToolActionsSelector';
import {
  BooleanField,
  CodeLanguageField,
  DefaultTextField,
  EnumSelectField,
  MaskedSecretField,
  MultilineField,
  ObjectField,
} from './ToolBaseProperty.renderers';
import { SecretFieldInput } from './SecretFieldInput';
import { isCredentialLikeKind, isIntegerKind, resolveAnyOfDefault } from './ToolBaseProperty.kinds';
import type { FieldKind } from './ToolBaseProperty.kinds';
import type { ToolBasePropertyCredentialContext, ToolBasePropertySlots } from './ToolBaseProperty.types';
import { MAX_NAME_LENGTH } from '@/shared/lib/limits';
import type { CredentialLikeFieldContext, EditToolField, OpenApiSpecFieldContext, SetToolErrors, ToolPropertySchema } from './types';

/**
 * Per-kind render functions plus the `RENDERERS` lookup table
 * `ToolBaseProperty.tsx` dispatches through — each function is its own
 * `eslint(complexity)` scope (§3.5 budget: 12), and the table itself is a
 * plain object literal, so `renderFieldByKind` (the one thing
 * `ToolBaseProperty.tsx` calls) stays at complexity 1 regardless of how
 * many kinds exist. Split out purely to keep the baseline's single
 * 700-line dispatch chain (`ToolBaseProperty.jsx:231-641`) under budget —
 * no behaviour change.
 */
export interface FieldRenderContext {
  readonly key: string;
  readonly schema: ToolPropertySchema;
  readonly label: string;
  readonly required: boolean;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly editField: EditToolField;
  readonly handleInputChange: (fieldPath: string) => (event: ChangeEvent<HTMLInputElement>) => void;
  readonly buildEditFieldPath: (fieldKey: string) => string;
  readonly toastError: boolean;
  readonly errorText: string | undefined;
  readonly effectiveDisabled: boolean;
  readonly noAccordionWrapper: boolean;
  readonly focusedField: string | undefined;
  readonly toggleFieldFocus: (field: string | undefined) => void;
  readonly credentialContext: ToolBasePropertyCredentialContext | undefined;
  readonly slots: ToolBasePropertySlots | undefined;
  readonly setToolErrors: SetToolErrors | undefined;
}

function renderOpenapiSpecFromSlot(ctx: FieldRenderContext, renderSlot: NonNullable<ToolBasePropertySlots['renderOpenApiSpecField']>): ReactNode {
  const context: OpenApiSpecFieldContext = {
    value: (ctx.settings[ctx.key] as string | undefined) ?? '',
    onSchemaChange: (schemaText) => ctx.editField(ctx.buildEditFieldPath(ctx.key), schemaText),
    selectedTools: (ctx.settings['selected_tools'] as readonly string[] | undefined) ?? [],
    onSelectionChange: (toolName, enabled) => {
      const current = (ctx.settings['selected_tools'] as readonly string[] | undefined) ?? [];
      const next = enabled ? (current.includes(toolName) ? current : [...current, toolName]) : current.filter((name) => name !== toolName);
      ctx.editField(ctx.buildEditFieldPath('selected_tools'), next);
    },
    error: ctx.toastError,
    helperText: ctx.errorText,
    disabled: ctx.effectiveDisabled,
    setToolErrors: ctx.setToolErrors,
  };
  return renderSlot(context);
}

/** `settings[k]`, parsed as an OpenAPI document (JSON, falling back to YAML) — mirrors the baseline's own local `parsedOpenAPIActions` `useMemo` (`ToolBaseProperty.jsx:133-149`). `[]` for anything unparsable, or without a `paths` key, same as the baseline. */
function parseOpenApiActions(specValue: string): readonly ExtractedOpenApiOperation[] {
  if (!specValue) return [];
  try {
    let parsedSpec: unknown;
    try {
      parsedSpec = JSON.parse(specValue);
    } catch {
      parsedSpec = loadYaml(specValue);
    }
    if (typeof parsedSpec !== 'object' || parsedSpec === null || !('paths' in parsedSpec)) return [];
    return openAPIExtract(parsedSpec as OpenApiDocument);
  } catch {
    return [];
  }
}

/** The baseline's own `selected_tools` auto-merge on a schema change (`ToolBaseProperty.jsx:241-254`): keep still-valid selections, append newly-discovered tools; when nothing was selected yet, select everything discovered. */
function mergeSelectedToolsWithNewSchema(newToolNames: readonly string[], currentSelectedTools: readonly string[]): readonly string[] {
  if (currentSelectedTools.length === 0) return newToolNames;
  const stillValid = currentSelectedTools.filter((tool) => newToolNames.includes(tool));
  const newlyAdded = newToolNames.filter((tool) => !currentSelectedTools.includes(tool));
  return [...stillValid, ...newlyAdded];
}

/** `exactOptionalPropertyTypes` (this app's tsconfig): `ExtractedOpenApiOperation.description` is `string | undefined` (a real property), `OpenAPIAction.description` is `string?` — the key must be omitted, not present-with-`undefined`, matching this codebase's established convention (e.g. `OpenAPISchemaInput.tsx`'s own conditional `onSyntaxError` spread). */
function toOpenApiAction(action: ExtractedOpenApiOperation): OpenAPIAction {
  return { name: action.name, method: action.method, path: action.path, ...(action.description !== undefined ? { description: action.description } : {}) };
}

/**
 * R2 fix: the real, now-landed `../ToolOpenAPI/{OpenAPISchemaInput,
 * OpenAPIActions}.tsx` are the DEFAULT when no `renderOpenApiSpecField` slot
 * is supplied — matching the baseline, which composed both inline,
 * unconditionally (`ToolBaseProperty.jsx:230-276`: a schema-text editor plus
 * a derived actions table that also auto-merges `selected_tools` on every
 * schema edit). Previously this unconditionally returned `null` without a
 * slot, which is what the live composition root (`ToolkitForm.hooks.ts`, no
 * `slots` concept) always does — every OpenAPI toolkit's schema editor was
 * silently blank.
 */
/**
 * `OpenAPISchemaInput.setToolErrors` declares a narrower `Record<string,
 * boolean>` updater (it only ever reads/writes its own `openApiSchema`
 * boolean key) than this file's real, wider `SetToolErrors` (`ToolErrors` =
 * `Record<string, boolean|string>`, integer-constraint fields carry a
 * message string). Adapts without an unsafe cast: the boolean-only view fed
 * to `OpenAPISchemaInput`'s own updater is a genuine subset of `previous`,
 * and every other (string-valued) key passes through the `...previous`
 * spread untouched.
 */
function adaptSetToolErrorsForOpenApi(setToolErrors: SetToolErrors | undefined): (updater: (prevErrors: Record<string, boolean>) => Record<string, boolean>) => void {
  return (updater) => {
    setToolErrors?.((previous) => {
      const booleanView: Record<string, boolean> = {};
      for (const [key, value] of Object.entries(previous)) {
        if (typeof value === 'boolean') booleanView[key] = value;
      }
      return { ...previous, ...updater(booleanView) };
    });
  };
}

function renderOpenapiSpecDefault(ctx: FieldRenderContext): ReactNode {
  const value = (ctx.settings[ctx.key] as string | undefined) ?? '';
  const selectedTools = (ctx.settings['selected_tools'] as readonly string[] | undefined) ?? [];
  const parsedActions = parseOpenApiActions(value);

  const handleValueChange = (schemaText: string, parsedNewActions: readonly ExtractedOpenApiOperation[]): void => {
    ctx.editField(ctx.buildEditFieldPath(ctx.key), schemaText);
    if (parsedNewActions.length === 0) return;
    const newToolNames = parsedNewActions.map((action) => action.name).filter(Boolean);
    ctx.editField(ctx.buildEditFieldPath('selected_tools'), mergeSelectedToolsWithNewSchema(newToolNames, selectedTools));
  };

  return (
    <>
      {/* `OpenAPISchemaInputProps` (`../ToolOpenAPI/OpenAPISchemaInput.tsx`, a
          sibling A4 file this cluster does not redesign) has no
          `disabled`/read-only prop at all — a real, pre-existing gap in that
          component unrelated to R1/R2/R3, so `ctx.effectiveDisabled` is not
          forwarded here rather than inventing an unsupported prop. */}
      <OpenAPISchemaInput
        value={value}
        onValueChange={handleValueChange}
        error={ctx.toastError}
        setToolErrors={adaptSetToolErrorsForOpenApi(ctx.setToolErrors)}
        {...(ctx.errorText !== undefined ? { helperText: ctx.errorText } : {})}
      />
      {parsedActions.length > 0 && <OpenAPIActions tools={parsedActions.map(toOpenApiAction)} />}
    </>
  );
}

function renderOpenapiSpec(ctx: FieldRenderContext): ReactNode {
  return ctx.slots?.renderOpenApiSpecField ? renderOpenapiSpecFromSlot(ctx, ctx.slots.renderOpenApiSpecField) : renderOpenapiSpecDefault(ctx);
}

function renderSelectedTools(ctx: FieldRenderContext): ReactNode {
  const items = ctx.schema.items;
  const argsSchemas = ctx.schema.args_schemas;
  const hasArgsSchemas = argsSchemas !== undefined && Object.keys(argsSchemas).length > 0;
  const tools = hasArgsSchemas ? Object.keys(argsSchemas) : (items?.enum ?? []);
  return (
    <ToolActionsSelector
      availableTools={tools}
      onChange={(value) => ctx.editField(ctx.buildEditFieldPath('selected_tools'), value)}
      disabled={ctx.effectiveDisabled}
    />
  );
}

function renderArray(ctx: FieldRenderContext): ReactNode {
  return (
    <ArrayFieldInput
      propertyKey={ctx.key}
      settings={ctx.settings}
      required={ctx.required}
      label={ctx.label}
      toastError={ctx.toastError}
      errorText={ctx.errorText}
      disableConfigFields={ctx.effectiveDisabled}
      disabled={ctx.effectiveDisabled}
      editField={ctx.editField}
      buildEditFieldPath={ctx.buildEditFieldPath}
    />
  );
}

function renderSecret(ctx: FieldRenderContext): ReactNode {
  if (ctx.effectiveDisabled) {
    return (
      <MaskedSecretField required={ctx.required} label={ctx.label} maxLength={ctx.schema.max_toolkit_length} />
    );
  }
  return (
    <SecretFieldInput
      value={ctx.settings[ctx.key] as string | undefined}
      onChange={(value) => ctx.editField(ctx.buildEditFieldPath(ctx.key), value)}
      label={ctx.label}
      required={ctx.required}
      error={ctx.toastError}
      helperText={ctx.errorText}
      isTeamProject={ctx.credentialContext?.isTeamProject}
    />
  );
}

function renderObject(ctx: FieldRenderContext): ReactNode {
  // elitea_issues: #2611 — a JSON object field (e.g. Postman's "Environment
  // Config JSON") used to commit `{}` on blur whenever the typed text was not
  // valid JSON, silently wiping whatever had been saved before and flipping
  // Save on with no actual valid change. On a parse failure this now LEAVES
  // the stored value untouched and flags the field as errored (blocking Save
  // via `hasErrors`, same mechanism `renderOpenapiSpec`'s schema field
  // already uses) instead of clobbering it — the user's data survives an
  // accidental or mid-edit blur, and the error clears itself once the text
  // parses again.
  const onChange = (rawText: string): void => {
    const textContent = rawText.trim();
    if (textContent === '') {
      ctx.editField(ctx.buildEditFieldPath(ctx.key), {});
      ctx.setToolErrors?.((previous) => ({ ...previous, [ctx.key]: false }));
      return;
    }
    try {
      const parsedValue = JSON.parse(textContent) as Readonly<Record<string, unknown>>;
      ctx.editField(ctx.buildEditFieldPath(ctx.key), parsedValue, true);
      ctx.setToolErrors?.((previous) => ({ ...previous, [ctx.key]: false }));
    } catch {
      ctx.setToolErrors?.((previous) => ({ ...previous, [ctx.key]: true }));
    }
  };
  return (
    <ObjectField
      value={ctx.settings[ctx.key] as Readonly<Record<string, unknown>> | undefined}
      title={ctx.schema.title}
      label={ctx.label}
      description={ctx.schema.description}
      required={ctx.required}
      readOnly={ctx.effectiveDisabled}
      noAccordionWrapper={ctx.noAccordionWrapper}
      onChange={onChange}
    />
  );
}

function renderBoolean(ctx: FieldRenderContext): ReactNode {
  return (
    <BooleanField
      checked={Boolean(ctx.settings[ctx.key])}
      label={ctx.label}
      description={ctx.schema.description}
      required={ctx.key === 'cloud' ? false : ctx.required}
      disabled={ctx.effectiveDisabled}
      onChange={(value) => ctx.editField(ctx.buildEditFieldPath(ctx.key), value)}
    />
  );
}

/** The current/default-resolved enum value — split out of `renderEnum` to keep that function's own complexity low. */
function resolveEnumValue(ctx: FieldRenderContext, options: readonly { value: string }[]): string {
  const currentValue = ctx.settings[ctx.key];
  if (options.some((option) => option.value === currentValue)) return currentValue as string;
  const schemaDefault = resolveAnyOfDefault(ctx.schema);
  if (options.some((option) => option.value === schemaDefault)) return schemaDefault as string;
  return '';
}

function renderEnum(ctx: FieldRenderContext): ReactNode {
  const options = (ctx.schema.enum ?? []).map((item) => ({ label: item, value: item }));
  return (
    <EnumSelectField
      label={ctx.label}
      description={ctx.schema.description}
      required={ctx.required}
      value={resolveEnumValue(ctx, options)}
      options={options}
      disabled={ctx.effectiveDisabled}
      onChange={(value) => ctx.editField(ctx.buildEditFieldPath(ctx.key), value)}
    />
  );
}

function renderCodeLanguage(ctx: FieldRenderContext): ReactNode {
  return (
    <CodeLanguageField
      label={ctx.label}
      value={(ctx.settings[ctx.key] as string | undefined) ?? ''}
      codeLanguage={ctx.schema.code_language}
      readOnly={ctx.effectiveDisabled}
      onChange={(value) => ctx.editField(ctx.buildEditFieldPath(ctx.key), value)}
    />
  );
}

function renderMultiline(ctx: FieldRenderContext): ReactNode {
  const rows = parseInt(String(ctx.schema.lines), 10);
  return (
    <MultilineField
      required={ctx.required}
      label={ctx.label}
      value={ctx.settings[ctx.key] as string | undefined}
      onChange={ctx.handleInputChange(ctx.buildEditFieldPath(ctx.key))}
      error={ctx.toastError}
      helperText={ctx.errorText}
      rows={rows}
      disabled={ctx.effectiveDisabled}
      maxLength={ctx.schema.max_toolkit_length}
      placeholder={ctx.schema.placeholder}
    />
  );
}

function renderCredentialLike(ctx: FieldRenderContext): ReactNode {
  if (!ctx.slots?.renderCredentialLikeField || !isCredentialLikeKind(ctx.schema.type)) return null;
  const context: CredentialLikeFieldContext = {
    kind: ctx.schema.type,
    propertyKey: ctx.key,
    schema: ctx.schema,
    label: ctx.label,
    required: ctx.required,
    disabled: ctx.effectiveDisabled,
    error: ctx.toastError,
    helperText: ctx.errorText,
    value: ctx.settings[ctx.key],
    onChange: (value, options) => ctx.editField(ctx.buildEditFieldPath(ctx.key), value as never, undefined, options),
    specifiedProjectId: ctx.credentialContext?.specifiedProjectId,
    presetOptions: ctx.credentialContext?.presetOptions,
    onCredentialReload: ctx.credentialContext?.onCredentialReload,
  };
  return ctx.slots.renderCredentialLikeField(context);
}

/** The default single-line text field's own `isInteger`/`maxLength`/`placeholder`/`showCharactersLeft` derivations, split out to keep `renderDefault` itself under the complexity budget. */
function resolveDefaultTextFieldPresentation(ctx: FieldRenderContext) {
  const isInteger = isIntegerKind(ctx.schema);
  const maxLength = ctx.key === 'label' ? MAX_NAME_LENGTH : ctx.schema.max_toolkit_length;
  const defaultValue = resolveAnyOfDefault(ctx.schema);
  const defaultValueText = typeof defaultValue === 'string' || typeof defaultValue === 'number' ? String(defaultValue) : undefined;
  const placeholder = ctx.schema.placeholder ?? (isInteger ? defaultValueText : undefined);
  const currentValue = ctx.settings[ctx.key];
  const showCharactersLeft =
    ctx.key === 'label' && ctx.focusedField === ctx.key && typeof currentValue === 'string' && currentValue.length === MAX_NAME_LENGTH;
  return { isInteger, maxLength, placeholder, currentValue, showCharactersLeft };
}

function renderDefault(ctx: FieldRenderContext): ReactNode {
  const { isInteger, maxLength, placeholder, currentValue, showCharactersLeft } = resolveDefaultTextFieldPresentation(ctx);
  return (
    <DefaultTextField
      field={{ required: ctx.required, label: ctx.label, description: ctx.schema.description, isInteger }}
      value={currentValue as string | number | undefined}
      onChange={ctx.handleInputChange(ctx.buildEditFieldPath(ctx.key))}
      focus={{ onFocus: () => ctx.toggleFieldFocus(ctx.key), onBlur: () => ctx.toggleFieldFocus(undefined) }}
      validation={{ error: ctx.toastError, helperText: ctx.errorText }}
      disabled={ctx.effectiveDisabled}
      maxLength={maxLength}
      placeholder={placeholder}
      showCharactersLeft={showCharactersLeft}
    />
  );
}

const RENDERERS: Readonly<Record<FieldKind, (ctx: FieldRenderContext) => ReactNode>> = {
  openapiSpec: renderOpenapiSpec,
  selectedTools: renderSelectedTools,
  array: renderArray,
  secret: renderSecret,
  object: renderObject,
  boolean: renderBoolean,
  enum: renderEnum,
  codeLanguage: renderCodeLanguage,
  multiline: renderMultiline,
  credentialLike: renderCredentialLike,
  default: renderDefault,
};

export function renderFieldByKind(kind: FieldKind, ctx: FieldRenderContext): ReactNode {
  return RENDERERS[kind](ctx);
}
