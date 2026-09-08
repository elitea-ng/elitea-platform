import type { ChangeEvent, ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { MAX_DESCRIPTION_LENGTH, MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { PROMPT_PAYLOAD_KEY } from '@/shared/lib/prompt-payload';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { useFieldFocus } from '../../lib/hooks/useFieldFocus';
import { useGetCurrentToolkitSchemas } from '../../lib/hooks/useGetCurrentToolkitSchemas.hooks';
import { useToolkitIconKind } from '../../lib/hooks/useToolkitIconKind.hooks';
import { useToolkitNameProp } from '../../lib/hooks/useToolkitNameProp.hooks';
import { EntityIcon } from '../EntityIcon';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/form/
 * NameDescriptionInput.jsx` (163 lines).
 *
 * DISCLOSED REDESIGN (matches this batch's established convention —
 * `features/agents/ui/ApplicationEditForm.tsx`'s own doc comment): the
 * baseline's `editField`/`toolErrors` props are unchanged (already explicit,
 * not Formik reads), but three baseline hooks needed adapting:
 *  - `useToolkitNameProp(type)` (internally self-fetching) -> this slice's
 *    already-landed `useToolkitNameProp(type, schemaOfTools)` (explicit
 *    `schemaOfTools` parameter — see that file's own "ambient -> parameter"
 *    doc comment); `schemaOfTools` is supplied here via this slice's
 *    `useGetCurrentToolkitSchemas()`.
 *  - `useIconMetaTooltipType(type, isMCP)` (baseline: per-brand SVG lookup,
 *    a documented, disclosed gap — see `useToolkitIconKind.hooks.ts`'s own
 *    doc comment) -> `useToolkitIconKind`, resolving a semantic
 *    `EntityIcon` `entityType` instead of a brand icon.
 *  - `useSelectedProjectId()` (baseline: passed to `EntityIcon` as a
 *    `projectId` prop for its editable-icon-picker mode) is dropped
 *    entirely — this app's `EntityIcon` (`../EntityIcon.tsx`) has no
 *    editable mode (disclosed scope reduction, that file's own doc
 *    comment), so it never reads a `projectId`.
 *
 * `Input.StyledInputEnhancer` -> `shared/ui`'s `StyledInputEnhancer`
 * (`InputBaseProps`-shaped: `slotProps.htmlInput.maxLength` instead of a
 * bare `inputProps` object, standard MUI `ChangeEvent` `onChange` instead of
 * a bare string setter) — same adaptation `ApplicationEditForm.tsx` already
 * made for its own Name/Description fields, reused verbatim here.
 *
 * Split into `NameField`/`DescriptionField` sub-components (not inlined in
 * the main render) purely to stay under the §3.5 cyclomatic-complexity
 * budget (12) — the baseline is one big function; this app's lint budget
 * isn't, so the visibility/value logic per field is isolated the same way
 * `OpenAPIActionsTable.tsx`'s own `ToolRow` split documents for an
 * unrelated reason (component size) but the same underlying technique.
 */
export interface NameDescriptionInputProps {
  readonly type: string | undefined;
  readonly name: string | undefined;
  readonly toolkitName: string | undefined;
  readonly description: string | undefined;
  readonly editField: (field: 'name' | 'description', value: string) => void;
  readonly showValidation?: boolean;
  readonly toolErrors?: Readonly<Record<string, boolean | undefined>>;
  readonly showOnlyRequiredFields?: boolean;
  readonly showOnlyConfigurationFields?: boolean;
  readonly showNameFieldForcedly?: boolean;
  readonly showToolkitIcon?: boolean;
  readonly hideNameInput?: boolean;
  readonly configuration_title?: string;
  readonly isMCP?: boolean;
  readonly disabled?: boolean;
}

const nameContainerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };
const nameInputContainerSx: SxProps<Theme> = { width: '100%', display: 'flex', flexDirection: 'column', position: 'relative' };
const nameLengthMessageSx: SxProps<Theme> = { textAlign: 'right', position: 'absolute', right: '0', bottom: '3.125rem' };
const descriptionContainerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', position: 'relative' };
const descriptionLengthMessageSx: SxProps<Theme> = { textAlign: 'right', width: '100%', marginTop: '0.25rem' };
const iconSx: SxProps<Theme> = { width: '2.25rem', height: '2.25rem' };

interface NameFieldProps {
  readonly visible: boolean;
  readonly value: string;
  readonly nameIsRequired: boolean;
  readonly disabled: boolean | undefined;
  readonly showValidation: boolean;
  readonly hasError: boolean | undefined;
  readonly onChange: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly onFocus: () => void;
  readonly onBlur: () => void;
  readonly isFocused: boolean;
}

function NameField({ visible, value, nameIsRequired, disabled, showValidation, hasError, onChange, onFocus, onBlur, isFocused }: NameFieldProps): ReactNode {
  if (!visible) return null;
  return (
    <Box sx={nameInputContainerSx}>
      <StyledInputEnhancer
        disabled={!nameIsRequired || disabled}
        required={nameIsRequired}
        label={t('features.toolkits.nameDescriptionInput.nameLabel', 'Toolkit Name')}
        value={value}
        onChange={onChange}
        error={Boolean(showValidation && hasError)}
        helperText={showValidation && hasError ? t('features.toolkits.nameDescriptionInput.fieldRequired', 'Field is required') : undefined}
        slotProps={{ htmlInput: { maxLength: MAX_NAME_LENGTH } }}
        onFocus={onFocus}
        onBlur={onBlur}
      />
      {isFocused && MAX_NAME_LENGTH === value.length && (
        <Typography
          variant="bodySmall2"
          sx={nameLengthMessageSx}
        >
          {t('features.toolkits.nameDescriptionInput.charactersLeftZero', '0 is left from {{max}} characters left', { max: MAX_NAME_LENGTH })}
        </Typography>
      )}
    </Box>
  );
}

interface DescriptionFieldProps {
  readonly visible: boolean;
  readonly value: string;
  readonly disabled: boolean | undefined;
  readonly showValidation: boolean;
  readonly hasError: boolean | undefined;
  readonly onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onFocus: () => void;
  readonly onBlur: () => void;
  readonly isFocused: boolean;
}

function DescriptionField({ visible, value, disabled, showValidation, hasError, onChange, onFocus, onBlur, isFocused }: DescriptionFieldProps): ReactNode {
  if (!visible) return null;
  return (
    <Box sx={descriptionContainerSx}>
      <StyledInputEnhancer
        autoComplete="off"
        id="tool-description"
        label={t('features.toolkits.nameDescriptionInput.descriptionLabel', 'Description')}
        expand={{ maxRows: 15 }}
        actions={{ enabled: true }}
        value={value}
        onChange={onChange}
        error={Boolean(showValidation && hasError)}
        helperText={showValidation && hasError ? t('features.toolkits.nameDescriptionInput.fieldRequired', 'Field is required') : undefined}
        slotProps={{ htmlInput: { maxLength: MAX_DESCRIPTION_LENGTH } }}
        disabled={disabled}
        onFocus={onFocus}
        onBlur={onBlur}
      />
      {/* #848 — never unmount: unlike `nameLengthMessageSx` above (absolutely
        * positioned, so it overlays rather than pushes), this counter sits
        * in normal flow — `ToolBase.render.tsx` renders real property
        * fields directly after this component, and unmounting on blur
        * shifts them up, swallowing a click already headed for one of
        * them. `visibility: hidden` keeps the line's height reserved. */}
      <Typography
        variant="bodySmall"
        sx={combineSx(descriptionLengthMessageSx, { visibility: isFocused && value.length > 0 ? 'visible' : 'hidden' })}
      >
        {t('features.toolkits.nameDescriptionInput.charactersLeft', '{{count}} characters left', { count: MAX_DESCRIPTION_LENGTH - value.length })}
      </Typography>
    </Box>
  );
}

/**
 * Every field below is optional-with-a-default in `NameDescriptionInputProps`
 * — merging this constant under the raw props (`{...DEFAULT_PROPS,
 * ...props}`) resolves every default via a single object spread instead of
 * per-field `= value` destructuring defaults, which oxlint's `complexity`
 * rule counts as one branch each (8 fields here). Same "spread-merge over
 * default-param branches" technique `ToolBase.tsx`'s own doc comment (a
 * sibling A4c file) already documents for an identical budget problem.
 */
const DEFAULT_PROPS = {
  showValidation: false,
  showOnlyRequiredFields: false,
  showOnlyConfigurationFields: false,
  showNameFieldForcedly: false,
  showToolkitIcon: false,
  hideNameInput: false,
  configuration_title: '',
  isMCP: false,
} as const satisfies Partial<NameDescriptionInputProps>;

interface VisibilityInput {
  readonly showOnlyRequiredFields: boolean;
  readonly hideNameInput: boolean;
  readonly nameIsRequired: boolean;
  readonly descriptionIsRequired: boolean | undefined;
  readonly toolkitNameProp: string | undefined;
}

/** The baseline's `isToolNameVisible` (`NameDescriptionInput.jsx:51-52`), split out purely to keep the component's own complexity under budget — same logic, same branch order. */
function resolveNameVisibility({ showOnlyRequiredFields, hideNameInput, nameIsRequired, toolkitNameProp }: VisibilityInput): boolean {
  if (hideNameInput) return false;
  return showOnlyRequiredFields ? nameIsRequired : nameIsRequired || !toolkitNameProp;
}

/** The baseline's `isDescriptionVisible` (`NameDescriptionInput.jsx:53`). */
function resolveDescriptionVisibility({ showOnlyRequiredFields, descriptionIsRequired, toolkitNameProp }: VisibilityInput): boolean {
  return showOnlyRequiredFields ? descriptionIsRequired === true : !toolkitNameProp;
}

/** The baseline's `nameValue` (`NameDescriptionInput.jsx:54`): the required-name field always shows `name` itself; the toolkitName-driven field falls back through `toolkitName -> name -> configuration_title -> ''`. */
function resolveNameValue(nameIsRequired: boolean, name: string | undefined, toolkitName: string | undefined, configurationTitle: string): string {
  if (nameIsRequired) return name ?? '';
  return toolkitName || name || configurationTitle || '';
}

export function NameDescriptionInput(rawProps: NameDescriptionInputProps): ReactNode {
  const { type, name, toolkitName, description, editField, showValidation, toolErrors, showOnlyRequiredFields, showOnlyConfigurationFields, showNameFieldForcedly, showToolkitIcon, hideNameInput, configuration_title: configurationTitle, isMCP, disabled } = {
    ...DEFAULT_PROPS,
    ...rawProps,
  };
  const { toolkitSchemas } = useGetCurrentToolkitSchemas();
  const { toolkitNameProp, nameIsRequired, descriptionIsRequired } = useToolkitNameProp(type ?? '', toolkitSchemas);
  const { toggleFieldFocus, isFocused } = useFieldFocus();
  const { iconKind } = useToolkitIconKind(type, isMCP);

  const handleNameChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => editField('name', event.target.value),
    [editField],
  );
  const handleDescriptionChange = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => editField('description', event.target.value),
    [editField],
  );
  const onNameFocus = useCallback(() => toggleFieldFocus(PROMPT_PAYLOAD_KEY.name), [toggleFieldFocus]);
  const onDescriptionFocus = useCallback(() => toggleFieldFocus(PROMPT_PAYLOAD_KEY.description), [toggleFieldFocus]);
  const onFieldBlur = useCallback(() => toggleFieldFocus(null), [toggleFieldFocus]);

  // Matches the baseline's own `//@todo` comments verbatim (not acted on —
  // ported as-authored, same "TODO preserved, not silently resolved"
  // discipline as `shared/lib/prompt-payload.ts`'s `// todo: delete this`).
  if (showOnlyConfigurationFields) {
    return null;
  }

  const visibilityInput: VisibilityInput = { showOnlyRequiredFields, hideNameInput, nameIsRequired, descriptionIsRequired, toolkitNameProp };
  const isToolNameVisible = resolveNameVisibility(visibilityInput);
  const isDescriptionVisible = resolveDescriptionVisibility(visibilityInput);
  const nameValue = resolveNameValue(nameIsRequired, name, toolkitName, configurationTitle);

  return (
    <>
      <Box sx={nameContainerSx}>
        {showToolkitIcon && (
          <EntityIcon
            entityType={iconKind ?? 'toolkit'}
            sx={iconSx}
          />
        )}
        <NameField
          visible={isToolNameVisible || showNameFieldForcedly}
          value={nameValue}
          nameIsRequired={nameIsRequired}
          disabled={disabled}
          showValidation={showValidation}
          hasError={toolErrors?.name}
          onChange={handleNameChange}
          onFocus={onNameFocus}
          onBlur={onFieldBlur}
          isFocused={isFocused(PROMPT_PAYLOAD_KEY.name)}
        />
      </Box>

      <DescriptionField
        visible={isDescriptionVisible}
        value={description ?? ''}
        disabled={disabled}
        showValidation={showValidation}
        hasError={toolErrors?.description}
        onChange={handleDescriptionChange}
        onFocus={onDescriptionFocus}
        onBlur={onFieldBlur}
        isFocused={isFocused(PROMPT_PAYLOAD_KEY.description)}
      />
    </>
  );
}
