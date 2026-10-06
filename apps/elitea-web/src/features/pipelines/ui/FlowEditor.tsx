/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * FlowEditor.jsx` (722 lines) — unit A2k, the flow-editor's canvas
 * composition root: instantiates `<ReactFlow>`, wires the `nodeTypes`/
 * `edgeTypes` maps to every node component across A2e/A2f/A2g (see
 * `./useFlowEditorNodeTypes.tsx`), and composes A2c's parse/layout helpers
 * with A2d's canvas-interaction hooks. The highest-fan-in file in the
 * `pipelines` domain — built LAST, after every node/hook/helper sub-unit.
 *
 * SPLIT ACROSS FILES purely to fit §3.5's 400-line/3-effects-per-component
 * budgets (baseline: 7 top-level `useEffect`s alone): `./FlowEditor.
 * styles.ts`, `./useFlowEditorLifecycle.ts` (the 7 effects, regrouped),
 * `./useFlowEditorNodeOperations.ts` (node-creation/layout callbacks),
 * `./useFlowEditorNodeTypes.tsx` (`nodeTypes`/`edgeTypes`), `./
 * FlowEditorProvider.tsx` (`<FlowEditorContext.Provider>`), `./
 * FlowEditorCanvasControls.tsx` / `./FlowEditorStateToggle.tsx` (self-
 * contained JSX chunks).
 *
 * DISCLOSED REDESIGN — no Redux (baseline: `useSelector(state =>
 * state.pipeline)`/`useSelector(state => state.pipelineEditor)`/
 * `dispatch(actions...)`, `FlowEditor.jsx:3,40,94-114,128-129,178-197,
 * 475-481`): this app has zustand only. `state.pipelineEditor`'s `{nodes,
 * edges}` CACHE already lives in `../model/pipelineEditorStore.ts` (A2d),
 * read unchanged below. Everything else `slices/pipeline.js` owned —
 * `yamlJsonObject`/`nodes`/`edges`/`layout_version`/`resetFlag` plus the
 * `clearResetFlag`/`setLayoutVersion` actions — has no store yet
 * (`pipelineEditorStore.ts`'s own doc comment defers it to "whichever
 * sub-unit owns the pipeline-editor PAGE/composition root" — this
 * component). Each becomes an explicit prop or callback prop, the same
 * "ambient Redux -> explicit prop" swap `useSaveNodeAndEdges.ts`/
 * `useStateValidation.ts` already established; the not-yet-built
 * pipeline-editor page is the natural next owner.
 *
 * DISCLOSED REDESIGN — GA event tracking dropped (baseline: `trackEvent
 * (GA_EVENT_NAMES.PIPELINE_NODE_CREATED, ...)` via `useTrackEvent` from
 * `@/GA`, `FlowEditor.jsx:9,89,254-256`): no such hook/module exists
 * anywhere in this app (grepped the full tree) — same documented gap
 * `useAgentCreation.ts`'s doc comment already established.
 *
 * DROPPED — `data-tour={PIPELINE_TOUR_TARGET_IDS.*}` (baseline:
 * `features/interactive-tours`): out of this Wave-2 batch's scope
 * entirely — same treatment `ApplicationTools.tsx` already established
 * (`no-sideways-features`, no carve-out).
 *
 * DISCLOSED REDESIGN RISK — `./useFlowEditorNodeOperations.ts`'s `onNodeCreateAtPosition` reads a
 * `setYamlJsonObject` ref snapshot instead of the baseline's atomic functional updater (`FlowEditor.
 * jsx:214-220`); `SetYamlJsonObject` still lacks a functional-updater overload (sibling-owned) — real lost-update risk, see that call site's comment.
 *
 * REAL PLUMBING GAP CLOSED HERE: `versionTools`/`llmSettings` — see
 * `./useFlowEditorNodeTypes.tsx`'s doc comment for the full "ambient
 * Formik -> explicit prop, no channel through `nodeTypes` itself" story.
 *
 * `FlowEditorState.StateDrawer` (unit A2j) is imported from `./state/
 * StateDrawer`, re-verified against its real, current `StateDrawerProps`
 * (`isOpen`/`onClose`/`setYamlJsonObject`/`yamlJsonObject`/`disabled`)
 * immediately before writing this version, not assumed from the baseline.
 *
 * `useReactFlow().fitView` is `() => Promise<boolean>` in `@xyflow/react`
 * 12 (the baseline's plain-JS `fitView()` never had to care). Every call
 * site that only wants "fire and forget" goes through `fitViewVoid` — one
 * `void fitView()` wrapper — so `() => void`-typed params (`useFlowEditorReset`/
 * `useFlowEditorInitialFitView`/the imperative handle's `fitView`) never
 * trip `no-misused-promises`.
 */
import type { ReactNode } from 'react';
import { forwardRef, memo, useCallback, useImperativeHandle, useState } from 'react';

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
  readonly onAddNode: (type: string) => FlowNode;
  readonly onRcvAgentEvent: (event: RunSocketEvent) => void;
  readonly setFlowEdges: SetFlowEdges;
  readonly setFlowNodes: SetFlowNodes;
  readonly deleteAllRunNodes: () => void;
  readonly getCurrentExpandState: () => boolean;
  readonly calculateLayoutNodes: (parsedYamlJson: YamlPipelineDocument, shouldDoLayout: boolean, layoutAll: boolean, explicitExpandState?: boolean) => void;
  readonly stopCurrentRun: () => void;
  readonly hasRunsInProgress: () => boolean;
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
          nodes={flowNodes}
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
