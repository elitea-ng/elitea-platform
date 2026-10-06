import type { ReactNode } from 'react';
import { useContext } from 'react';
import type { NodeProps } from '@xyflow/react';
import { useEdges } from '@xyflow/react';
import Alert from '@mui/material/Alert';
import { t } from '@/shared/i18n';
import { FIXED_PARALLEL_AUTHORING_ENABLED } from '../../lib/flow-editor/constants/parallel.constants';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { patchGraphExtensionNode } from '../../lib/graphExtensions.helpers';
import { ParallelSettings } from '../settings/parallel/ParallelSettings';
import { CustomHandle } from './CustomHandle';
import { NodeCard } from './BaseNode/NodeCard';

/** Render stored YAML while keeping the separate production authoring gate off. */
export function ParallelNode({ id, type = 'parallel', data, selected }: NodeProps<FlowNode>): ReactNode {
  const context = useContext(FlowEditorContext);
  const edges = useEdges();
  if (!context) return null;
  const node = context.yamlJsonObject.nodes?.find((entry) => entry.id === id);
  if (!node) return null;
  const running = Boolean(context.isRunningPipeline);
  const disabled = !FIXED_PARALLEL_AUTHORING_ENABLED || running || Boolean(context.disabled);
  const performing = Boolean(data['isPerforming']);
  const change = (field: string, value: unknown): void => {
    if (!disabled) context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, id, field, value));
  };
  const remove = (field: string): void => {
    if (!disabled) context.setYamlJsonObject(patchGraphExtensionNode(context.yamlJsonObject, id, field, undefined, true));
  };
  return <FlowEditorContext.Provider value={{ ...context, disabled }}>
    <NodeCard id={id} name={id} type={type} selected={selected} isEntrypoint={context.yamlJsonObject.entry_point === id}
      isPerforming={performing} handles={() => <>
        <CustomHandle type="target" id="target" isConnectable={!disabled} isRunningPipeline={running} isPerforming={performing} />
        <CustomHandle type="source" id="source" isConnectable={!disabled && !edges.some((edge) => edge.source === id && edge.target !== 'END')}
          isRunningPipeline={running} isPerforming={performing} />
      </>}>
      {!FIXED_PARALLEL_AUTHORING_ENABLED && <Alert severity="info">{t('pipelines.parallel.authoringOff', 'Fixed Parallel authoring is not enabled. Stored settings remain visible.')}</Alert>}
      <ParallelSettings node={node} disabled={disabled} change={change} remove={remove} />
    </NodeCard>
  </FlowEditorContext.Provider>;
}
