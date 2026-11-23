import type { ComponentType, ReactNode } from 'react';
import { memo, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import type { NodeTypes } from '@xyflow/react';

import { t } from '@/shared/i18n';
import { CodeIcon } from '@/shared/ui/icons/code-icon';
import { FormIcon } from '@/shared/ui/icons/form-icon';
import { TabGroupButton, type TabGroupButtonItem } from '@/shared/ui/TabGroupButton';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import {
  FlowEditorContext,
  NodeConfigPanelContext,
  type FlowEditorContextValue,
  type NodeConfigPanelContextValue,
} from '../../lib/flow-editor/flowEditorContext';
import { isDeprecatedNodeType } from '../../lib/flow-editor/helpers/node.helpers';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { duplicatePipelineNode } from './duplicateNode.helpers';
import { NODE_CONFIG_DRAWER_WIDTH_REM, NodeConfigDrawer } from './NodeConfigDrawer';
import { NodeConfigFooter } from './NodeConfigFooter';
import type { NodeConfigViewMode } from './NodeConfigPanel';
import { NodeConfigPanelHeader } from './NodeConfigPanelHeader';
import { NodeDeprecationBanner } from './NodeDeprecationBanner';
import { NodeNameField } from './NodeNameField';
import { NodeYamlView } from './NodeYamlView';
import { useNodeYamlEditor } from './useNodeYamlEditor';

const { PipelineNodeTypes, DECISION_NODE_ID_SUFFIX } = FlowEditorConstants;

export interface FlowNodeConfigDrawerProps {
  readonly flowNodes: readonly FlowNode[];
  readonly nodeTypes: NodeTypes;
  readonly selectedNodeId: string | null;
  readonly onClose: () => void;
  readonly onSelectNode: (id: string | null) => void;
  readonly toolNames?: readonly string[] | undefined;
}

const panelRootSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  width: NODE_CONFIG_DRAWER_WIDTH_REM,
  maxWidth: '100%',
  height: '100%',
  boxSizing: 'border-box',
  backgroundColor: theme.vars.palette.background.paper,
  borderLeft: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'hidden',
});

const panelBodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  padding: '1rem 1.5rem',
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
};

const viewToggleItemSx: SxProps<Theme> = {
  width: '2rem',
  minWidth: '2rem',
  height: '1.75rem',
};

const toggleIconStyle = { width: '1rem', height: '1rem' };

function isConfigurableNode(node: FlowNode | undefined): node is FlowNode {
  if (!node || !node.type) return false;
  return node.type !== PipelineNodeTypes.End && node.type !== PipelineNodeTypes.Ghost;
}

interface LiveNodeMeta {
  readonly id: string;
  readonly type: string;
  readonly isEntrypoint: boolean;
  readonly isDeprecated: boolean;
  readonly isLegacyDecision: boolean;
  readonly isLegacyLayout: boolean;
  readonly canDuplicate: boolean;
  readonly isReadOnly: boolean;
  readonly displayName: string;
}

function isLegacyLayoutType(type: string, isDeprecated: boolean, isLegacyDecision: boolean): boolean {
  if (isLegacyDecision) return true;
  if (type === PipelineNodeTypes.Default || type === PipelineNodeTypes.Custom) return true;
  // `FunctionNode` delegates to `BaseToolNode`, which already renders `NodeConfigSection`s.
  return isDeprecated && type !== PipelineNodeTypes.Function;
}

function deriveLiveNodeMeta(flowNode: FlowNode, flowContext: FlowEditorContextValue): LiveNodeMeta {
  const { id, type = '', data } = flowNode;
  const isDeprecated = isDeprecatedNodeType(type);
  const isLegacyDecision = type === PipelineNodeTypes.Decision && id.endsWith(DECISION_NODE_ID_SUFFIX);
  const usesLabelAsName = type === PipelineNodeTypes.Decision || type === PipelineNodeTypes.Condition;
  const isOwnershipNode = type === PipelineNodeTypes.Parallel || type === PipelineNodeTypes.Map;
  return {
    id,
    type,
    isEntrypoint: flowContext.yamlJsonObject.entry_point === id,
    isDeprecated,
    isLegacyDecision,
    isLegacyLayout: isLegacyLayoutType(type, isDeprecated, isLegacyDecision),
    canDuplicate: !isDeprecated && !isLegacyDecision && !isOwnershipNode,
    isReadOnly: Boolean(flowContext.isRunningPipeline || flowContext.disabled),
    displayName: usesLabelAsName ? (data?.label ?? id) : id,
  };
}

interface LiveNodePanelBodyProps {
  readonly flowNode: FlowNode;
  readonly NodeComponent: ComponentType<Record<string, unknown>>;
  readonly flowNodes: readonly FlowNode[];
  readonly flowContext: FlowEditorContextValue;
  readonly onClose: () => void;
  readonly onSelectNode: (id: string | null) => void;
  readonly toolNames: readonly string[] | undefined;
}

function LiveNodePanel({
  flowNode,
  NodeComponent,
  flowNodes,
  flowContext,
  onClose,
  onSelectNode,
  toolNames,
}: LiveNodePanelBodyProps): ReactNode {
  const { data } = flowNode;
  const meta = deriveLiveNodeMeta(flowNode, flowContext);
  const { id, type, isEntrypoint, isDeprecated, isLegacyDecision, isLegacyLayout, canDuplicate, isReadOnly, displayName } =
    meta;
  const [view, setView] = useState<NodeConfigViewMode>('flow');
  const pendingRenamedIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (pendingRenamedIdRef.current === id) {
      pendingRenamedIdRef.current = null;
      return;
    }
    setView('flow');
  }, [id]);

  const handleRenamed = useCallback(
    (newId: string) => {
      pendingRenamedIdRef.current = newId;
      onSelectNode(newId);
    },
    [onSelectNode],
  );
  const yamlEditor = useNodeYamlEditor({ nodeId: id, onRenamed: handleRenamed, toolNames });

  const handleMakeEntrypoint = useCallback(
    (nodeId: string) => {
      if (isReadOnly) return;
      flowContext.setYamlJsonObject({ ...flowContext.yamlJsonObject, entry_point: nodeId });
    },
    [flowContext, isReadOnly],
  );

  const handleDuplicate = useCallback(
    (nodeId: string) => {
      if (isReadOnly || !canDuplicate) return;
      const nextId = duplicatePipelineNode({
        sourceId: nodeId,
        yamlJsonObject: flowContext.yamlJsonObject,
        flowNodes,
        setYamlJsonObject: flowContext.setYamlJsonObject,
        setFlowNodes: flowContext.setFlowNodes,
        toolNames,
      });
      if (nextId) onSelectNode(nextId);
    },
    [canDuplicate, flowContext, flowNodes, isReadOnly, onSelectNode, toolNames],
  );

  const handleDelete = useCallback(
    (nodeId: string) => {
      if (isReadOnly) return;
      flowContext.handleDeleteNode?.(nodeId);
    },
    [flowContext, isReadOnly],
  );

  const viewItems = useMemo<TabGroupButtonItem[]>(
    () => [
      {
        value: 'flow',
        icon: <FormIcon style={toggleIconStyle} />,
        tooltip: t('pipelines.nodeConfigPanel.flowView', 'Flow view'),
      },
      {
        value: 'yaml',
        icon: <CodeIcon style={toggleIconStyle} />,
        tooltip: t('pipelines.nodeConfigPanel.yamlView', 'YAML view'),
      },
    ],
    [],
  );

  const handleViewChange = useCallback((next: string) => {
    if (next === 'flow' || next === 'yaml') setView(next);
  }, []);

  const formContextValue = useMemo<FlowEditorContextValue>(
    () => (isDeprecated ? { ...flowContext, disabled: true } : flowContext),
    [flowContext, isDeprecated],
  );

  const panelContextValue = useMemo<NodeConfigPanelContextValue>(
    () => ({ isEntrypoint, isLegacyLayout }),
    [isEntrypoint, isLegacyLayout],
  );

  return (
    <Box sx={panelRootSx} className="nokey" data-testid="pipeline-node-panel" data-node-id={id}>
      <NodeConfigPanelHeader type={type} onClose={onClose} />
      <Box sx={panelBodySx}>
        <Box data-testid="pipeline-node-panel-view-toggle">
          <TabGroupButton
            items={viewItems}
            value={view}
            onChange={handleViewChange}
            ariaLabel={t('pipelines.nodeConfigPanel.viewMode', 'Node configuration view mode')}
            itemSx={viewToggleItemSx}
          />
        </Box>
        {view === 'yaml' ? (
          <NodeYamlView
            code={yamlEditor.code}
            onChangeCode={yamlEditor.onChangeCode}
            error={yamlEditor.error}
            disabled={isReadOnly || isLegacyDecision}
          />
        ) : (
          <>
            <NodeNameField
              name={displayName}
              type={type}
              onRenamed={handleRenamed}
              isLocked={isLegacyDecision || isDeprecated}
              toolNames={toolNames}
            />
            {isDeprecated ? <NodeDeprecationBanner type={type} /> : null}
            <FlowEditorContext.Provider value={formContextValue}>
              <NodeConfigPanelContext.Provider value={panelContextValue}>
                <NodeComponent
                  key={id}
                  id={id}
                  type={type}
                  data={data}
                  selected={false}
                />
              </NodeConfigPanelContext.Provider>
            </FlowEditorContext.Provider>
          </>
        )}
      </Box>
      {!isReadOnly ? (
        <NodeConfigFooter
          id={id}
          type={type}
          isEntrypoint={isEntrypoint}
          canMakeEntrypoint={!isDeprecated}
          canDuplicate={canDuplicate}
          disabled={isReadOnly}
          onMakeEntrypoint={handleMakeEntrypoint}
          onDuplicate={handleDuplicate}
          onDelete={handleDelete}
        />
      ) : null}
    </Box>
  );
}

/**
 * Live 400px node configuration drawer mounted inside `FlowEditor`.
 * Renders the selected node's own component in panel mode (`NodeConfigPanelContext`)
 * alongside `NodeNameField`, `NodeDeprecationBanner`, per-node YAML editing, and footer actions.
 */
export const FlowNodeConfigDrawer = memo(function FlowNodeConfigDrawer({
  flowNodes,
  nodeTypes,
  selectedNodeId,
  onClose,
  onSelectNode,
  toolNames,
}: FlowNodeConfigDrawerProps): ReactNode {
  const flowContext = useContext(FlowEditorContext);

  const selectedNode = useMemo(
    () => (selectedNodeId ? flowNodes.find((node) => node.id === selectedNodeId) : undefined),
    [flowNodes, selectedNodeId],
  );

  const configurableNode = isConfigurableNode(selectedNode) ? selectedNode : undefined;
  const nodeTypeKey = configurableNode?.type;
  const NodeComponent =
    nodeTypeKey && Object.hasOwn(nodeTypes, nodeTypeKey)
      ? (nodeTypes[nodeTypeKey] as ComponentType<Record<string, unknown>> | undefined)
      : undefined;

  const isOpen = Boolean(flowContext && configurableNode && NodeComponent);

  return (
    <NodeConfigDrawer open={isOpen}>
      {flowContext && configurableNode && NodeComponent ? (
        <LiveNodePanel
          flowNode={configurableNode}
          NodeComponent={NodeComponent}
          flowNodes={flowNodes}
          flowContext={flowContext}
          onClose={onClose}
          onSelectNode={onSelectNode}
          toolNames={toolNames}
        />
      ) : null}
    </NodeConfigDrawer>
  );
});
