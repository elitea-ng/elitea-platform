/**
 * Two sections of the "Run to completion" popover: the sensitive tools that
 * the grant names (cloud) and the chain limits as typed fields.
 */
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { chainErrors, type ChainField, type SensitiveToolRef, type UnattendedGrantValue } from '../model/grant';
import { CHAIN_PARTS, sensitiveHelp, sensitiveLabel } from './grantParts';

/** A sensitive tool the grant may name; `label` defaults to `toolkit/tool`. */
export interface SensitiveToolOption extends SensitiveToolRef {
  label?: string;
}

export const muted = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

const sameTool = (a: SensitiveToolRef, b: SensitiveToolRef): boolean => a.toolkit === b.toolkit && a.tool === b.tool;

interface SectionProps {
  value: UnattendedGrantValue;
  onChange: (next: UnattendedGrantValue) => void;
  disabled: boolean;
}

export function SensitiveTools({ value, onChange, disabled, options }: SectionProps & { options: readonly SensitiveToolOption[] }): React.JSX.Element {
  const toggle = (tool: SensitiveToolRef, on: boolean): void => {
    const rest = value.sensitive_tools.filter((named) => !sameTool(named, tool));
    onChange({ ...value, sensitive_tools: on ? [...rest, { toolkit: tool.toolkit, tool: tool.tool }] : rest });
  };
  return (
    <Box component="fieldset" data-testid="unattended-sensitive-tools" sx={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column' }}>
      <Typography component="legend" variant="labelSmall">
        {sensitiveLabel()}
      </Typography>
      <Typography variant="bodySmall" component="p" sx={muted}>
        {sensitiveHelp()}
      </Typography>
      {options.length === 0 && (
        <Typography variant="bodySmall" sx={muted}>
          {t('unattended.sensitive.none', 'This agent has no sensitive tools.')}
        </Typography>
      )}
      <Box sx={{ display: 'flex', flexDirection: 'column', maxHeight: '10rem', overflowY: 'auto' }}>
        {options.map((option) => (
          <FormControlLabel
            key={`${option.toolkit}/${option.tool}`}
            disabled={disabled}
            control={<BaseCheckbox size="small" checked={value.sensitive_tools.some((named) => sameTool(named, option))} onChange={(_event, on) => toggle(option, on)} />}
            label={<Typography variant="bodySmall">{option.label ?? `${option.toolkit}/${option.tool}`}</Typography>}
          />
        ))}
      </Box>
    </Box>
  );
}

export function ChainLimits({ value, onChange, disabled }: SectionProps): React.JSX.Element {
  const invalid = chainErrors(value.chain);
  const set = (field: ChainField, text: string): void => onChange({ ...value, chain: { ...value.chain, [field]: text } });
  return (
    <Box component="fieldset" sx={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Typography component="legend" variant="bodySmall" sx={muted}>
        {t('unattended.chain.help', 'The run stops when it reaches one of these limits. An empty field takes the platform default.')}
      </Typography>
      {CHAIN_PARTS.map((part) => {
        const error = invalid.includes(part.field);
        return (
          <TextField
            key={part.field}
            size="small"
            label={part.label()}
            value={value.chain[part.field]}
            disabled={disabled}
            error={error}
            helperText={error ? part.error() : part.help()}
            onChange={(event) => set(part.field, event.target.value)}
            slotProps={{ htmlInput: { inputMode: 'decimal', 'data-testid': `unattended-chain-${part.field.replaceAll('_', '-')}` } }}
          />
        );
      })}
    </Box>
  );
}
