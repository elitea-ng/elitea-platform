import type { ReactNode } from 'react';
import TextField from '@mui/material/TextField';
import { t } from '@/shared/i18n';

export interface ExtensionFieldProps {
  readonly label: string;
  readonly value: string | number;
  readonly disabled: boolean;
  readonly change: (value: string) => void;
  readonly help?: string;
  readonly number?: boolean;
  readonly multiline?: boolean;
}
export function ExtensionField({ label, value, disabled, change, help, number, multiline }: ExtensionFieldProps): ReactNode {
  return <TextField className="nodrag nopan nowheel" label={label} value={value} disabled={disabled}
    onChange={(event) => change(event.target.value)} type={number ? 'number' : 'text'}
    helperText={help} multiline={multiline} size="small" fullWidth />;
}
export interface ExtensionChoiceProps extends Omit<ExtensionFieldProps, 'number'> {
  readonly choices: readonly string[];
}
export function ExtensionChoice({ label, value, disabled, change, help, choices }: ExtensionChoiceProps): ReactNode {
  return <TextField className="nodrag nopan nowheel" select label={label} value={value} disabled={disabled}
    onChange={(event) => change(event.target.value)} helperText={help} size="small" fullWidth
    slotProps={{ select: { native: true } }}>
    {!choices.includes(String(value)) && value !== '' && <option value={value}>{value}</option>}
    {choices.map((choice) => <option key={choice} value={choice}>{t(`pipelines.graphExtensions.values.${choice}`, choice)}</option>)}
  </TextField>;
}
