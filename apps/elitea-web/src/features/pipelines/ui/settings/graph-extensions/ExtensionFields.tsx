import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import FormHelperText from '@mui/material/FormHelperText';
import { t } from '@/shared/i18n';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';

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
  return <InputBase className="nodrag nopan nowheel" label={label} value={value} disabled={disabled}
    onChange={(event) => change(event.target.value)} type={number ? 'number' : 'text'}
    helperText={help} {...(multiline ? { expand: { minRows: 2, maxRows: 6 } } : {})} size="small" fullWidth />;
}
export interface ExtensionChoiceProps extends Omit<ExtensionFieldProps, 'number' | 'multiline'> {
  readonly choices: readonly string[];
  readonly emptyLabel?: string;
}
function choiceOption(choice: string, emptyLabel: string): SingleSelectOption {
  if (choice === '') return { value: '', label: emptyLabel };
  return { value: choice, label: t(`pipelines.graphExtensions.values.${choice}`, choice) };
}
export function ExtensionChoice({ label, value, disabled, change, help, choices, emptyLabel }: ExtensionChoiceProps): ReactNode {
  const stringValue = String(value);
  const resolvedEmptyLabel = emptyLabel ?? t('shared.ui.singleSelect.placeholder', 'None');
  const options: SingleSelectOption[] = [
    ...(!choices.includes(stringValue) && stringValue !== '' ? [{ value: stringValue, label: stringValue }] : []),
    ...choices.map((choice) => choiceOption(choice, resolvedEmptyLabel)),
  ];
  return <Box className="nodrag nopan nowheel">
    <SingleSelect label={label} value={stringValue} disabled={disabled} options={options} onChange={change} />
    {help && <FormHelperText>{help}</FormHelperText>}
  </Box>;
}
