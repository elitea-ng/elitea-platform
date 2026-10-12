/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * state/StateVariableList.jsx` (259 lines) — unit A2j. The State drawer's
 * list body: the two always-present `input`/`messages` rows, an optional
 * `input_attachments` row, every custom variable row, an in-place "new
 * variable" create row, and the "Context" add button.
 *
 * `borderRadius: spacing(2)` -> `theme.vars.shape.radiusLg` (R-T10; 16px is
 * an exact match). `useStateValidation` is the already-landed A2d hook
 * (`../../lib/flow-editor/hooks/useStateValidation`) — its own doc comment
 * records the Redux-slice -> `pipelineEditorStore` swap, unrelated to this
 * file.
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button, { buttonClasses } from '@mui/material/Button';
import type { SxProps, Theme } from '@mui/material/styles';

import { FlowEditorConstants, StateDrawerConstants } from '../../lib/flow-editor/constants';
import {
  getDefaultValueForType,
  getMessagesFromState,
  getValueByType,
  validateVariableName,
} from '../../lib/flow-editor/helpers/state.helpers';
import { useStateValidation } from '../../lib/flow-editor/hooks/useStateValidation';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';
import { t } from '@/shared/i18n';

import { StateVariableItem } from './StateVariableItem';

interface StateVariableConfig {
  readonly type?: string;
  readonly value?: unknown;
}

/** @public */
export interface StateVariableListProps {
  readonly states?: Readonly<Record<string, StateVariableConfig>> | undefined;
  readonly drawerWidth?: number;
  readonly onDeleteState: (name: string) => void;
  readonly onToggleState: (name: string, enabled: boolean) => void;
  readonly onUpdateState: (name: string, changes: Readonly<Record<string, unknown>>) => void;
  readonly onAddState: (name: string, type?: string) => boolean;
  readonly disabled?: boolean | undefined;
}

interface StateEntry {
  readonly name: string;
  readonly type: string;
  readonly value: unknown;
}

function toStateEntries(states: Readonly<Record<string, StateVariableConfig>> | undefined): StateEntry[] {
  // `FlowEditorConstants.DefaultState`'s own `as const` literal type is
  // narrower than `Record<string, StateVariableConfig>` — normalizing the
  // union to one explicit type before `Object.entries` keeps its inferred
  // entry type as `[string, StateVariableConfig]`, not the untyped
  // `[string, any][]` fallback overload `Object.entries` picks for an
  // unrecognized union shape (verified: without this, oxlint's
  // `no-unsafe-member-access` fires on `config.type`/`config.value` below).
  const source: Readonly<Record<string, StateVariableConfig>> = states ?? FlowEditorConstants.DefaultState;
  return Object.entries(source)
    .filter(
      ([entryName]) =>
        !(FlowEditorConstants.StateDefaultProps as readonly string[]).includes(entryName) &&
        !(FlowEditorConstants.StateManagedProps as readonly string[]).includes(entryName),
    )
    .map(([entryName, config]) => ({
      name: entryName,
      type: config.type ?? 'str',
      value: config.value,
    }));
}

export function StateVariableList(props: StateVariableListProps): ReactNode {
  const { states, drawerWidth, onDeleteState, onToggleState, onUpdateState, onAddState, disabled } = props;
  const [isCreatingNew, setIsCreatingNew] = useState(false);

  const { validateVariable, clearValidationError } = useStateValidation(states);

  const stateEntries = useMemo(() => toStateEntries(states), [states]);

  const validateName = useCallback(
    (variableName: string, excludeName?: string | null) =>
      validateVariableName(variableName, excludeName, states),
    [states],
  );

  const handleCancelCreate = useCallback(() => {
    setIsCreatingNew(false);
  }, []);

  const handleUpdateNameWithCreate = useCallback(
    (oldName: string, newName: string) => {
      if (isCreatingNew && oldName === '') {
        const success = onAddState(newName, 'str');
        if (success) {
          setIsCreatingNew(false);
        }
      } else if (newName && !states?.[newName]) {
        onUpdateState(oldName, { newName });
      }
    },
    [isCreatingNew, states, onAddState, onUpdateState],
  );

  const handleToggle = useCallback(
    (variableName: string, enabled: boolean) => {
      onToggleState(variableName, enabled);
    },
    [onToggleState],
  );

  const handleDelete = useCallback(
    (variableName: string) => {
      clearValidationError(variableName);
      onDeleteState(variableName);
    },
    [onDeleteState, clearValidationError],
  );

  const handleUpdateType = useCallback(
    (stateName: string, newType: string) => {
      const currentValue = states?.[stateName]?.value;
      const value =
        currentValue !== undefined && currentValue !== '' ? currentValue : getDefaultValueForType(newType);
      onUpdateState(stateName, { type: newType, value });
    },
    [onUpdateState, states],
  );

  const handleUpdateDefaultValue = useCallback(
    (variableName: string, type: string, newValue: unknown) => {
      if (
        type === FlowEditorConstants.StateVariableTypes.List ||
        type === FlowEditorConstants.StateVariableTypes.Json ||
        type === FlowEditorConstants.StateVariableTypes.Number
      ) {
        const validationError = validateVariable(variableName, type, newValue);
        if (validationError) {
          onUpdateState(variableName, { value: newValue });
          return;
        }
      } else {
        clearValidationError(variableName);
      }

      onUpdateState(variableName, { value: getValueByType(variableName, type, newValue) });
    },
    [onUpdateState, validateVariable, clearValidationError],
  );

  return (
    <Box sx={containerSx}>
      <StateVariableItem
        key={FlowEditorConstants.STATE_INPUT}
        name={FlowEditorConstants.STATE_INPUT}
        type={FlowEditorConstants.StateVariableTypes.String}
        enabled={!states || Boolean(states[FlowEditorConstants.STATE_INPUT])}
        isDefault
        defaultValue={states?.[FlowEditorConstants.STATE_INPUT]?.value}
        drawerWidth={drawerWidth}
        validateName={validateName}
        onToggle={handleToggle}
        onDelete={handleDelete}
        onUpdateName={handleUpdateNameWithCreate}
        onUpdateType={handleUpdateType}
        onUpdateDefaultValue={handleUpdateDefaultValue}
        editable={false}
        disabled={disabled}
      />
      <StateVariableItem
        key={FlowEditorConstants.STATE_MESSAGES}
        name={FlowEditorConstants.STATE_MESSAGES}
        type={FlowEditorConstants.StateVariableTypes.List}
        enabled={!states || Boolean(states[FlowEditorConstants.STATE_MESSAGES])}
        isDefault
        defaultValue={getMessagesFromState(states)}
        drawerWidth={drawerWidth}
        validateName={validateName}
        onToggle={handleToggle}
        onDelete={handleDelete}
        onUpdateName={handleUpdateNameWithCreate}
        onUpdateType={handleUpdateType}
        onUpdateDefaultValue={handleUpdateDefaultValue}
        editable={false}
        disabled={disabled}
      />
      {Boolean(states?.[FlowEditorConstants.STATE_INPUT_ATTACHMENTS]) && (
        <StateVariableItem
          key={FlowEditorConstants.STATE_INPUT_ATTACHMENTS}
          name={FlowEditorConstants.STATE_INPUT_ATTACHMENTS}
          type={FlowEditorConstants.StateVariableTypes.List}
          enabled
          isDefault
          drawerWidth={drawerWidth}
          validateName={validateName}
          onToggle={handleToggle}
          onDelete={handleDelete}
          onUpdateName={handleUpdateNameWithCreate}
          onUpdateType={handleUpdateType}
          onUpdateDefaultValue={handleUpdateDefaultValue}
          editable={false}
          disabled={disabled}
        />
      )}
      {stateEntries.map(({ name: entryName, type, value }) => (
        <StateVariableItem
          key={entryName}
          name={entryName}
          type={type}
          enabled
          isDefault={false}
          defaultValue={value}
          drawerWidth={drawerWidth}
          validateName={validateName}
          onToggle={handleToggle}
          onDelete={handleDelete}
          onUpdateName={handleUpdateNameWithCreate}
          onUpdateType={handleUpdateType}
          onUpdateDefaultValue={handleUpdateDefaultValue}
          disabled={disabled}
        />
      ))}

      {isCreatingNew && (
        <StateVariableItem
          mode={StateDrawerConstants.ItemMode.Create}
          name=""
          type="str"
          enabled
          drawerWidth={drawerWidth}
          validateName={validateName}
          onUpdateName={handleUpdateNameWithCreate}
          onCancel={handleCancelCreate}
        />
      )}

      <Button
        variant="outlined"
        startIcon={
          <Box sx={iconWrapperSx}>
            <PlusIcon />
          </Box>
        }
        onClick={() => setIsCreatingNew(true)}
        sx={addButtonSx}
        disabled={disabled}
      >
        {t('pipelines.flowEditor.state.addContext', 'Context')}
      </Button>
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
});

const iconWrapperSx: SxProps<Theme> = {
  display: 'flex',
  transform: 'scale(0.7)',
};

const addButtonSx: SxProps<Theme> = (theme: Theme) => ({
  minHeight: theme.spacing(3),
  padding: `${theme.spacing(0.5)} ${theme.spacing(1.5)}`,
  // Baseline: `fontSize: '.75rem'` — R-T11 (spec §4.4) bans ad-hoc
  // fontSize literals, so this reads the same 0.75rem value off the
  // `bodySmall` typography variant (`theme.typography.bodySmall.fontSize`)
  // instead, matching this button's own `fontWeight: 400` (bodySmall's
  // weight; the sibling `labelSmall` variant is the same 0.75rem size but
  // weight 500).
  fontSize: theme.typography.bodySmall.fontSize,
  fontWeight: 400,
  alignSelf: 'flex-start',
  background: 'transparent',
  color: theme.vars.palette.text.secondary,
  textTransform: 'none',
  border: `.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusLg,
  marginTop: theme.spacing(0.75),
  gap: 0,
  [`& .${buttonClasses.startIcon}`]: {
    marginRight: theme.spacing(0.5),
  },
  '&:hover': {
    background: theme.vars.palette.background.button.secondary.hover,
    border: `.0625rem solid ${theme.vars.palette.border.lines}`,
  },
});
