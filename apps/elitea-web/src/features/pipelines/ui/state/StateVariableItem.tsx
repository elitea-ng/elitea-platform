/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * state/StateVariableItem.jsx` (284 lines) — unit A2j. A single state-drawer
 * row: an editable name field/label plus `StateVariableItemActions`, with an
 * inline name-validation error and a fullscreen default-value viewer.
 * State/handlers live in `./StateVariableItem.controller.ts`, the shared
 * props type in `./StateVariableItem.types.ts` (both purely to keep this
 * component under the §3.5 `complexity` budget — see the controller file's
 * own doc comment).
 *
 * DISCLOSED REDESIGN (fullscreen default-value editor): the baseline opens
 * `@/components/StyledInputModal` (a top-level app component, not part of
 * any sub-unit's port scope and not built anywhere in this app — same
 * documented gap `shared/ui/ResizableCodeMirrorEditor.tsx`'s own doc
 * comment records for the identical component). Rebuilt from primitives
 * this unit actually owns: `shared/ui/ExpandedViewerModal` (fullscreen
 * chrome) hosting a `shared/ui/CodeMirrorEditor` (the editing surface),
 * exactly the same "compose the in-scope shared/ui pieces instead of the
 * unported bespoke modal" substitution `ResizableCodeMirrorEditor.tsx`
 * already established. JSON/List values get `@codemirror/lang-json`
 * highlighting + `jsonParseLinter()` (matching the baseline's
 * `specifiedLanguage="json"` -> `useLanguageLinter` wiring); every other
 * type gets a plain text editor (no baseline linter existed for `str`/
 * `number` either).
 *
 * `autoFocus` (baseline: on both the create-mode and the click-to-edit name
 * field) is dropped — `jsx-a11y/no-autofocus` bans it outright with no
 * per-file waiver, same fix `./EditCellInput.tsx`/`ui/nodes/BaseNode/
 * NodeCardHeader.tsx` already apply for the identical rule.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { json, jsonParseLinter } from '@codemirror/lang-json';
import { linter } from '@codemirror/lint';
import type { Extension } from '@codemirror/state';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { convertValueByType } from '../../lib/flow-editor/helpers/state.helpers';
import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { ExpandedViewerModal } from '@/shared/ui/ExpandedViewerModal';
import { t } from '@/shared/i18n';

import { useStateVariableItemController } from './StateVariableItem.controller';
import type { StateVariableItemProps } from './StateVariableItem.types';
import { StateVariableItemActions } from './StateVariableItemActions';
import { StateVariableTextField } from './StateVariableTextField';

export type { StateVariableItemProps } from './StateVariableItem.types';

const JSON_EXTENSIONS: Extension[] = [json(), linter(jsonParseLinter())];
const NO_EXTENSIONS: Extension[] = [];

function extensionsForType(type: string): Extension[] {
  return type === FlowEditorConstants.StateVariableTypes.Json || type === FlowEditorConstants.StateVariableTypes.List
    ? JSON_EXTENSIONS
    : NO_EXTENSIONS;
}

interface NameCellProps {
  readonly showField: boolean;
  readonly editValue: string;
  readonly name: string;
  readonly nameError: string;
  readonly nameFieldWidth: string;
  readonly isCreateMode: boolean;
  readonly isDefault: boolean;
  readonly enabled: boolean;
  readonly shouldExpandNameField: boolean;
  readonly disabled: boolean | undefined;
  readonly controller: Pick<
    ReturnType<typeof useStateVariableItemController>,
    'handleNameChange' | 'handleNameBlur' | 'handleNameKeyDown' | 'handleStartEdit'
  >;
}

/** Split out of `StateVariableItem` purely for the §3.5 `complexity` budget — the create/edit-field vs. static-label branch was the single biggest contributor. */
function NameCell(props: NameCellProps): ReactNode {
  const { showField, editValue, name, nameError, nameFieldWidth, isCreateMode, isDefault, enabled, shouldExpandNameField, disabled, controller } = props;

  if (showField) {
    return (
      <StateVariableTextField
        value={editValue}
        onChange={controller.handleNameChange}
        onBlur={controller.handleNameBlur}
        onKeyDown={controller.handleNameKeyDown}
        error={Boolean(nameError)}
        placeholder={isCreateMode ? t('pipelines.flowEditor.state.namePlaceholder', 'name') : undefined}
        width={nameFieldWidth}
        disabled={disabled}
      />
    );
  }

  return (
    <Box
      sx={nameBoxSx(isDefault, nameFieldWidth, enabled, shouldExpandNameField)}
      onClick={disabled ? undefined : controller.handleStartEdit}
    >
      <Typography sx={nameTextSx}>{name}</Typography>
    </Box>
  );
}

export function StateVariableItem(props: StateVariableItemProps): ReactNode {
  const { name, type, enabled, isDefault, defaultValue, drawerWidth = 300, editable = true, disabled } = props;
  const controller = useStateVariableItemController(props);
  const {
    isCreateMode,
    isEditing,
    editValue,
    nameError,
    validationError,
    isDefaultValueModalOpen,
    nameFieldWidth,
    shouldExpandNameField,
  } = controller;

  const errorMessage = nameError || validationError;
  const showNameField = isCreateMode || (!isDefault && isEditing);

  return (
    <Box sx={outerContainerSx}>
      <Box sx={containerSx}>
        <NameCell
          showField={showNameField}
          editValue={editValue}
          name={name}
          nameError={nameError}
          nameFieldWidth={nameFieldWidth}
          isCreateMode={isCreateMode}
          isDefault={Boolean(isDefault)}
          enabled={Boolean(enabled)}
          shouldExpandNameField={shouldExpandNameField}
          disabled={disabled}
          controller={controller}
        />

        <StateVariableItemActions
          type={type}
          enabled={enabled}
          showToggle={!isCreateMode && Boolean(isDefault)}
          drawerWidth={drawerWidth}
          defaultValue={defaultValue}
          disableTypeSelector={isCreateMode || !editable}
          onTypeChange={controller.handleTypeChange}
          onToggle={controller.handleToggleChange}
          onDelete={controller.handleDeleteClick}
          onDefaultValueClick={controller.handleDefaultValueClick}
          onDefaultValueChange={controller.handleDefaultValueInlineChange}
          editable={editable}
          disabled={disabled}
        />
      </Box>
      {!isCreateMode && (
        <ExpandedViewerModal
          open={isDefaultValueModalOpen}
          onClose={controller.handleDefaultValueClose}
          title={t('pipelines.flowEditor.state.defaultValueTitle', 'Default value')}
          content={
            <CodeMirrorEditor
              value={convertValueByType(type, defaultValue)}
              onBlur={controller.handleDefaultValueModalChange}
              extensions={extensionsForType(type)}
              readOnly={disabled}
              height="100%"
              aria-label={t('pipelines.flowEditor.state.defaultValueTitle', 'Default value')}
            />
          }
        />
      )}
      {errorMessage && (
        <Box sx={errorContainerSx}>
          <Typography
            variant="bodySmall"
            sx={errorTextSx}
          >
            {errorMessage}
          </Typography>
        </Box>
      )}
    </Box>
  );
}

const outerContainerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
  minWidth: 0,
});

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  minWidth: 0,
});

// Baseline: `fontSize: '0.6875rem'` (an ad-hoc size below `caption`'s own).
// Dropped per R-T11 — `fontSize` literals are banned outright, no token is
// close enough to justify a substitution — `variant="bodySmall"` (below)
// supplies the size and its 1.5 leading (the baseline's 1.3 is under the
// spec §5 floor of 1.4, and this text wraps).
const errorTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.error.main,
  whiteSpace: 'normal',
  wordBreak: 'break-word',
  overflowWrap: 'anywhere',
});

const errorContainerSx: SxProps<Theme> = (theme: Theme) => ({
  marginLeft: theme.spacing(0.5),
  minWidth: 0,
});

function nameBoxSx(
  isDefault: boolean,
  nameFieldWidth: string,
  enabled: boolean,
  shouldExpandNameField: boolean,
): SxProps<Theme> {
  return (theme: Theme) => ({
    flex: shouldExpandNameField ? 1 : '0 0 auto',
    width: shouldExpandNameField ? 'auto' : nameFieldWidth,
    minWidth: shouldExpandNameField ? 0 : nameFieldWidth,
    padding: `${theme.spacing(0.5)} ${theme.spacing(1.25)}`,
    borderRadius: theme.vars.shape.radiusMd,
    display: 'flex',
    alignItems: 'center',
    background: theme.vars.palette.background.userInputBackground,
    cursor: !isDefault ? 'text' : 'default',
    opacity: isDefault && !enabled ? 0.5 : 1,
    border: '.0625rem solid transparent',
    height: theme.spacing(4),
    boxSizing: 'border-box',
    '&:hover': {
      borderColor: !isDefault ? theme.vars.palette.border.lines : 'transparent',
    },
  });
}

const nameTextSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.bodyMedium,
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  width: '100%',
});
