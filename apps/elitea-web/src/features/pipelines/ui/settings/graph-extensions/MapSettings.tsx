import type { ReactNode } from 'react';
import { useContext } from 'react';
import Box from '@mui/material/Box';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { MAP_WORKER_TYPES } from '../../../lib/graphMapAdmission.helpers';
import { extensionNumberText, extensionStrings, extensionText, extensionValues, patchGraphExtensionNode } from '../../../lib/graphExtensions.helpers';
import type { ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';
import { MapWorkerInputs } from './MapWorkerInputs';
import { StateChannelSelect } from './StateChannelSelect';

/** A declared, non-null key: a YAML null is absent, as the Worker reads it. */
function present(node: Readonly<Record<string, unknown>>, field: string): boolean {
  return Object.hasOwn(node, field) && node[field] !== null && node[field] !== undefined;
}

/**
 * The stored On Error. Absent means `continue` in the v2 shape; a v1 document
 * (`reduction` without `on_error`) stops on the first failed item.
 */
function onErrorOf(node: Readonly<Record<string, unknown>>): string {
  if (present(node, 'on_error')) return extensionText(node['on_error']);
  return present(node, 'reduction') ? 'stop' : 'continue';
}
const policyOption = (value: string): { value: string; label: string } => ({
  value, label: t(`pipelines.graphExtensions.values.${value}`, value),
});

function MapBasicFields({
  node,
  disabled,
  change,
  worker,
  workers,
  source,
  destination,
  join,
  workerNode,
  rawOutputs,
  selectedOutputs,
}: {
  readonly node: ExtensionSettingsProps['node'];
  readonly disabled: boolean;
  readonly change: ExtensionSettingsProps['change'];
  readonly worker: string;
  readonly workers: readonly { readonly id: string; readonly output?: unknown }[];
  readonly source: string;
  readonly destination: string;
  readonly join: string;
  readonly workerNode: { readonly output?: unknown } | undefined;
  readonly rawOutputs: readonly unknown[];
  readonly selectedOutputs: readonly string[];
}): ReactNode {
  return <>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.mapDescription', 'Run the same owned worker once per frozen list item. Preserve input order and original item identity.')}</Typography>
    <ExtensionChoice label={t('pipelines.graphExtensions.worker', 'Owned worker')} value={worker} disabled={disabled}
      choices={['', ...workers.map((entry) => entry.id)]} change={(value) => change('worker', value)}
      help={t('pipelines.graphExtensions.workerHelp', 'The worker must have no transition, parent route, or static pause. It can be a State Modifier, an LLM node, a Code node, or a saved agent or pipeline.')} />
    <StateChannelSelect label={t('pipelines.graphExtensions.source', 'Source state variable')} value={source} types={['list']}
      disabled={disabled} change={(value) => change('source', value)} />
    <ExtensionField label={t('pipelines.graphExtensions.itemChannel', 'Child item channel')} value={extensionText(node['item'])}
      disabled={disabled} change={(value) => change('item', value)}
      help={t('pipelines.graphExtensions.itemHelp', 'The complete item remains opaque JSON. This channel exists only inside the worker invocation.')} />
    <ExtensionField label={t('pipelines.graphExtensions.indexChannel', 'Child index channel')} value={extensionText(node['index'])}
      disabled={disabled} change={(value) => change('index', value)}
      help={t('pipelines.graphExtensions.indexHelp', 'The runtime supplies the original integer index. Do not declare this channel in parent State.')} />
    <MapWorkerInputs owner={node} disabled={disabled} />
    {selectedOutputs.map((key, index) => <Stack key={index} spacing={1}>
      <ExtensionChoice label={t('pipelines.graphExtensions.workerOutput', 'Worker output {{index}}', { index: index + 1 })} value={key}
        disabled={disabled} choices={['', ...extensionStrings(workerNode?.output)]}
        change={(value) => change('outputs', rawOutputs.map((entry, ordinal) => ordinal === index ? value : entry))} />
      <BaseBtn variant="text" size="small" className="nodrag nopan nowheel" disabled={disabled} onClick={() => change('outputs', rawOutputs.filter((_, ordinal) => ordinal !== index))}>
        {t('pipelines.graphExtensions.removeWorkerOutput', 'Remove worker output {{index}}', { index: index + 1 })}
      </BaseBtn>
    </Stack>)}
    <BaseBtn variant="secondary" size="small" className="nodrag nopan nowheel" disabled={disabled || selectedOutputs.length >= 64} onClick={() => change('outputs', [...rawOutputs, ''])}>
      {t('pipelines.graphExtensions.addWorkerOutput', 'Add worker output')}
    </BaseBtn>
    <StateChannelSelect label={t('pipelines.graphExtensions.mapDestination', 'Result list state variable')} value={destination}
      types={join === 'append' ? ['list'] : ['dict']} disabled={disabled} change={(value) => change('destination', value)} />
  </>;
}

function MapAdvancedFields({
  node,
  disabled,
  change,
  changePolicy,
  onError,
  join,
  legacyReduction,
  rawBroadcast,
  broadcast,
}: {
  readonly node: ExtensionSettingsProps['node'];
  readonly disabled: boolean;
  readonly change: ExtensionSettingsProps['change'];
  readonly changePolicy: (field: 'on_error' | 'join', value: string) => void;
  readonly onError: string;
  readonly join: string;
  readonly legacyReduction: boolean;
  readonly rawBroadcast: readonly unknown[];
  readonly broadcast: readonly string[];
}): ReactNode {
  return <>
    {broadcast.map((key, index) => <Stack key={index} spacing={1}>
      <StateChannelSelect label={t('pipelines.graphExtensions.broadcastChannel', 'Broadcast state variable {{index}}', { index: index + 1 })}
        value={key} types={['str', 'string', 'int', 'number', 'float', 'bool', 'list', 'dict']} disabled={disabled}
        change={(value) => change('broadcast', rawBroadcast.map((entry, ordinal) => ordinal === index ? value : entry))} />
      <BaseBtn variant="text" size="small" className="nodrag nopan nowheel" disabled={disabled} onClick={() => change('broadcast', rawBroadcast.filter((_, ordinal) => ordinal !== index))}>
        {t('pipelines.graphExtensions.removeBroadcast', 'Remove broadcast {{index}}', { index: index + 1 })}
      </BaseBtn>
    </Stack>)}
    <BaseBtn variant="secondary" size="small" className="nodrag nopan nowheel" disabled={disabled || broadcast.length >= 64} onClick={() => change('broadcast', [...rawBroadcast, ''])}>
      {t('pipelines.graphExtensions.addBroadcast', 'Add broadcast state variable')}
    </BaseBtn>
    <Box className="nodrag nopan nowheel">
      <SingleSelect label={t('pipelines.graphExtensions.mapOnError', 'On Error')} value={onError} disabled={disabled}
        options={['continue', 'stop'].map(policyOption)}
        onChange={(value) => changePolicy('on_error', value)} />
    </Box>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.mapOnErrorHelp', 'Continue keeps the other items and returns partial results. Stop fails the node after running items finish.')}</Typography>
    <Box className="nodrag nopan nowheel">
      <SingleSelect label={t('pipelines.graphExtensions.mapJoin', 'Join')} value={join} disabled={disabled}
        options={['append', 'keyed'].map(policyOption)}
        onChange={(value) => changePolicy('join', value)} />
    </Box>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.mapJoinHelp', 'Append lists the item results in source order. Keyed maps them by item index.')}</Typography>
    {legacyReduction && <ExtensionChoice label={t('pipelines.graphExtensions.reduction', 'Reduction')} value={extensionText(node['reduction'])}
      disabled={disabled} choices={['ordered_collection']} change={(value) => change('reduction', value)}
      help={t('pipelines.graphExtensions.reductionHelp', 'Replace the destination once with ordered {index, outputs} records after every item succeeds. Selected parent output roots stay unchanged.')} />}
    <ExtensionField label={t('pipelines.graphExtensions.maxItems', 'Maximum items')} value={extensionNumberText(node['max_items'])}
      disabled={disabled} number change={(value) => change('max_items', value === '' ? '' : Number(value))}
      help={t('pipelines.graphExtensions.maxItemsHelp', 'Set an integer from 1 to 64.')} />
    <ExtensionField label={t('pipelines.graphExtensions.maxConcurrency', 'Maximum concurrent items')} value={extensionNumberText(node['max_concurrency'])}
      disabled={disabled} number change={(value) => change('max_concurrency', value === '' ? '' : Number(value))}
      help={t('pipelines.graphExtensions.maxConcurrencyHelp', 'Set an integer from 1 to 8.')} />
  </>;
}

export function MapSettings({ node, disabled, change, section = 'all' }: ExtensionSettingsProps): ReactNode {
  const showBasic = section === 'all' || section === 'basic';
  const showAdvanced = section === 'all' || section === 'advanced';
  const context = useContext(FlowEditorContext);
  const document = context?.yamlJsonObject;
  const worker = extensionText(node['worker']);
  const workers = document?.nodes?.filter((entry) => entry.id !== node['id'] && MAP_WORKER_TYPES.includes(entry.type ?? '')) ?? [];
  const onError = onErrorOf(node);
  const join = present(node, 'join') ? extensionText(node['join']) : 'append';
  const legacyReduction = present(node, 'reduction');
  const changePolicy = (field: 'on_error' | 'join', value: string): void => {
    if (disabled) return;
    if (!context || !present(node, 'reduction')) {
      change(field, value);
      return;
    }
    const id = extensionText(node['id']);
    let next = patchGraphExtensionNode(context.yamlJsonObject, id, 'reduction', undefined, true);
    if (field === 'join' && !present(node, 'on_error')) next = patchGraphExtensionNode(next, id, 'on_error', onError);
    context.setYamlJsonObject(patchGraphExtensionNode(next, id, field, value));
  };
  const workerNode = workers.find((entry) => entry.id === worker);
  const rawOutputs = extensionValues(node['outputs']);
  const rawBroadcast = extensionValues(node['broadcast']);
  return <Stack spacing={2}>
    {showBasic && (
      <MapBasicFields
        node={node}
        disabled={disabled}
        change={change}
        worker={worker}
        workers={workers}
        source={extensionText(node['source'])}
        destination={extensionText(node['destination'])}
        join={join}
        workerNode={workerNode}
        rawOutputs={rawOutputs}
        selectedOutputs={rawOutputs.map(extensionText)}
      />
    )}
    {showAdvanced && (
      <MapAdvancedFields
        node={node}
        disabled={disabled}
        change={change}
        changePolicy={changePolicy}
        onError={onError}
        join={join}
        legacyReduction={legacyReduction}
        rawBroadcast={rawBroadcast}
        broadcast={rawBroadcast.map(extensionText)}
      />
    )}
  </Stack>;
}
