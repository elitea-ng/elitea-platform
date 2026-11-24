/**
 * The per-node "Settings" section (5b-1, PLAN §3.9): On error, Always output
 * data and Run once per execution, plus Retry on fail, Max tries, Wait between
 * tries (ms) and Timeout (ms) for families whose contract admits them (Code).
 * Built only from existing controls: `BaseSwitch` rows, `SingleSelect`, and
 * `TextField`. A setting returned to its default leaves no `settings:` key.
 * Refusals are shown by `NodeAdmissionIssues` from `nodeSettings.helpers.ts`.
 */
import type { ReactNode } from 'react';
import { useContext, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Collapse from '@mui/material/Collapse';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { ERROR_OUTPUT_HANDLE } from '../../lib/flow-editor/helpers/nodeErrorRoute.helpers';
import {
  NODE_ON_ERROR,
  NODE_SETTINGS_KEY,
  NodeSettingsLimits,
  nodeSettingsFamilyKeys,
  type NodeSettings as ParsedNodeSettings,
  readNodeSettings,
  writeNodeSettings,
} from '../../lib/nodeSettings.helpers';

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

function parseOptionalInteger(raw: string): number | undefined {
  const trimmed = raw.trim();
  if (trimmed === '') return undefined;
  const parsed = Number(trimmed);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}

interface SettingNumberFieldProps {
  readonly label: string;
  readonly value: number | undefined;
  readonly min: number;
  readonly max: number;
  readonly placeholder?: string;
  readonly helperText: string;
  readonly disabled: boolean;
  readonly onChange: (value: number | undefined) => void;
}

function SettingNumberField({ label, value, min, max, placeholder, helperText, disabled, onChange }: SettingNumberFieldProps): ReactNode {
  return (
    <TextField
      className="nodrag nopan nowheel"
      type="number"
      size="small"
      fullWidth
      label={label}
      value={value === undefined ? '' : String(value)}
      placeholder={placeholder}
      helperText={helperText}
      disabled={disabled}
      slotProps={{ htmlInput: { min, max, step: 1 } }}
      onChange={(event) => onChange(parseOptionalInteger(event.target.value))}
    />
  );
}

interface ExecutionTimingControlsProps {
  readonly nodeType: string;
  readonly settings: ParsedNodeSettings;
  readonly supportsRetry: boolean;
  readonly supportsTimeout: boolean;
  readonly disabled: boolean;
  readonly onChange: (key: string, value: unknown) => void;
}

function ExecutionTimingControls({ nodeType, settings, supportsRetry, supportsTimeout, disabled, onChange }: ExecutionTimingControlsProps): ReactNode {
  if (!supportsRetry && !supportsTimeout) return null;
  const isHttp = nodeType === 'http';
  const maxTimeoutMs = isHttp ? NodeSettingsLimits.max_http_timeout_ms : NodeSettingsLimits.max_node_timeout_ms;
  const timeoutHelp = isHttp
    ? t('pipelines.nodeSettings.httpTimeoutMsHelp', '1000 to 30000 ms')
    : t('pipelines.nodeSettings.timeoutMsHelp', '1000 to 900000 ms (clamped to sandbox profile limit)');
  return (
    <>
      {supportsRetry && (
        <SettingSwitch
          label={t('pipelines.nodeSettings.retryOnFail', 'Retry on fail')}
          checked={settings.retry_on_fail}
          disabled={disabled}
          onChange={(checked) => onChange('retry_on_fail', checked)}
        />
      )}
      {supportsRetry && settings.retry_on_fail && (
        <Box sx={columnSx}>
          <SettingNumberField
            label={t('pipelines.nodeSettings.maxTries', 'Max tries')}
            value={settings.max_tries}
            min={NodeSettingsLimits.min_node_tries}
            max={NodeSettingsLimits.max_node_tries}
            placeholder="3"
            helperText={t('pipelines.nodeSettings.maxTriesHelp', '2 to 5 (default 3)')}
            disabled={disabled}
            onChange={(value) => onChange('max_tries', value)}
          />
          <SettingNumberField
            label={t('pipelines.nodeSettings.waitBetweenTriesMs', 'Wait between tries (ms)')}
            value={settings.wait_between_tries_ms}
            min={NodeSettingsLimits.min_wait_between_tries_ms}
            max={NodeSettingsLimits.max_wait_between_tries_ms}
            placeholder={String(NodeSettingsLimits.default_wait_between_tries_ms)}
            helperText={t('pipelines.nodeSettings.waitBetweenTriesMsHelp', '100 to 300000 ms (default 1000)')}
            disabled={disabled}
            onChange={(value) => onChange('wait_between_tries_ms', value)}
          />
        </Box>
      )}
      {supportsTimeout && (
        <SettingNumberField
          label={t('pipelines.nodeSettings.timeoutMs', 'Timeout (ms)')}
          value={settings.timeout_ms}
          min={NodeSettingsLimits.min_node_timeout_ms}
          max={maxTimeoutMs}
          helperText={timeoutHelp}
          disabled={disabled}
          onChange={(value) => onChange('timeout_ms', value)}
        />
      )}
    </>
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
  const allowedKeys = nodeSettingsFamilyKeys(type);
  if (!context || !node || allowedKeys === undefined) return null;
  const settings = readNodeSettings(node);
  const supportsRetry = allowedKeys.includes('retry_on_fail');
  const supportsTimeout = allowedKeys.includes('timeout_ms');
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
          <ExecutionTimingControls
            nodeType={type}
            settings={settings}
            supportsRetry={supportsRetry}
            supportsTimeout={supportsTimeout}
            disabled={disabled}
            onChange={change}
          />
        </Box>
      </Collapse>
    </Box>
  );
}
