import type { ChangeEvent, ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import TextField from '@mui/material/TextField';

import { useSecretFieldOptions } from '@/entities/secret';
import { MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { ResizableCodeMirrorEditor } from '@/shared/ui/ResizableCodeMirrorEditor';
import { SecretManagementInput } from '@/shared/ui/SecretManagementInput';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import type { SingleSelectOption } from '@/shared/ui/SingleSelectMenuItem';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { getCodeLanguageExtensions } from './codeLanguageExtensions';

/**
 * The per-JSON-Schema-type render branches split out of
 * `ToolBaseProperty.tsx` to stay under the §3.5 400-line file budget — the
 * baseline (`ToolBaseProperty.jsx`, 720 lines) has no equivalent split,
 * this is a file-organization change only, no behaviour change.
 */

/** `label` plus, when `description` is set, a trailing info-icon tooltip — ported from the baseline's `renderLabelWithHint` closure. */
function labelWithHint(label: string, description: string | undefined, requiredMark: boolean): ReactNode {
  if (!description) return label;
  return (
    <>
      {label}
      {requiredMark && ' *'}
      <InfoTooltip
        title={description}
        sx={infoIconWrapperSx}
      />
    </>
  );
}

const infoIconWrapperSx = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  marginLeft: '0.25rem',
  verticalAlign: 'middle',
  marginBottom: '0.35rem',
};

export interface MaskedSecretFieldProps {
  readonly required: boolean;
  readonly label: string;
  readonly maxLength?: number | undefined;
}

/** A disabled, permanently-masked stand-in for a secret field — the baseline's `disableConfigFields || disabled` branch of the secret-field case (`ToolBaseProperty.jsx:308-320`). */
export function MaskedSecretField({ required, label, maxLength }: MaskedSecretFieldProps): ReactNode {
  return (
    <TextField
      fullWidth
      autoComplete="off"
      variant="standard"
      required={required}
      label={label}
      value="********"
      type="text"
      disabled
      slotProps={maxLength !== undefined ? { htmlInput: { maxLength } } : undefined}
    />
  );
}

export interface ObjectFieldProps {
  readonly value: Readonly<Record<string, unknown>> | undefined;
  readonly title: string | undefined;
  readonly label: string;
  readonly description: string | undefined;
  readonly required: boolean;
  readonly readOnly: boolean;
  readonly noAccordionWrapper: boolean;
  readonly onChange: (rawText: string) => void;
}

/** `label || title || 'Code Editor'` in the baseline — falls back to a generic name when the schema gives this object field neither. */
function objectFieldTitle(label: string, title: string | undefined): string {
  return label || title || t('features.toolkits.toolBase.objectField.defaultTitle', 'Code Editor');
}

/** A JSON object field, rendered as a code-editor of its pretty-printed JSON. Ported from `ToolBaseProperty.jsx:340-391`. */
export function ObjectField({
  value,
  title,
  label,
  description,
  required,
  readOnly,
  noAccordionWrapper,
  onChange,
}: ObjectFieldProps): ReactNode {
  const editor = (
    <ResizableCodeMirrorEditor
      expandAction
      value={JSON.stringify(value ?? {}, null, 2)}
      minHeight={100}
      onChange={onChange}
      readOnly={readOnly}
      {...(title !== undefined ? { fieldName: title } : {})}
    />
  );

  if (noAccordionWrapper) {
    return (
      <Box sx={objectFieldBoxSx}>
        <Typography
          variant="bodyMedium"
          sx={objectFieldLabelSx}
        >
          {description ? labelWithHint(label, description, required) : objectFieldTitle(label, title)}
        </Typography>
        {editor}
      </Box>
    );
  }

  // The hint is the `summaryAction`, never the title: a `<button>` inside the
  // summary's own is J18's `nested-interactive` axe failure (`BasicAccordion`).
  return (
    <Box sx={accordionContainerSx}>
      <BasicAccordion
        items={[
          {
            title: `${objectFieldTitle(label, title)}${description && required ? ' *' : ''}`,
            content: editor,
            ...(description ? { summaryAction: <InfoTooltip title={description} /> } : {}),
          },
        ]}
        slotSx={{ summary: { paddingRight: 0 } }}
      />
    </Box>
  );
}

const objectFieldBoxSx = { padding: '0 0.75rem', marginTop: '1rem' };
const objectFieldLabelSx = { display: 'inline-flex', alignItems: 'center', marginBottom: '0.5rem' };
const accordionContainerSx = { display: 'flex', flexDirection: 'column' as const, marginTop: '0.5rem' };

export interface BooleanFieldProps {
  readonly checked: boolean;
  readonly label: string;
  readonly description: string | undefined;
  readonly required: boolean;
  readonly disabled: boolean;
  readonly onChange: (checked: boolean) => void;
}

/** A checkbox field. Ported from `ToolBaseProperty.jsx:392-424`. */
export function BooleanField({ checked, label, description, required, disabled, onChange }: BooleanFieldProps): ReactNode {
  return (
    <Box sx={checkboxBoxSx}>
      <FormControlLabel
        required={required}
        sx={formControlLabelSx}
        control={
          <BaseCheckbox
            checked={checked}
            onChange={(_event, value) => onChange(value)}
            disabled={disabled}
          />
        }
        label={
          <Typography
            variant="bodyMedium"
            sx={checkboxLabelSx}
          >
            {description ? labelWithHint(label, description, false) : label}
          </Typography>
        }
      />
    </Box>
  );
}

const checkboxBoxSx = { marginTop: '0.75rem', marginBottom: '0.75rem' };
const formControlLabelSx = { height: '2rem', marginLeft: 0 };
const checkboxLabelSx = { display: 'inline-flex', alignItems: 'center' };

export interface EnumSelectFieldProps {
  readonly label: string;
  readonly description: string | undefined;
  readonly required: boolean;
  readonly value: string;
  readonly options: readonly SingleSelectOption[];
  readonly disabled: boolean;
  readonly onChange: (value: string) => void;
}

/**
 * A `string` field with an `enum` — ported from `ToolBaseProperty.jsx:425-450`.
 *
 * **R3 FIX (was: `description` passed as `SingleSelect`'s `placeholder`,
 * only visible while the field is unset — `shared/ui`'s `SingleSelect`, out
 * of this cluster's scope, has no `infoIconDescription`/persistent-tooltip
 * prop like the baseline's own `SingleSelect.jsx:633-636`).** A persistent
 * `InfoTooltip` (the same component `labelWithHint` above already uses) now
 * renders alongside the select instead — present whenever `description` is
 * set, independent of `value`.
 */
export function EnumSelectField({ label, description, required, value, options, disabled, onChange }: EnumSelectFieldProps): ReactNode {
  return (
    <Box sx={enumFieldContainerSx}>
      <SingleSelect
        label={label}
        required={required}
        onChange={onChange}
        value={value}
        options={[...options]}
        sx={selectSx}
        disabled={disabled}
      />
      {description && (
        <InfoTooltip
          title={description}
          sx={enumInfoIconSx}
        />
      )}
    </Box>
  );
}

const enumFieldContainerSx = { position: 'relative' as const };
const enumInfoIconSx = { position: 'absolute' as const, top: '0.5rem', right: '0.5rem' };
const selectSx = { marginTop: '0.5rem' };

export interface CodeLanguageFieldProps {
  readonly label: string;
  readonly value: string;
  readonly codeLanguage: string | undefined;
  readonly readOnly: boolean;
  readonly onChange: (value: string) => void;
}

/** A `string` field tagged with `code_language` — ported from `ToolBaseProperty.jsx:455-471`. See `codeLanguageExtensions.ts` for the disclosed language-support gap. */
export function CodeLanguageField({ label, value, codeLanguage, readOnly, onChange }: CodeLanguageFieldProps): ReactNode {
  return (
    <Box sx={objectFieldBoxSx}>
      <Typography variant="bodyMedium">{label}</Typography>
      <ResizableCodeMirrorEditor
        value={value}
        extensions={[...getCodeLanguageExtensions(codeLanguage)]}
        minHeight={100}
        onChange={onChange}
        readOnly={readOnly}
      />
    </Box>
  );
}

export interface MultilineFieldProps {
  readonly required: boolean;
  readonly label: string;
  readonly value: string | undefined;
  readonly onChange: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly error: boolean;
  readonly helperText: string | undefined;
  readonly rows: number;
  readonly disabled: boolean;
  readonly maxLength?: number | undefined;
  readonly placeholder?: string | undefined;
}

/** A `string` field with `lines > 1` — ported from `ToolBaseProperty.jsx:472-492`. */
export function MultilineField({ required, label, value, onChange, error, helperText, rows, disabled, maxLength, placeholder }: MultilineFieldProps): ReactNode {
  return (
    <TextField
      fullWidth
      autoComplete="off"
      variant="standard"
      required={required}
      label={label}
      value={value ?? ''}
      onChange={onChange}
      error={error}
      helperText={helperText}
      multiline
      rows={rows}
      disabled={disabled}
      slotProps={maxLength !== undefined ? { htmlInput: { maxLength } } : undefined}
      placeholder={placeholder}
    />
  );
}

/** Grouped per §3.5's 12-prop budget — see `DefaultTextFieldProps`. */
interface DefaultTextFieldMeta {
  readonly required: boolean;
  readonly label: string;
  readonly description: string | undefined;
  readonly isInteger: boolean;
}

interface DefaultTextFieldValidation {
  readonly error: boolean;
  readonly helperText: string | undefined;
}

interface DefaultTextFieldFocus {
  readonly onFocus: () => void;
  readonly onBlur: () => void;
}

export interface DefaultTextFieldProps {
  readonly field: DefaultTextFieldMeta;
  readonly value: string | number | undefined;
  readonly onChange: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly focus: DefaultTextFieldFocus;
  readonly validation: DefaultTextFieldValidation;
  readonly disabled: boolean;
  readonly maxLength?: number | undefined;
  readonly placeholder?: string | undefined;
  readonly showCharactersLeft: boolean;
}

/** The fallback single-line text field — ported from `ToolBaseProperty.jsx:601-639` (the trailing `else` branch). */
export function DefaultTextField({
  field,
  value,
  onChange,
  focus,
  validation,
  disabled,
  maxLength,
  placeholder,
  showCharactersLeft,
}: DefaultTextFieldProps): ReactNode {
  return (
    <Box sx={nameInputContainerSx}>
      <StyledInputEnhancer
        required={field.required}
        label={field.label}
        tooltipDescription={field.description}
        value={String(value ?? '')}
        onChange={onChange}
        error={validation.error}
        helperText={validation.helperText}
        type={field.isInteger ? 'tel' : undefined}
        disabled={disabled}
        slotProps={maxLength !== undefined ? { htmlInput: { maxLength } } : undefined}
        placeholder={placeholder}
        onFocus={focus.onFocus}
        onBlur={focus.onBlur}
      />
      {showCharactersLeft && (
        <Typography
          variant="bodySmall"
          sx={nameLengthMessageSx}
        >
          {`0 is left from ${MAX_NAME_LENGTH} characters left`}
        </Typography>
      )}
    </Box>
  );
}

const nameInputContainerSx = { width: '100%', display: 'flex', flexDirection: 'column' as const, position: 'relative' as const };
const nameLengthMessageSx = (theme: Theme) => ({
  textAlign: 'right' as const,
  fontSize: theme.typography.pxToRem(10),
  position: 'absolute' as const,
  right: 0,
  bottom: '2.75rem',
});

export interface SecretFieldProps {
  readonly value: string | undefined;
  readonly onChange: (value: string) => void;
  readonly label: string;
  readonly required: boolean;
  readonly error: boolean;
  readonly helperText: string | undefined;
}

/** The editable (non-disabled) secret field — ported from `ToolBaseProperty.jsx:324-339`. `secrets` comes from `useSecretFieldOptions()` (#441 — see that hook's header): no caller supplied it before, so the mode toggle, the saved-secret picker and its "Create new secret" entry were off for every user, an administrator included. The hook queries, so it runs in this leaf, which mounts for a secret field only. */
export function SecretFieldInput({ value, onChange, label, required, error, helperText }: SecretFieldProps): ReactNode {
  const secrets = useSecretFieldOptions();
  return (
    <SecretManagementInput
      value={value ?? ''}
      onChange={onChange}
      label={label}
      required={required}
      error={error}
      secrets={secrets}
      {...(helperText !== undefined ? { helperText } : {})}
    />
  );
}
