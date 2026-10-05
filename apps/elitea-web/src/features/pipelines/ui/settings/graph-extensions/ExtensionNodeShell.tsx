import type { ReactNode } from 'react';
import { useContext } from 'react';
import { useEdges, type NodeProps } from '@xyflow/react';
import type { FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { FlowEditorContext } from '../../../lib/flow-editor/flowEditorContext';
import { patchGraphExtensionNode } from '../../../lib/graphExtensions.helpers';
import type { ExtensionSettingsProps } from '../../../lib/graphExtensions.types';
import { NodeCard } from '../../nodes/BaseNode/NodeCard';
import { CustomHandle } from '../../nodes/CustomHandle';

interface ExtensionNodeShellProps extends NodeProps<FlowNode> {
  readonly renderSettings: (props: ExtensionSettingsProps) => ReactNode;
}
export function ExtensionNodeShell({ id, type = '', data, selected, renderSettings }: ExtensionNodeShellProps): ReactNode {
  const context = useContext(FlowEditorContext);
  const edges = useEdges();
  if (!context) return null;
  const { yamlJsonObject, setYamlJsonObject } = context;
  const node = yamlJsonObject.nodes?.find((entry) => entry.id === id);
  if (!node) return null;
  const running = Boolean(context.isRunningPipeline);
  const disabled = running || Boolean(context.disabled);
  const performing = Boolean(data['isPerforming']);
  const sourceAvailable = !edges.some((edge) => edge.source === id && edge.target !== 'END');
  const change = (field: string, value: unknown): void => {
    setYamlJsonObject(patchGraphExtensionNode(yamlJsonObject, id, field, value));
  };
  const remove = (field: string): void => {
    setYamlJsonObject(patchGraphExtensionNode(yamlJsonObject, id, field, undefined, true));
  };
  return <NodeCard id={id} name={id} type={type} selected={selected} isEntrypoint={yamlJsonObject.entry_point === id}
    isPerforming={performing} handles={() => <>
      <CustomHandle type="target" id="target" isConnectable={!disabled} isRunningPipeline={running} isPerforming={performing} />
      <CustomHandle type="source" id="source" isConnectable={sourceAvailable && !disabled} isRunningPipeline={running} isPerforming={performing} />
    </>}>
    {renderSettings({ node, disabled, change, remove })}
  </NodeCard>;
}
