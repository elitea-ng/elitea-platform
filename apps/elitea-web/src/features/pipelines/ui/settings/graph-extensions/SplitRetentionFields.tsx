import type { ReactNode } from 'react';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import { t } from '@/shared/i18n';
import { extensionRecord, extensionValues, extensionStrings, extensionText, patchExtensionRecord } from '../../../lib/graphExtensions.helpers';
import type { ExtensionRecord } from '../../../lib/graphExtensions.types';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';

interface SplitRetentionFieldsProps {
  readonly value: unknown;
  readonly defaultMode: 'all' | 'none';
  readonly choices?: readonly string[];
  readonly disabled: boolean;
  readonly change: (value: ExtensionRecord) => void;
}
export function SplitRetentionFields({ value, defaultMode, disabled, change, choices = ['none', 'all', 'only', 'except'] }: SplitRetentionFieldsProps): ReactNode {
  const retain = extensionRecord(value);
  const mode = extensionText(retain['mode']) || defaultMode;
  const original = extensionValues(retain['fields']);
  const rows = original.map(extensionRecord);
  const changeMode = (next: string) => {
    let updated = patchExtensionRecord(retain, 'mode', next);
    if (next === 'none' || next === 'all') updated = patchExtensionRecord(updated, 'fields', null, true);
    else if (next !== mode || !Object.hasOwn(retain, 'fields')) updated = patchExtensionRecord(updated, 'fields', []);
    change(updated);
  };
  return <Stack spacing={2}>
    <ExtensionChoice label={t('pipelines.graphExtensions.retainMode', 'Retain fields')} value={mode} disabled={disabled}
      choices={choices} change={changeMode} />
    {mode === 'except' && <ExtensionField label={t('pipelines.graphExtensions.exceptFields', 'Exclude top-level fields')}
      value={extensionStrings(retain['fields']).join('\n')} disabled={disabled} multiline
      help={t('pipelines.graphExtensions.literalNames', 'Enter one literal field name per line. Dots do not select nested fields.')}
      change={(next) => change({ ...retain, fields: next.split('\n').filter((name) => name !== '') })} />}
    {mode === 'only' && rows.map((row, index) => <Stack key={index} spacing={1}>
      <ExtensionField label={t('pipelines.graphExtensions.retainPath', 'Retained field pointer {{index}}', { index: index + 1 })}
        value={extensionText(row['path'])} disabled={disabled}
        change={(path) => change({ ...retain, fields: original.map((entry, ordinal) => ordinal === index ? { ...extensionRecord(entry), path } : entry) })} />
      <ExtensionField label={t('pipelines.graphExtensions.retainOutput', 'Retained field name {{index}}', { index: index + 1 })}
        value={extensionText(row['output'])} disabled={disabled}
        change={(output) => change({ ...retain, fields: original.map((entry, ordinal) => ordinal === index ? { ...extensionRecord(entry), output } : entry) })} />
      <ExtensionChoice label={t('pipelines.graphExtensions.retainMissing', 'Missing retained field {{index}}', { index: index + 1 })}
        value={extensionText(row['missing']) || 'error'} disabled={disabled} choices={['error', 'null', 'skip']}
        change={(missing) => change({ ...retain, fields: original.map((entry, ordinal) => ordinal === index ? { ...extensionRecord(entry), missing } : entry) })} />
      <Button disabled={disabled} onClick={() => change({ ...retain, fields: original.filter((_, ordinal) => ordinal !== index) })}>
        {t('pipelines.graphExtensions.removeRetainedField', 'Remove retained field {{index}}', { index: index + 1 })}
      </Button>
    </Stack>)}
    {mode === 'only' && <Button disabled={disabled || rows.length >= 64}
      onClick={() => change({ ...retain, fields: [...original, { path: '', output: '', missing: 'error' }] })}>
      {t('pipelines.graphExtensions.addRetainedField', 'Add retained field')}
    </Button>}
  </Stack>;
}
