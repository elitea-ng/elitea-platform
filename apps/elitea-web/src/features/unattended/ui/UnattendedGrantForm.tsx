/**
 * The unattended grant's form (ADR-0045 decision 1), controlled: each part
 * with what it answers and what happens without the grant, the sensitive
 * tools (only when `sensitiveTools` is given) and the chain limits. The
 * desktop-only parts (the tool-call cap, hook questions, remote
 * confirmations) hide with `desktop={false}`; the cloud-only output limit
 * shows with `cloud`.
 */
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { chainErrors, type ChainField, type SensitiveToolRef, type UnattendedGrantValue } from '../model/grant';
import { CHAIN_PARTS, sensitiveHelp, sensitiveLabel, visibleParts, type SwitchPart } from './grantParts';

/** A sensitive tool the grant may name; `label` defaults to `toolkit/tool`. */
export interface SensitiveToolOption extends SensitiveToolRef {
  label?: string;
}

export interface UnattendedGrantFormProps {
  value: UnattendedGrantValue;
  onChange: (next: UnattendedGrantValue) => void;
  /** Shows the desktop-only parts (default true). */
  desktop?: boolean;
  /** Shows the cloud-only part, the output limit (default false). */
  cloud?: boolean;
  /** The sensitive tools the grant may name; the list hides when this is absent. */
  sensitiveTools?: readonly SensitiveToolOption[];
  disabled?: boolean;
}

const muted = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

const sameTool = (a: SensitiveToolRef, b: SensitiveToolRef): boolean => a.toolkit === b.toolkit && a.tool === b.tool;

function Help({ children }: { children: string }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component="p" sx={(theme: Theme) => ({ ...muted(theme), marginLeft: 5.5, marginTop: -0.5 })}>
      {children}
    </Typography>
  );
}

interface SectionProps {
  value: UnattendedGrantValue;
  onChange: (next: UnattendedGrantValue) => void;
  disabled: boolean;
}

function SensitiveTools({ value, onChange, disabled, options }: SectionProps & { options: readonly SensitiveToolOption[] }): React.JSX.Element {
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
      {options.map((option) => (
        <FormControlLabel
          key={`${option.toolkit}/${option.tool}`}
          disabled={disabled}
          control={<BaseCheckbox size="small" checked={value.sensitive_tools.some((named) => sameTool(named, option))} onChange={(_event, on) => toggle(option, on)} />}
          label={<Typography variant="bodySmall">{option.label ?? `${option.toolkit}/${option.tool}`}</Typography>}
        />
      ))}
    </Box>
  );
}

function ChainLimits({ value, onChange, disabled }: SectionProps): React.JSX.Element {
  const invalid = chainErrors(value.chain);
  const set = (field: ChainField, text: string): void => onChange({ ...value, chain: { ...value.chain, [field]: text } });
  return (
    <Box component="fieldset" sx={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Typography component="legend" variant="labelSmall">
        {t('unattended.chain.title', 'Chain limits')}
      </Typography>
      <Typography variant="bodySmall" component="p" sx={muted}>
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

export function UnattendedGrantForm({ value, onChange, desktop = true, cloud = false, sensitiveTools, disabled = false }: UnattendedGrantFormProps): React.JSX.Element {
  const set = (key: SwitchPart, on: boolean): void => onChange({ ...value, [key]: on });
  return (
    <Box data-testid="unattended-grant-form" sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
        {visibleParts({ desktop, cloud }).map((part) => (
          <Box key={part.key}>
            <FormControlLabel
              disabled={disabled}
              control={<Switch size="small" checked={value[part.key]} onChange={(_event, on) => set(part.key, on)} />}
              label={<Typography variant="bodyMedium">{part.label()}</Typography>}
            />
            <Help>{part.help()}</Help>
          </Box>
        ))}
      </Box>
      {sensitiveTools !== undefined && <SensitiveTools value={value} onChange={onChange} disabled={disabled} options={sensitiveTools} />}
      <ChainLimits value={value} onChange={onChange} disabled={disabled} />
    </Box>
  );
}
