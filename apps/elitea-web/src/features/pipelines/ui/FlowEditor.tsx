/**
 * Ported from `EliteaUI` `flow-editor/ui/FlowEditor.jsx` — canvas composition root.
 * Split across `./FlowEditor.styles.ts`, `./useFlowEditorLifecycle.ts`,
 * `./useFlowEditorNodeOperations.ts`, `./useFlowEditorNodeTypes.tsx`,
 * `./FlowEditorProvider.tsx`, `./FlowEditorCanvasControls.tsx`, and
 * `./FlowEditorStateToggle.tsx` to stay within §3.5 budgets.
 *
 * Uses zustand (`pipelineEditorStore`) + explicit props instead of Redux;
 * GA tracking and interactive-tour `data-tour` attributes are omitted.
 */
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
import { FlowEditorBackground, FlowEditorCanvasControls } from './FlowEditorCanvasControls';
import { FlowEditorProvider } from './FlowEditorProvider';
import { flowEditorFitViewOptions } from './flowEditorFitView';
import { FlowEditorStateToggle } from './FlowEditorStateToggle';
import { FlowEmptyState } from './empty-state';
import { RunStateNodeGroup } from './nodes/RunStateNodeGroup';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';
import { ConnectionDropdown } from './settings/ConnectionDropdown';
import { StateDrawer } from './state/StateDrawer';
import {
  extractSxDisplay,
  useFlowEditorInitialFitView,
  useFlowEditorLayoutVersionSync,
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
  readonly sx?: SxProps<Theme> | undefined;
}

/** @public Imperative surface `FlowWrapper`'s baseline exposes via `ref` (`FlowEditor.jsx:327-363`). */
export interface FlowEditorHandle {
  readonly fitView: () => void;
  readonly onAddNode: (type: string) => FlowNode | undefined;
  readonly onRcvAgentEvent: (event: RunSocketEvent) => void;
  readonly setFlowEdges: SetFlowEdges;
  readonly setFlowNodes: SetFlowNodes;
  readonly deleteAllRunNodes: () => void;
  readonly getCurrentExpandState: () => boolean;
  readonly calculateLayoutNodes: (parsedYamlJson: YamlPipelineDocument, shouldDoLayout: boolean, layoutAll: boolean, explicitExpandState?: boolean) => void;
  readonly stopCurrentRun: () => void;
  readonly hasRunsInProgress: () => boolean;
}

function isEmptyPipelineDocument(yamlJsonObject: YamlPipelineDocument): boolean {
  const nodes = (yamlJsonObject as { readonly nodes?: readonly unknown[] }).nodes;
  return !nodes?.filter(Boolean).length;
}

function getVisibleFlowNodes(flowNodes: FlowNode[], isEmpty: boolean): FlowNode[] {
  return isEmpty ? flowNodes.filter(node => node.type !== FlowEditorConstants.PipelineNodeTypes.End) : flowNodes;
}

const FlowEditorImpl = forwardRef<FlowEditorHandle, FlowEditorProps>(function FlowEditor(props, ref): ReactNode {
  const { yamlJsonObject, setYamlJsonObject, initialNodes, initialEdges, layoutVersion, resetFlag, onResetHandled, onLayoutVersionChange, stopRun, versionTools, llmSettings, disabled, sx } = props;

  const { colorScheme } = useColorScheme();
  const [expandAll, setExpandAll] = useState(true);
  const [isStateDrawerOpen, setIsStateDrawerOpen] = useState(false);

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
  // Every A2d hook's `disabled?: boolean` (no `| undefined`) rejects an
  // explicit `undefined` under `exactOptionalPropertyTypes: true`; those
  // args interfaces aren't owned here. `disabled` is only ever read as
  // `!disabled`, so coercing the missing case to `false` is behaviourally
  // identical.
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

  const { onNodeCreateAtPosition, onAddNode, calculateLayoutNodes } = useFlowEditorNodeOperations({
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
      calculateLayoutNodes(yamlJsonObject, true, true, expandAll);
      setTimeout(fitViewVoid, 100);
    },
    onLayoutVersionChange,
  });

  const latest = useLatest({ fitViewVoid, flowNodes, onAddNode, onRcvAgentEvent, setFlowEdges, setFlowNodes, deleteAllRunNodes, calculateLayoutNodes, expandAll, onStopRun, pipelineRunNodes });

  useImperativeHandle(
    ref,
    () => ({
      fitView: () => {
        if (latest.current.flowNodes.length > 2) setTimeout(latest.current.fitViewVoid, 100);
      },
      onAddNode: type => latest.current.onAddNode(type),
      onRcvAgentEvent: event => latest.current.onRcvAgentEvent(event),
      setFlowEdges: updater => latest.current.setFlowEdges(updater),
      setFlowNodes: updater => latest.current.setFlowNodes(updater),
      deleteAllRunNodes: () => latest.current.deleteAllRunNodes(),
      getCurrentExpandState: () => latest.current.expandAll,
      calculateLayoutNodes: (parsedYamlJson, shouldDoLayout, layoutAll, explicitExpandState) =>
        latest.current.calculateLayoutNodes(parsedYamlJson, shouldDoLayout, layoutAll, explicitExpandState ?? latest.current.expandAll),
      stopCurrentRun: () => latest.current.onStopRun(latest.current.pipelineRunNodes[latest.current.pipelineRunNodes.length - 1]?.id ?? ''),
      hasRunsInProgress: () => latest.current.pipelineRunNodes.some(node => node.data?.status === FlowEditorConstants.PipelineStatus.InProgress),
    }),
    [latest],
  );

  const onNodesChange = useCallback<OnNodesChange<FlowNode>>(
    // Only clone-and-reassign `measured` when it is actually present —
    // spreading `{...nd, measured: undefined}` for nodes with no `measured`
    // yet would explicitly set the key to `undefined`, which
    // `exactOptionalPropertyTypes: true` treats as a distinct (and here,
    // illegal) state from "the key is simply absent".
    changes => setFlowNodes(nds => applyNodeChanges(changes, nds.map(nd => (nd.measured ? { ...nd, measured: structuredClone(nd.measured) } : nd)))),
    [setFlowNodes],
  );
  const onEdgesChange = useCallback<OnEdgesChange<FlowEdge>>(changes => setFlowEdges(eds => applyEdgeChanges(changes, eds.map(ed => ({ ...ed })))), [setFlowEdges]);

  const onConnect = useConnectNodes({ flowNodes, yamlJsonObjectRef, setFlowNodes, setYamlJsonObject, setFlowEdges, disabled: safeDisabled });

  const { onConnectEnd, onReconnect, onReconnectEnd, showConnectionDropdown, dropdownPosition, handleDropdownClose, handleNodeSelect, handleNodeCreate, availableTargets, availableNodeTypes } =
    useIncompleteEdge({ onConnect, onNodeCreateAtPosition, yamlJsonObjectRef, disabled: safeDisabled });

  const onExpandAll = useCallback(() => {
    setExpandAll(prev => !prev);
    setTimeout(() => {
      calculateLayoutNodes(yamlJsonObject, true, true, !expandAll);
      setTimeout(fitViewVoid, 100);
    }, 200);
  }, [calculateLayoutNodes, yamlJsonObject, expandAll, fitViewVoid]);

  const onReLayoutClick = useCallback(() => {
    calculateLayoutNodes(yamlJsonObject, true, true, expandAll);
    setTimeout(fitViewVoid, 100);
  }, [calculateLayoutNodes, yamlJsonObject, expandAll, fitViewVoid]);

  const onToggleStateDrawer = useCallback(() => setIsStateDrawerOpen(prev => !prev), []);

  const isEmptyPipeline = isEmptyPipelineDocument(yamlJsonObject);
  const visibleFlowNodes = useMemo(
    () => getVisibleFlowNodes(flowNodes, isEmptyPipeline),
    [flowNodes, isEmptyPipeline],
  );

  const { nodeTypes, edgeTypes } = useFlowEditorNodeTypes({ versionTools, llmSettings });

  // `RunStateNodeGroup`'s `RunStateGraphNode` (A2f) kept the baseline's own
  // node-shaped contract (`{id, data, selected?}`) — the same shape
  // `useRunEvent`'s `pipelineRunNodes` (A2d, `RunPipelineStatus`:
  // `{id, type, data: {label, status, timeline, error?}}`) already has, so
  // no adapter/flattening is needed at this boundary; `pipelineRunNodes` is
  // passed straight through.

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
          defaultViewport={FLOW_EDITOR_DEFAULT_VIEWPORT}
        >
          <FlowEditorBackground />
          <FlowEditorCanvasControls
            expandAll={expandAll}
            onExpandAll={onExpandAll}
            onReLayout={onReLayoutClick}
          />
        </ReactFlow>
      </FlowEditorProvider>
      {isEmptyPipeline && (
        <FlowEmptyState
          onAddNode={onAddNode}
          disabled={safeDisabled}
        />
      )}
      <StateDrawer
        isOpen={isStateDrawerOpen}
        onClose={onToggleStateDrawer}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
        disabled={disabled}
      />
      <DeleteEntityModal
        open={showDeleteConfirmDlg}
        onClose={onCancelDelete}
        onConfirm={onConfirmDelete}
        name={nodesToDelete[0]?.data?.label ?? nodesToDelete[0]?.id ?? ''}
        alarm={false}
        copy={{ title: t('pipelines.flowEditor.deleteNode.title', 'Delete node?'), textContent: confirmContent, confirmText: t('pipelines.flowEditor.deleteNode.confirm', 'Remove') }}
      />
      <ConnectionDropdown
        open={showConnectionDropdown}
        anchorPosition={dropdownPosition}
        targetNodes={availableTargets}
        availableNodeTypes={availableNodeTypes}
        onNodeSelect={handleNodeSelect}
        onNodeCreate={handleNodeCreate}
        onClose={handleDropdownClose}
      />
    </Box>
  );
});

FlowEditorImpl.displayName = 'FlowEditor';

export const FlowEditor = memo(FlowEditorImpl);
