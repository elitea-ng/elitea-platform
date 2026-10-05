import type { ReactNode } from 'react';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import { t } from '@/shared/i18n';
import { extensionRecord, extensionValues, extensionText } from '../../../lib/graphExtensions.helpers';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';

interface AggregateGroupingProps {
  readonly value: unknown;
  readonly disabled: boolean;
  readonly change: (rows: readonly unknown[]) => void;
}
export function AggregateGrouping({ value, disabled, change }: AggregateGroupingProps): ReactNode {
  const original = extensionValues(value);
  const rows = original.map(extensionRecord);
  const update = (index: number, field: string, next: string): void => {
    change(original.map((row, ordinal) => ordinal === index ? { ...extensionRecord(row), [field]: next } : row));
  };
  return <Stack spacing={2}>
    {rows.map((row, index) => <Stack key={index} spacing={1}>
      <ExtensionField label={t('pipelines.graphExtensions.groupPointer', 'Group field pointer {{index}}', { index: index + 1 })}
        value={extensionText(row['path'])} disabled={disabled} change={(next) => update(index, 'path', next)} />
      <ExtensionField label={t('pipelines.graphExtensions.groupName', 'Group key name {{index}}', { index: index + 1 })}
        value={extensionText(row['output'])} disabled={disabled} change={(next) => update(index, 'output', next)} />
      <ExtensionChoice label={t('pipelines.graphExtensions.groupMissing', 'Missing group field {{index}}', { index: index + 1 })}
        value={extensionText(row['missing']) || 'error'} choices={['error', 'null']} disabled={disabled}
        change={(next) => update(index, 'missing', next)} />
      <Button disabled={disabled} onClick={() => change(original.filter((_, ordinal) => ordinal !== index))}>
        {t('pipelines.graphExtensions.removeGroup', 'Remove group field {{index}}', { index: index + 1 })}
      </Button>
    </Stack>)}
    <Button disabled={disabled || rows.length >= 64} onClick={() => change([...original, { path: '', output: '', missing: 'error' }])}>
      {t('pipelines.graphExtensions.addGroup', 'Add group field')}
    </Button>
  </Stack>;
}
