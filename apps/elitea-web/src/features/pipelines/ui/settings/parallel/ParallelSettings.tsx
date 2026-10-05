import type { ReactNode } from 'react';
import { useContext } from 'react';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { extensionNumberText, extensionText, extensionValues } from '../../../lib/graphExtensions.helpers';
import { ExtensionChoice, ExtensionField } from '../graph-extensions/ExtensionFields';
import { StateChannelSelect } from '../graph-extensions/StateChannelSelect';
import { ParallelBranches } from './ParallelBranches';

export interface ParallelSettingsProps {
  readonly node: Readonly<Record<string, unknown>>;
  readonly disabled: boolean;
  readonly change: (field: string, value: unknown) => void;
  readonly remove: (field: string) => void;
}
/** Fixed branches own declared Agents. They have no Map bindings or reducer contract. */
export function ParallelSettings({ node, disabled, change }: ParallelSettingsProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const guardedChange = (field: string, value: unknown): void => { if (!disabled) change(field, value); };
  const targets = ['END', ...(context?.yamlJsonObject.nodes ?? []).map((entry) => entry.id)];
  return <Stack spacing={2}>
    <Typography variant="bodySmall">{t('pipelines.parallel.fixedOrder', 'Declare 2 to 16 fixed Agent branches. The list result follows branch order and keeps each branch result key.')}</Typography>
    <ParallelBranches ownerId={extensionText(node['id'])} branches={node['branches']} disabled={disabled}
      change={(branches) => guardedChange('branches', branches)} />
    <ExtensionField label={t('pipelines.parallel.concurrency', 'Maximum concurrent branches')}
      value={extensionNumberText(node['max_concurrency'])} number disabled={disabled}
      change={(value) => guardedChange('max_concurrency', value === '' ? '' : Number(value))}
      help={t('pipelines.parallel.concurrencyHelp', 'Use 1 to 8, within the declared branch count.')} />
    <ExtensionChoice label={t('pipelines.parallel.wait', 'Wait policy')} value={extensionText(node['wait'])}
      choices={['all']} disabled={disabled} change={(value) => guardedChange('wait', value)} />
    <ExtensionChoice label={t('pipelines.parallel.error', 'Error policy')}
      value={Object.hasOwn(node, 'error_policy') ? extensionText(node['error_policy']) : 'fail_after_drain'}
      choices={['fail_after_drain']} disabled={disabled} change={(value) => guardedChange('error_policy', value)} />
    <StateChannelSelect label={t('pipelines.parallel.output', 'Ordered list output')}
      value={extensionText(extensionValues(node['output'])[0])} types={['list']} disabled={disabled}
      change={(value) => guardedChange('output', [value])} />
    <Typography variant="bodySmall">{t('pipelines.parallel.resultShape', 'Each result contains branch_id, node, and outputs from that Agent. No reducer is configured.')}</Typography>
    <ExtensionChoice label={t('pipelines.parallel.transition', 'Join transition')} value={extensionText(node['transition'])}
      choices={['', ...targets]} disabled={disabled} change={(value) => guardedChange('transition', value === '' ? null : value)} />
  </Stack>;
}
