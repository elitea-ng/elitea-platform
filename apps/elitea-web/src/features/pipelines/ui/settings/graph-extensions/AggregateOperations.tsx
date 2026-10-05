import type { ReactNode } from 'react';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import Stack from '@mui/material/Stack';
import { t } from '@/shared/i18n';
import { extensionRecord, extensionValues, extensionText } from '../../../lib/graphExtensions.helpers';
import { AGGREGATE_OPERATIONS, type ExtensionRecord } from '../../../lib/graphExtensions.types';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';
import { SplitRetentionFields } from './SplitRetentionFields';

interface AggregateOperationsProps {
  readonly value: unknown;
  readonly disabled: boolean;
  readonly change: (rows: readonly unknown[]) => void;
}
function changedOperation(row: ExtensionRecord, operation: string): ExtensionRecord {
  const next: Record<string, unknown> = { ...row, operation };
  delete next['field']; delete next['retain']; delete next['merge_lists'];
  if (operation === 'collect_rows') next['retain'] = { mode: 'all' };
  else if (operation !== 'count_rows') next['field'] = { path: '', missing: 'error', null: 'keep' };
  if (operation === 'collect') next['merge_lists'] = false;
  return next;
}
interface AggregateOperationRowProps {
  readonly row: ExtensionRecord;
  readonly index: number;
  readonly disabled: boolean;
  readonly change: (row: ExtensionRecord) => void;
  readonly remove: () => void;
}
function AggregateOperationRow({ row, index, disabled, change, remove }: AggregateOperationRowProps): ReactNode {
  const operation = extensionText(row['operation']);
  const field = extensionRecord(row['field']);
  const numbered = { index: index + 1 };
  return <Stack spacing={1}>
    <ExtensionChoice label={t('pipelines.graphExtensions.operation', 'Operation {{index}}', numbered)} value={operation}
      choices={AGGREGATE_OPERATIONS} disabled={disabled} change={(next) => change(changedOperation(row, next))} />
    <ExtensionField label={t('pipelines.graphExtensions.operationOutput', 'Result field name {{index}}', numbered)}
      value={extensionText(row['output'])} disabled={disabled} change={(output) => change({ ...row, output })} />
    {operation !== 'count_rows' && operation !== 'collect_rows' && <>
      <ExtensionField label={t('pipelines.graphExtensions.operationPointer', 'Value field pointer {{index}}', numbered)}
        value={extensionText(field['path'])} disabled={disabled} change={(path) => change({ ...row, field: { ...field, path } })} />
      <ExtensionChoice label={t('pipelines.graphExtensions.operationMissing', 'Missing value {{index}}', numbered)}
        value={extensionText(field['missing']) || 'error'} choices={['error', 'null', 'skip']} disabled={disabled}
        change={(missing) => change({ ...row, field: { ...field, missing } })} />
      <ExtensionChoice label={t('pipelines.graphExtensions.operationNull', 'Null value {{index}}', numbered)}
        value={extensionText(field['null']) || 'keep'} choices={['keep', 'error', 'skip']} disabled={disabled}
        change={(value) => change({ ...row, field: { ...field, null: value } })} />
    </>}
    {operation === 'collect_rows' && <SplitRetentionFields value={row['retain']} defaultMode="all" disabled={disabled}
      change={(retain) => change({ ...row, retain })} />}
    {operation === 'collect' && <FormControlLabel label={t('pipelines.graphExtensions.mergeLists', 'Flatten collected lists {{index}}', numbered)}
      control={<Checkbox className="nodrag nopan" disabled={disabled} checked={row['merge_lists'] === true}
        onChange={(_, merge_lists) => change({ ...row, merge_lists })} />} />}
    <Button disabled={disabled} onClick={remove}>{t('pipelines.graphExtensions.removeOperation', 'Remove operation {{index}}', numbered)}</Button>
  </Stack>;
}
export function AggregateOperations({ value, disabled, change }: AggregateOperationsProps): ReactNode {
  const original = extensionValues(value);
  const rows = original.map(extensionRecord);
  return <Stack spacing={2}>
    {rows.map((row, index) => <AggregateOperationRow key={index} row={row} index={index} disabled={disabled}
      change={(next) => change(original.map((entry, ordinal) => ordinal === index ? next : entry))}
      remove={() => change(original.filter((_, ordinal) => ordinal !== index))} />)}
    <Button disabled={disabled || rows.length >= 64} onClick={() => change([...original, { operation: 'count_rows', output: '' }])}>
      {t('pipelines.graphExtensions.addOperation', 'Add operation')}
    </Button>
  </Stack>;
}
