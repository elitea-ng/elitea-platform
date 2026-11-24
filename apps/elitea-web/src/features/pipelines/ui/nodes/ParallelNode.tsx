import type { ReactNode } from 'react';
import { useContext } from 'react';
import type { NodeProps } from '@xyflow/react';
import { useEdges } from '@xyflow/react';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { patchGraphExtensionNode } from '../../lib/graphExtensions.helpers';
import { nodeSettingsFamilyKeys } from '../../lib/nodeSettings.helpers';
import { NodeConfigSection } from '../node-config-drawer/NodeConfigSection';
import { NodeSettings } from '../settings/NodeSettings';
import { ParallelSettings } from '../settings/parallel/ParallelSettings';
import { CustomHandle } from './CustomHandle';
import { NodeCard } from './BaseNode/NodeCard';

/** The fixed Parallel node: its branches, concurrency, On Error and Join settings. */
export function ParallelNode({ id, type = 'parallel', data, selected }: NodeProps<FlowNode>): ReactNode {
  const context = useContext(FlowEditorContext);
  const edges = useEdges();
  if (!context) return null;
  const node = context.yamlJsonObject.nodes?.find((entry) => entry.id === id);
  if (!node) return null;
  const running = Boolean(context.isRunningPipeline);
  const disabled = running || Boolean(context.disabled);
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
      <NodeConfigSection section="basic">
        <ParallelSettings node={node} disabled={disabled} change={change} remove={remove} section="basic" />
      </NodeConfigSection>
      <NodeConfigSection section="advanced" defaultExpanded={false}>
        <ParallelSettings node={node} disabled={disabled} change={change} remove={remove} section="advanced" />
        {nodeSettingsFamilyKeys(type) !== undefined ? (
          <NodeSettings id={id} type={type} disabled={disabled} />
        ) : null}
      </NodeConfigSection>
    </NodeCard>
  </FlowEditorContext.Provider>;
}
