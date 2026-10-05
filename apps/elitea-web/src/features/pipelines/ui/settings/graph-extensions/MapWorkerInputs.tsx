import type { ReactNode } from 'react';
import { useContext } from 'react';
import Button from '@mui/material/Button';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { mapOwnedWorkerChannels } from '../../../lib/graphMapAdmission.helpers';
import { extensionRecord, extensionStrings, extensionText, extensionValues, patchGraphExtensionNode } from '../../../lib/graphExtensions.helpers';
import type { YamlInputMappingEntry, YamlPipelineNode } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { ExtensionRecord } from '../../../lib/graphExtensions.types';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';

interface MapWorkerInputsProps {
  readonly owner: ExtensionRecord;
  readonly disabled: boolean;
}
interface AgentTaskProps {
  readonly worker: YamlPipelineNode;
  readonly channels: readonly string[];
  readonly disabled: boolean;
  readonly change: (mapping: YamlInputMappingEntry) => void;
}
function AgentTask({ worker, channels, disabled, change }: AgentTaskProps): ReactNode {
  const mapping = extensionRecord(worker.input_mapping?.['task']);
  const type = extensionText(mapping['type']) || 'fixed';
  const nextMapping = (value: string): YamlInputMappingEntry => ({ ...mapping, type, value });
  return <Stack spacing={1}>
    <ExtensionChoice label={t('pipelines.graphExtensions.workerTaskType', 'Worker task mapping')} value={type}
      choices={['fixed', 'variable', 'fstring']} disabled={disabled}
      change={(next) => change({ ...mapping, type: next, value: extensionText(mapping['value']) })} />
    {type === 'variable'
      ? <ExtensionChoice label={t('pipelines.graphExtensions.workerTaskChannel', 'Worker task channel')}
        value={extensionText(mapping['value'])} choices={['', ...channels]} disabled={disabled}
        change={(value) => change(nextMapping(value))} />
      : <ExtensionField label={t('pipelines.graphExtensions.workerTask', 'Worker task value')}
        value={extensionText(mapping['value'])} disabled={disabled} multiline
        change={(value) => change(nextMapping(value))}
        help={t('pipelines.graphExtensions.workerTaskHelp', 'Use an explicit task. A field template can read the child item, index, and approved broadcasts.')} />}
  </Stack>;
}
export function MapWorkerInputs({ owner, disabled }: MapWorkerInputsProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const workerId = extensionText(owner['worker']);
  const worker = context?.yamlJsonObject.nodes?.find((node) => node.id === workerId);
  const local = mapOwnedWorkerChannels(context?.yamlJsonObject ?? {}, workerId);
  if (!context || !worker || local.length !== 2) return null;
  const approved = [...new Set([...local, ...extensionStrings(owner['broadcast']).filter((key) => Object.hasOwn(context.yamlJsonObject.state ?? {}, key))])];
  const selected = extensionValues(worker.input);
  const changeInput = (key: string, checked: boolean): void => {
    const next = checked ? [...selected, key] : selected.filter((entry) => entry !== key);
    context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, workerId, 'input', next));
  };
  const removeTransition = (): void => {
    context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, workerId, 'transition', undefined, true));
  };
  const hasTransition = typeof worker.transition === 'string';
  return <Stack spacing={1}>
    <Typography variant="bodySmall">{t('pipelines.graphExtensions.ownedInputScope', 'Worker inputs belong only to {{worker}}. Parent State remains unchanged.', { worker: workerId })}</Typography>
    {worker.type === 'state_modifier' && approved.map((key) => <FormControlLabel key={key} label={key}
      control={<BaseCheckbox className="nodrag nopan" disabled={disabled} checked={selected.includes(key)}
        onChange={(_, checked) => changeInput(key, checked)} />} />)}
    {worker.type === 'agent' && <AgentTask worker={worker} channels={approved} disabled={disabled}
      change={(mapping) => context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, workerId,
        'input_mapping', { ...worker.input_mapping, task: mapping }))} />}
    {hasTransition && <Button disabled={disabled} onClick={removeTransition}>
      {t('pipelines.graphExtensions.removeWorkerTransition', 'Remove owned worker transition')}
    </Button>}
  </Stack>;
}
