/** Ported from `EliteaUI` `flow-editor/ui/FlowEditor.jsx` — canvas composition root (zustand + explicit props). */
import type { ReactNode } from 'react';
import { forwardRef, memo, useCallback, useImperativeHandle, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import { useColorScheme, type SxProps, type Theme } from '@mui/material/styles';
import {
  ReactFlow,
  SelectionMode,
  applyEdgeChanges,
  applyNodeChanges,
  useEdgesState,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  type OnBeforeDelete,
  type OnConnectEnd,
  type OnEdgesChange,
  type OnNodesChange,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';

import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { t } from '@/shared/i18n';

import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import { FlowEditorConstants } from '../lib/flow-editor/constants';
import type { RunSocketEvent } from '../lib/flow-editor/helpers/parseRunsByEvent.support';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import { useConnectNodes } from '../lib/flow-editor/hooks/useConnectNodes';
import { useCtrlASelectAll } from '../lib/flow-editor/hooks/useCtrlASelectAll';
import { useDeleteItems } from '../lib/flow-editor/hooks/useDeleteItems';
import { useIncompleteEdge } from '../lib/flow-editor/hooks/useIncompleteEdge';
import { useRunEvent } from '../lib/flow-editor/hooks/useRunEvent';
import { useSaveNodesAndEdges } from '../lib/flow-editor/hooks/useSaveNodeAndEdges';
import type { FlowEdge, FlowNode, SetFlowEdges, SetFlowNodes, SetYamlJsonObject } from '../lib/flow-editor/reactFlowTypes';
import { usePipelineEditorStore } from '../model/pipelineEditorStore';
import { flowEditorContainerSx, flowEditorStateBarSx, FLOW_EDITOR_DEFAULT_VIEWPORT } from './FlowEditor.styles';
import type { CanvasArrangeOrientation } from './canvas-controls';
import { FlowEditorBackground, FlowEditorCanvasControls } from './FlowEditorCanvasControls';
import { FlowEditorProvider } from './FlowEditorProvider';
import { flowEditorFitViewOptions } from './flowEditorFitView';
import { FlowEditorStateToggle } from './FlowEditorStateToggle';
import { FlowEmptyState } from './empty-state';
import { FlowNodeConfigDrawer } from './node-config-drawer/FlowNodeConfigDrawer';
import { RunStateNodeGroup } from './nodes/RunStateNodeGroup';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';
import { ConnectionDropdown } from './settings/ConnectionDropdown';
import { StateDrawer } from './state/StateDrawer';
import {
  computeDockingOffsets,
  extractSxDisplay,
  getCanvasInteractionFlags,
  getVisibleFlowNodes,
  isEmptyPipelineDocument,
  useFlowEditorInitialFitView,
  useFlowEditorLayoutVersionSync,
  useFlowEditorNodeSelection,
  useFlowEditorPersistence,
  useFlowEditorReset,
  useFlowEditorResizeObserver,
  useLatest,
  useYamlJsonObjectRef,
} from './useFlowEditorLifecycle';
import { useFlowEditorNodeOperations } from './useFlowEditorNodeOperations';
import { useFlowEditorNodeTypes } from './useFlowEditorNodeTypes';

/** @public Composition root — not currently on this slice's `index.ts` curated surface (only `PipelineEditor.jsx`/`useEditPipeline.js`/`usePipelineCreation.js` carry the hard cross-domain export requirement; those are separate, not-yet-landed sub-units). */
export interface FlowEditorProps {
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly initialNodes: readonly FlowNode[];
  readonly initialEdges: readonly FlowEdge[];
  /** Replaces the baseline's `state.pipeline.layout_version` Redux read — see module doc comment. */
  readonly layoutVersion: string | undefined;
  /** Replaces the baseline's `state.pipeline.resetFlag` Redux read — see module doc comment. */
  readonly resetFlag: boolean;
  /** Replaces the baseline's `dispatch(actions.clearResetFlag())` — see module doc comment. */
  readonly onResetHandled: () => void;
  /** Replaces the baseline's `dispatch(actions.setLayoutVersion(...))` — see module doc comment. */
  readonly onLayoutVersionChange: (version: string) => void;
  readonly stopRun: () => void;
  /** Bridged into every `versionTools`-accepting node — see `./useFlowEditorNodeTypes.tsx`'s doc comment. */
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
  /** Bridged into every `llmSettings`-accepting node — see `./useFlowEditorNodeTypes.tsx`'s doc comment. */
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
  readonly disabled?: boolean | undefined;
  readonly onCanvasNodeInteraction?: (() => void) | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

/** @public Imperative surface `FlowWrapper`'s baseline exposes via `ref` (`FlowEditor.jsx:327-363`). */
export interface FlowEditorHandle {
  readonly fitView: () => void;
  readonly onAddNode: (type: string) => FlowNode | undefined;
  readonly onDuplicateNode: (nodeId: string) => FlowNode | undefined;
  readonly onRcvAgentEvent: (event: RunSocketEvent) => void;
  readonly setFlowEdges: SetFlowEdges;
  readonly setFlowNodes: SetFlowNodes;
  readonly deleteAllRunNodes: () => void;
  readonly getCurrentExpandState: () => boolean;
  readonly calculateLayoutNodes: (
    parsedYamlJson: YamlPipelineDocument,
    shouldDoLayout: boolean,
    layoutAll: boolean,
    explicitExpandState?: boolean,
    orientation?: CanvasArrangeOrientation,
  ) => void;
  readonly stopCurrentRun: () => void;
  readonly hasRunsInProgress: () => boolean;
}

const FlowEditorImpl = forwardRef<FlowEditorHandle, FlowEditorProps>(function FlowEditor(props, ref): ReactNode {
  const { yamlJsonObject, setYamlJsonObject, initialNodes, initialEdges, layoutVersion, resetFlag, onResetHandled, onLayoutVersionChange, stopRun, versionTools, llmSettings, disabled, onCanvasNodeInteraction, sx } = props;

  const { colorScheme } = useColorScheme();
  const [expandAll] = useState(true);
  const [isCanvasLocked, setIsCanvasLocked] = useState(false);
  const [orientation, setOrientation] = useState<CanvasArrangeOrientation>('TB');
  const [isStateDrawerOpen, setIsStateDrawerOpen] = useState(false);
  const [stateDrawerWidth, setStateDrawerWidth] = useState(0);

  const { setNodes: persistNodes, setEdges: persistEdges } = useSaveNodesAndEdges();
  const cachedNodes = usePipelineEditorStore(state => state.nodes);
  const cachedEdges = usePipelineEditorStore(state => state.edges);

  const yamlJsonObjectRef = useYamlJsonObjectRef(yamlJsonObject);
  const { editorRef, editorHeight, editorWidth } = useFlowEditorResizeObserver();
  const { fitView, getViewport, getZoom, setCenter } = useReactFlow();
  const fitViewVoid = useCallback(() => {
    void fitView(flowEditorFitViewOptions(editorRef.current?.querySelector<HTMLElement>('.react-flow') ?? null));
  }, [fitView, editorRef]);
  const nodesInitialized = useNodesInitialized({ includeHiddenNodes: true });

  const [flowNodes, setFlowNodes] = useNodesState<FlowNode>(cachedNodes.length ? [...cachedNodes] : [...initialNodes]);
  const [flowEdges, setFlowEdges] = useEdgesState<FlowEdge>(cachedEdges.length ? [...cachedEdges] : [...initialEdges]);

  const { onStopRun, deleteRunNode, deleteAllRunNodes, onRcvAgentEvent, onResetRunParseStatus, isRunningPipeline, pipelineRunNodes } = useRunEvent(setFlowNodes, yamlJsonObject);

  const handleStopRun = useCallback(
    (id: string) => {
      stopRun();
      onStopRun(id);
    },
    [onStopRun, stopRun],
  );

  const sxDisplay = extractSxDisplay(sx);
  const safeDisabled = disabled ?? false;

  const { showDeleteConfirmDlg, confirmContent, nodesToDelete, onBeforeDelete, handleDeleteNode, onConfirmDelete, onCancelDelete } = useDeleteItems({
    display: sxDisplay,
    yamlJsonObject,
    flowNodes,
    flowEdges,
    setYamlJsonObject,
    setFlowNodes,
    setFlowEdges,
    disabled: safeDisabled,
  });

  useCtrlASelectAll({ display: sxDisplay, setFlowNodes, setFlowEdges });
  useFlowEditorReset({ resetFlag, initialNodes, initialEdges, setFlowNodes, setFlowEdges, onResetRunParseStatus, onResetHandled, persistNodes, persistEdges, fitView: fitViewVoid });
  useFlowEditorInitialFitView(initialNodes.length, fitViewVoid);
  useFlowEditorPersistence({ flowNodes, flowEdges, initialNodes, initialEdges, isRunningPipeline, persistNodes, persistEdges });

  const { onNodeCreateAtPosition, onAddNode, onDuplicateNode, calculateLayoutNodes } = useFlowEditorNodeOperations({
    flowNodes,
    setFlowNodes,
    setFlowEdges,
    setYamlJsonObject,
    yamlJsonObjectRef,
    getViewport,
    setCenter,
    getZoom,
    editorRef,
    editorWidth,
    editorHeight,
  });

  useFlowEditorLayoutVersionSync({
    layoutVersion,
    currentLayoutVersion: FlowEditorConstants.LAYOUT_VERSION,
    nodesInitialized,
    onReLayout: () => {
      calculateLayoutNodes(yamlJsonObject, true, true, expandAll, orientation);
      setTimeout(fitViewVoid, 100);
    },
    onLayoutVersionChange,
  });

  const versionToolNames = useMemo(
    () => (versionTools ?? []).map(tool => tool.toolkit_name || tool.name || '').filter(Boolean),
    [versionTools],
  );

  const handleAddNode = useCallback(
    (type: string) => {
      if (safeDisabled) return undefined;
      onCanvasNodeInteraction?.();
      return onAddNode(type);
    },
    [onAddNode, onCanvasNodeInteraction, safeDisabled],
  );

  const handleDuplicateNode = useCallback(
    (nodeId: string): FlowNode | undefined => {
      if (safeDisabled) return undefined;
      onCanvasNodeInteraction?.();
      return onDuplicateNode(nodeId, versionToolNames);
    },
    [onDuplicateNode, safeDisabled, versionToolNames, onCanvasNodeInteraction],
  );

  const latest = useLatest({ fitViewVoid, flowNodes, onAddNode: handleAddNode, handleDuplicateNode, onRcvAgentEvent, setFlowEdges, setFlowNodes, deleteAllRunNodes, calculateLayoutNodes, expandAll, orientation, onStopRun, pipelineRunNodes });

  useImperativeHandle(
    ref,
    () => ({
      fitView: () => {
        if (latest.current.flowNodes.length > 2) setTimeout(latest.current.fitViewVoid, 100);
      },
      onAddNode: type => latest.current.onAddNode(type),
      onDuplicateNode: nodeId => latest.current.handleDuplicateNode(nodeId),
      onRcvAgentEvent: event => latest.current.onRcvAgentEvent(event),
      setFlowEdges: updater => latest.current.setFlowEdges(updater),
      setFlowNodes: updater => latest.current.setFlowNodes(updater),
      deleteAllRunNodes: () => latest.current.deleteAllRunNodes(),
      getCurrentExpandState: () => latest.current.expandAll,
      calculateLayoutNodes: (parsedYamlJson, shouldDoLayout, layoutAll, explicitExpandState, layoutOrientation) =>
        latest.current.calculateLayoutNodes(
          parsedYamlJson,
          shouldDoLayout,
          layoutAll,
          explicitExpandState ?? latest.current.expandAll,
          layoutOrientation ?? latest.current.orientation,
        ),
      stopCurrentRun: () => latest.current.onStopRun(latest.current.pipelineRunNodes[latest.current.pipelineRunNodes.length - 1]?.id ?? ''),
      hasRunsInProgress: () => latest.current.pipelineRunNodes.some(node => node.data?.status === FlowEditorConstants.PipelineStatus.InProgress),
    }),
    [latest],
  );

  const onNodesChange = useCallback<OnNodesChange<FlowNode>>(
    changes => setFlowNodes(nds => applyNodeChanges(changes, nds.map(nd => (nd.measured ? { ...nd, measured: structuredClone(nd.measured) } : nd)))),
    [setFlowNodes],
  );
  const onEdgesChange = useCallback<OnEdgesChange<FlowEdge>>(changes => setFlowEdges(eds => applyEdgeChanges(changes, eds.map(ed => ({ ...ed })))), [setFlowEdges]);

  const onConnect = useConnectNodes({ flowNodes, yamlJsonObjectRef, setFlowNodes, setYamlJsonObject, setFlowEdges, disabled: safeDisabled });

  const { onConnectEnd, onReconnect, onReconnectEnd, showConnectionDropdown, dropdownPosition, handleDropdownClose, handleNodeSelect, handleNodeCreate, availableTargets, availableNodeTypes } =
    useIncompleteEdge({ onConnect, onNodeCreateAtPosition, yamlJsonObjectRef, disabled: safeDisabled });

  const onToggleCanvasLock = useCallback(() => setIsCanvasLocked(prev => !prev), []);

  const onArrange = useCallback(
    (nextOrientation: CanvasArrangeOrientation) => {
      setOrientation(nextOrientation);
      calculateLayoutNodes(yamlJsonObject, true, true, expandAll, nextOrientation);
      setTimeout(fitViewVoid, 100);
    },
    [calculateLayoutNodes, yamlJsonObject, expandAll, fitViewVoid],
  );

  const onToggleStateDrawer = useCallback(() => setIsStateDrawerOpen(prev => !prev), []);

  const isEmptyPipeline = isEmptyPipelineDocument(yamlJsonObject);
  const { canEditCanvas, canArrange } = getCanvasInteractionFlags(
    safeDisabled,
    isCanvasLocked,
    isRunningPipeline,
    isEmptyPipeline,
  );
  const visibleFlowNodes = useMemo(
    () => getVisibleFlowNodes(flowNodes, isEmptyPipeline),
    [flowNodes, isEmptyPipeline],
  );

  const { nodeTypes, edgeTypes } = useFlowEditorNodeTypes({ versionTools, llmSettings });
  const { selectedNodeId, isNodeDrawerOpen, handleSelectNode, handleCloseNodeDrawer } = useFlowEditorNodeSelection(visibleFlowNodes, setFlowNodes, onCanvasNodeInteraction);
  const { drawerRightOffset, minimapRightOffset } = computeDockingOffsets(isNodeDrawerOpen, isStateDrawerOpen, stateDrawerWidth);

  return (
    <Box
      sx={combineSx(flowEditorContainerSx, sx)}
      ref={editorRef}
    >
      <Box sx={flowEditorStateBarSx}>
        <RunStateNodeGroup
          deleteRunNode={deleteRunNode}
          handleStopRun={handleStopRun}
          yamlJsonObject={yamlJsonObject}
          editorHeight={editorHeight}
          editorWidth={editorWidth}
          nodes={pipelineRunNodes}
        />
      </Box>
      <FlowEditorStateToggle
        isOpen={isStateDrawerOpen}
        onToggle={onToggleStateDrawer}
        rightOffset={drawerRightOffset}
      />
      <FlowEditorProvider
        setFlowEdges={setFlowEdges}
        setFlowNodes={setFlowNodes}
        editorHeight={editorHeight}
        editorWidth={editorWidth}
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        deleteRunNode={deleteRunNode}
        isRunningPipeline={isRunningPipeline}
        handleDeleteNode={handleDeleteNode}
        handleDuplicateNode={handleDuplicateNode}
        expandAll={expandAll}
        disabled={safeDisabled}
      >
        <ReactFlow<FlowNode, FlowEdge>
          panOnDrag
          panOnScroll
          selectionOnDrag
          nodes={visibleFlowNodes}
          onNodesChange={onNodesChange}
          edges={flowEdges}
          onEdgesChange={onEdgesChange}
          onConnect={onConnect}
          // A2d's `onBeforeDelete`/`onConnectEnd` types are narrower than
          // xyflow 12's own `OnBeforeDelete`/`OnConnectEnd` — real,
          // documented cross-file gaps, not editable here (not owned).
          onBeforeDelete={onBeforeDelete as unknown as OnBeforeDelete<FlowNode, FlowEdge>}
          onConnectEnd={onConnectEnd as unknown as OnConnectEnd}
          onReconnect={onReconnect}
          onReconnectEnd={onReconnectEnd}
          colorMode={colorScheme ?? 'light'}
          proOptions={{ hideAttribution: true }}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          zoomOnDoubleClick={false}
          selectionMode={SelectionMode.Partial}
          minZoom={0.1}
          nodesDraggable={canEditCanvas}
          nodesConnectable={canEditCanvas}
          edgesReconnectable={canEditCanvas}
          {...(onCanvasNodeInteraction ? { onNodeDragStop: onCanvasNodeInteraction } : {})}
          defaultViewport={FLOW_EDITOR_DEFAULT_VIEWPORT}
        >
          <FlowEditorBackground />
          <FlowEditorCanvasControls
            isLocked={isCanvasLocked}
            onToggleLock={onToggleCanvasLock}
            orientation={orientation}
            onArrange={onArrange}
            canArrange={canArrange}
            rightOffsetPx={minimapRightOffset}
          />
        </ReactFlow>
        <FlowNodeConfigDrawer
          flowNodes={visibleFlowNodes}
          nodeTypes={nodeTypes}
          selectedNodeId={selectedNodeId}
          onClose={handleCloseNodeDrawer}
          onSelectNode={handleSelectNode}
          toolNames={versionToolNames}
        />
      </FlowEditorProvider>
      {isEmptyPipeline && <FlowEmptyState onAddNode={handleAddNode} disabled={safeDisabled} />}
      <StateDrawer isOpen={isStateDrawerOpen} onClose={onToggleStateDrawer} setYamlJsonObject={setYamlJsonObject} yamlJsonObject={yamlJsonObject} disabled={disabled} rightOffset={drawerRightOffset} onWidthChange={setStateDrawerWidth} />
      <DeleteEntityModal
        open={showDeleteConfirmDlg}
        onClose={onCancelDelete}
        onConfirm={onConfirmDelete}
        name={nodesToDelete[0]?.data?.label ?? nodesToDelete[0]?.id ?? ''}
        alarm={false}
        copy={{ title: t('pipelines.flowEditor.deleteNode.title', 'Delete node?'), textContent: confirmContent, confirmText: t('pipelines.flowEditor.deleteNode.confirm', 'Remove') }}
      />
      <ConnectionDropdown open={showConnectionDropdown} anchorPosition={dropdownPosition} targetNodes={availableTargets} availableNodeTypes={availableNodeTypes} onNodeSelect={handleNodeSelect} onNodeCreate={handleNodeCreate} onClose={handleDropdownClose} />
    </Box>
  );
});

FlowEditorImpl.displayName = 'FlowEditor';

export const FlowEditor = memo(FlowEditorImpl);
