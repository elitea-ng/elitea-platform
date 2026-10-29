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
/**
 * The stored On Error (`on_error`, contract v1.1 §8a). Absent means `continue`;
 * the legacy `error_policy: fail_after_drain` is the old spelling of `stop`.
 */
function onErrorOf(node: Readonly<Record<string, unknown>>): string {
  if (Object.hasOwn(node, 'on_error')) return extensionText(node['on_error']);
  return node['error_policy'] === 'fail_after_drain' ? 'stop' : 'continue';
}

/** Fixed branches own declared Agents. They have no Map bindings or reducer contract. */
export function ParallelSettings({ node, disabled, change, remove }: ParallelSettingsProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const guardedChange = (field: string, value: unknown): void => { if (!disabled) change(field, value); };
  const onError = onErrorOf(node);
  const join = Object.hasOwn(node, 'join') ? extensionText(node['join']) : 'append';
  // One YAML write per pick. Continue on a legacy `error_policy` node is the
  // removal of that key (absent = continue); `merge` needs `stop`, so the two
  // selectors never offer a combination the Worker refuses.
  const changeOnError = (value: string): void => {
    if (disabled) return;
    if (value === 'continue' && Object.hasOwn(node, 'error_policy') && !Object.hasOwn(node, 'on_error')) remove('error_policy');
    else change('on_error', value);
  };
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
    <ExtensionChoice label={t('pipelines.parallel.onError', 'On Error')} value={onError}
      choices={join === 'merge' ? ['stop'] : ['continue', 'stop']} disabled={disabled} change={changeOnError}
      help={t('pipelines.parallel.onErrorHelp', 'Continue keeps the other branches and returns partial results. Stop fails the node after running branches finish.')} />
    <ExtensionChoice label={t('pipelines.parallel.join', 'Join')} value={join}
      choices={onError === 'stop' ? ['append', 'keyed', 'merge'] : ['append', 'keyed']} disabled={disabled}
      change={(value) => guardedChange('join', value)}
      help={t('pipelines.parallel.joinHelp', 'Append lists the results in branch order, Keyed maps them by result key, Merge combines their outputs and needs Stop.')} />
    <StateChannelSelect label={t('pipelines.parallel.output', 'Ordered list output')}
      value={extensionText(extensionValues(node['output'])[0])} types={['list']} disabled={disabled}
      change={(value) => guardedChange('output', [value])} />
    <Typography variant="bodySmall">{t('pipelines.parallel.resultEntry', 'Each result contains branch_id, node and status, with outputs from that Agent or a readable error.')}</Typography>
    <ExtensionChoice label={t('pipelines.parallel.transition', 'Join transition')} value={extensionText(node['transition'])}
      choices={['', ...targets]} disabled={disabled} change={(value) => guardedChange('transition', value === '' ? null : value)} />
  </Stack>;
}
