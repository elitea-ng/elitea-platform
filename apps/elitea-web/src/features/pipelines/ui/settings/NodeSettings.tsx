/**
 * The per-node "Settings" section (5b-1, PLAN §3.9): On error, Always output
 * data and Run once per execution, for the node types the contract admits
 * (`pipeline-node-settings-v1.json` `families`). Built only from existing
 * controls: `BaseSwitch` rows as `CommonInterruptSettings`, `SingleSelect` as
 * `MapSettings`. A setting returned to its default leaves no `settings:` key.
 * Refusals are shown by `NodeAdmissionIssues` from `nodeSettings.helpers.ts`.
 */
import type { ReactNode } from 'react';
import { useContext, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Collapse from '@mui/material/Collapse';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { ERROR_OUTPUT_HANDLE } from '../../lib/flow-editor/helpers/nodeErrorRoute.helpers';
import { NODE_ON_ERROR, NODE_SETTINGS_KEY, NodeSettingsFamilies, readNodeSettings, writeNodeSettings } from '../../lib/nodeSettings.helpers';

export interface NodeSettingsProps {
  readonly id: string;
  readonly type: string;
  readonly disabled: boolean;
}

const columnSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', width: '100%' };
const rowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.5rem', width: '100%', flexDirection: 'row' };

function switchLabelSx(theme: Theme) {
  return {
    width: '13.375rem',
    height: '2rem',
    borderRadius: theme.vars.shape.radiusMd,
    marginLeft: '0rem',
    marginRight: '0rem',
    padding: '0.25rem 0.5rem',
    justifyContent: 'flex-start',
    gap: '0.5rem',
    background: theme.vars.palette.background.userInputBackground,
  };
}

function SettingSwitch({ label, checked, disabled, onChange }: {
  readonly label: string; readonly checked: boolean; readonly disabled: boolean; readonly onChange: (checked: boolean) => void;
}): ReactNode {
  return (
    <FormControlLabel
      sx={switchLabelSx}
      control={<BaseSwitch disabled={disabled} checked={checked} onChange={(event) => onChange(event.target.checked)} />}
      label={<Typography variant="labelSmall" color="text.secondary">{label}</Typography>}
      labelPlacement="end"
    />
  );
}

const onErrorLabel = (value: string): string => {
  switch (value) {
    case 'continue':
      return t('pipelines.nodeSettings.onErrorContinue', 'Continue');
    case 'error_output':
      return t('pipelines.nodeSettings.onErrorErrorOutput', 'Continue (using error output)');
    default:
      return t('pipelines.nodeSettings.onErrorStop', 'Stop pipeline');
  }
};

export function NodeSettings({ id, type, disabled }: NodeSettingsProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const [open, setOpen] = useState(false);
  const node = context?.yamlJsonObject.nodes?.find((entry) => entry.id === id);
  if (!context || !node || NodeSettingsFamilies[type] === undefined) return null;
  const settings = readNodeSettings(node);
  const change = (key: string, value: unknown): void => {
    const next = writeNodeSettings(node[NODE_SETTINGS_KEY] as Readonly<Record<string, unknown>> | undefined, { [key]: value });
    FlowEditorHelpers.batchUpdateYamlNode(id, { [NODE_SETTINGS_KEY]: next }, context.yamlJsonObject, context.setYamlJsonObject);
    // The error edge is `settings.error_transition`: it leaves with the key.
    if (next?.['error_transition'] === undefined) {
      context.setFlowEdges((edges) => edges.filter((edge) => !(edge.source === id && edge.sourceHandle === ERROR_OUTPUT_HANDLE)));
    }
  };
  return (
    <Box sx={columnSx} className="nodrag nopan">
      <Button size="small" variant="text" onClick={() => setOpen((value) => !value)} aria-expanded={open} sx={{ alignSelf: 'flex-start' }}>
        {t('pipelines.nodeSettings.title', 'Settings')}
      </Button>
      <Collapse in={open} unmountOnExit>
        <Box sx={columnSx}>
          <SingleSelect
            label={t('pipelines.nodeSettings.onError', 'On error')}
            value={settings.on_error}
            disabled={disabled}
            options={NODE_ON_ERROR.map((value) => ({ value, label: onErrorLabel(value) }))}
            onChange={(value) => change('on_error', value)}
          />
          <Box sx={rowSx}>
            <SettingSwitch
              label={t('pipelines.nodeSettings.alwaysOutputData', 'Always output data')}
              checked={settings.always_output_data}
              disabled={disabled}
              onChange={(checked) => change('always_output_data', checked)}
            />
            <SettingSwitch
              label={t('pipelines.nodeSettings.executeOnce', 'Run once per execution')}
              checked={settings.execute_once}
              disabled={disabled}
              onChange={(checked) => change('execute_once', checked)}
            />
          </Box>
        </Box>
      </Collapse>
    </Box>
  );
}
