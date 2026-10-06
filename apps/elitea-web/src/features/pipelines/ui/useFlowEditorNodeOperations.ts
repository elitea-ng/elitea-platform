import { isFixedParallelAuthoringAllowed } from '../lib/flow-editor/constants/parallel.constants';
/**
 * `FlowEditor.jsx`'s node-creation/layout callbacks (baseline lines 205-
 * 326: `onNodeCreateAtPosition`/`onAddNode`/`calculateLayoutNodes`), split
 * into their own hook purely to keep `FlowEditor.tsx` itself under the
 * §3.5 400-line file-length budget.
 */
import { useCallback, useEffect, useRef } from 'react';

import type { Viewport } from '@xyflow/react';

import { FlowEditorConstants } from '../lib/flow-editor/constants';
import * as FlowEditorHelpers from '../lib/flow-editor/helpers/flowEditor.helpers';
import * as LayoutHelpers from '../lib/flow-editor/helpers/layout.helpers';
import * as NewNodeRevealHelpers from '../lib/flow-editor/helpers/newNodeReveal.helpers';
import * as ParsePipelineHelpers from '../lib/flow-editor/helpers/parsePipeline.helpers';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode, SetFlowEdges, SetFlowNodes, SetYamlJsonObject, YamlPipelineDocumentRef } from '../lib/flow-editor/reactFlowTypes';

export interface UseFlowEditorNodeOperationsArgs {
  readonly flowNodes: readonly FlowNode[];
  readonly setFlowNodes: SetFlowNodes;
  readonly setFlowEdges: SetFlowEdges;
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly yamlJsonObjectRef: YamlPipelineDocumentRef;
  readonly getViewport: () => Viewport;
  readonly setCenter: (x: number, y: number, options?: { readonly zoom?: number; readonly duration?: number }) => unknown;
  readonly getZoom: () => number;
  readonly editorRef: { readonly current: HTMLElement | null };
  readonly editorWidth: number;
  readonly editorHeight: number;
}

export interface UseFlowEditorNodeOperationsResult {
  readonly onNodeCreateAtPosition: (type: string, position: { readonly x: number; readonly y: number }) => FlowNode;
  readonly onAddNode: (type: string) => FlowNode;
  readonly calculateLayoutNodes: (parsedYamlJson: YamlPipelineDocument, shouldDoLayout: boolean, layoutAll: boolean, expanded: boolean) => void;
}

/** `FlowEditor.jsx:205-261` — creates a new YAML node + matching flow node at a given canvas position, wiring `entry_point` on the first non-Condition node the way the baseline does. */
function useOnNodeCreateAtPosition(args: Pick<UseFlowEditorNodeOperationsArgs, 'flowNodes' | 'setFlowNodes' | 'setYamlJsonObject' | 'yamlJsonObjectRef'>): UseFlowEditorNodeOperationsResult['onNodeCreateAtPosition'] {
  const { flowNodes, setFlowNodes, setYamlJsonObject, yamlJsonObjectRef } = args;

  return useCallback(
    (type, position) => {
      if (!isFixedParallelAuthoringAllowed(type)) throw new Error('Fixed Parallel authoring is not enabled.');
      const newNode = FlowEditorHelpers.generateNodeIdByType(type, flowNodes);
      // DISCLOSED REDESIGN RISK — baseline (`FlowEditor.jsx:214-220`) calls
      // `setYamlJsonObject(prevValue => ({...prevValue, ...}))`, an atomic
      // functional updater: React/Redux guarantees `prevValue` is the
      // latest committed state at update time, so two calls queued in the
      // same tick (e.g. two rapid `onAddNode` calls before React flushes)
      // both see each other's writes. `SetYamlJsonObject` here (`../lib/
      // flow-editor/reactFlowTypes.ts`, sibling-owned) is typed
      // `(next: YamlPipelineDocument) => void` — no functional-updater
      // overload — so this can only read `yamlJsonObjectRef.current`, a
      // ref mirrored via a `useEffect` in `useYamlJsonObjectRef` (i.e.
      // updated only after commit, not synchronously). Two calls into this
      // callback within the same commit window would both read the same
      // stale `currentDoc` and the second call's write would silently
      // clobber the first's `nodes` entry — a real, currently-unmitigated
      // lost-update risk, not just a style downgrade. Still true as of this
      // pass: `SetYamlJsonObject`'s signature has not gained a functional
      // overload, so the atomic pattern cannot be restored from this file.
      const currentDoc = yamlJsonObjectRef.current;
      const shouldSetEntryPoint = type !== FlowEditorConstants.PipelineNodeTypes.Condition && type !== FlowEditorConstants.PipelineNodeTypes.End && !currentDoc?.entry_point;

      if (type !== FlowEditorConstants.PipelineNodeTypes.Condition) {
        setYamlJsonObject({
          ...currentDoc,
          nodes: [...(currentDoc?.nodes ?? []), newNode],
          ...(shouldSetEntryPoint ? { entry_point: newNode.id } : {}),
        });
      }

      const label = type === FlowEditorConstants.PipelineNodeTypes.Condition ? 'Condition' : newNode.id;
      const newFlowNode: FlowNode = {
        id: newNode.id,
        type,
        data: {
          label,
          ...(type === FlowEditorConstants.PipelineNodeTypes.Condition
            ? { condition: { condition_input: [], condition_definition: '', conditional_outputs: [], default_output: '' } }
            : {}),
          ...(type === FlowEditorConstants.PipelineNodeTypes.Decision
            ? { decision: { input: [], description: '', nodes: [], default_output: '' } }
            : {}),
        },
        position,
        selected: true,
      };

      setFlowNodes(prevNodes => [...prevNodes.map(node => ({ ...node, selected: false })), newFlowNode]);

      return newFlowNode;
    },
    [flowNodes, setFlowNodes, setYamlJsonObject, yamlJsonObjectRef],
  );
}

/**
 * `FlowEditor.jsx:263-276` — picks a free canvas position centred in the current viewport (stacked below the existing cards when the centre is taken — see `calculatePositionForNewNode`), then delegates to `onNodeCreateAtPosition`.
 *
 * The stacking rule fires on the very first add (an `End` node always sits
 * at the viewport centre, so the new card is always pushed below it), which
 * put most of the card under the fold with nothing bringing it back into
 * view. `calculateRevealCenterForNewNode` decides whether the viewport has
 * to move at all and returns a target that keeps the current zoom unless
 * the card cannot fit at it — unlike `fitView`, which rescales the whole
 * canvas to the whole graph on every add, needed or not.
 */
function useOnAddNode(
  args: Pick<UseFlowEditorNodeOperationsArgs, 'flowNodes' | 'getViewport' | 'setCenter' | 'editorRef' | 'editorWidth' | 'editorHeight'>,
  onNodeCreateAtPosition: UseFlowEditorNodeOperationsResult['onNodeCreateAtPosition'],
): UseFlowEditorNodeOperationsResult['onAddNode'] {
  const { flowNodes, getViewport, setCenter, editorRef, editorWidth, editorHeight } = args;

  // The pending reveal, so an unmounting editor can cancel it. Without this
  // the timer below still fires after the `ReactFlowProvider` has gone —
  // adding a node and navigating away inside the delay left `setCenter`
  // writing to a disposed store.
  const revealTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      if (revealTimer.current !== undefined) clearTimeout(revealTimer.current);
    },
    [],
  );

  return useCallback(
    type => {
      const viewPort = getViewport();
      const { xPos, yPos } = FlowEditorHelpers.calculatePositionForNewNode(
        (editorWidth / 2 - 230 - viewPort.x) / viewPort.zoom,
        (editorHeight / 2 - 200 - viewPort.y) / viewPort.zoom,
        flowNodes,
        type,
      );
      const created = onNodeCreateAtPosition(type, { x: xPos, y: yPos });
      // Deferred and re-measured rather than reusing `editorWidth`/
      // `editorHeight` above: those are the pane's size DURING the add, and a
      // node that opens the admission panel shrinks the pane immediately
      // after it. `setCenter` is called without a duration, like every other
      // reveal in this editor — `settle()` in the @visual suite freezes CSS
      // transitions, not React Flow's own d3 one, so an animated pan would
      // make every shot that adds a node a timing race.
      if (revealTimer.current !== undefined) clearTimeout(revealTimer.current);
      revealTimer.current = setTimeout(() => {
        revealTimer.current = undefined;
        const pane = editorRef.current;
        const revealCenter = NewNodeRevealHelpers.calculateRevealCenterForNewNode({ x: xPos, y: yPos }, type, getViewport(), {
          width: pane?.offsetWidth ?? editorWidth,
          height: pane?.offsetHeight ?? editorHeight,
        });
        if (revealCenter) void setCenter(revealCenter.x, revealCenter.y, { zoom: revealCenter.zoom });
      }, NewNodeRevealHelpers.NEW_NODE_REVEAL_DELAY_MS);
      return created;
    },
    [getViewport, setCenter, editorRef, editorWidth, editorHeight, flowNodes, onNodeCreateAtPosition],
  );
}

/** `FlowEditor.jsx:278-325` — re-parses the YAML into a graph, optionally runs the dagre auto-layout pass, then merges the result back onto the live canvas nodes/edges (preserving each node's live position unless `layoutAll`). */
function useCalculateLayoutNodes(
  args: Pick<UseFlowEditorNodeOperationsArgs, 'flowNodes' | 'getZoom' | 'editorRef' | 'setFlowEdges' | 'setFlowNodes'>,
): UseFlowEditorNodeOperationsResult['calculateLayoutNodes'] {
  const { flowNodes, getZoom, editorRef, setFlowEdges, setFlowNodes } = args;

  return useCallback(
    (parsedYamlJson, shouldDoLayout, layoutAll, expanded) => {
      const parsed = ParsePipelineHelpers.parseYaml(parsedYamlJson, FlowEditorConstants.ORIENTATION.vertical);
      // `parseYaml`'s `FlowGraphNode`/`FlowGraphEdge` (structural, unit A2c) ->
      // `doLayout`'s `@xyflow/react`-typed `FlowNode`/`FlowEdge` (unit A2d) —
      // real cross-file structural-vs-nominal boundary, same "cast once,
      // documented" precedent `pipelineFlow.types.ts`'s own header
      // establishes for the reverse direction.
      let finalNodes: readonly FlowNode[] = parsed.nodes as unknown as readonly FlowNode[];
      let finalEdges: readonly FlowEdge[] = parsed.edges as unknown as readonly FlowEdge[];

      if (shouldDoLayout) {
        const measuredNodes = FlowEditorHelpers.measureNodes(flowNodes, getZoom(), editorRef);
        const layout = LayoutHelpers.doLayout({
          nodes: finalNodes,
          edges: finalEdges,
          flowNodes: measuredNodes,
          orientation: FlowEditorConstants.ORIENTATION.vertical,
          expanded,
        });
        finalNodes = layout.nodes;
        finalEdges = layout.edges;
      }

      setFlowNodes(prevNodes =>
        finalNodes.map(node => {
          const foundNode = prevNodes.find(prevNode => prevNode.type === node.type && prevNode.id === node.id);
          return foundNode ? { ...foundNode, position: !layoutAll ? foundNode.position : node.position, data: { ...node.data } } : node;
        }),
      );
      setFlowEdges(prevEdges =>
        finalEdges.map(edge => {
          const foundEdge = prevEdges.find(prevEdge => prevEdge.source === edge.source && prevEdge.target === edge.target && edge.id === prevEdge.id);
          return foundEdge ? { ...foundEdge, data: { ...foundEdge.data } } : edge;
        }),
      );
    },
    [flowNodes, getZoom, editorRef, setFlowEdges, setFlowNodes],
  );
}

export function useFlowEditorNodeOperations(args: UseFlowEditorNodeOperationsArgs): UseFlowEditorNodeOperationsResult {
  const onNodeCreateAtPosition = useOnNodeCreateAtPosition(args);
  const onAddNode = useOnAddNode(args, onNodeCreateAtPosition);
  const calculateLayoutNodes = useCalculateLayoutNodes(args);

  return { onNodeCreateAtPosition, onAddNode, calculateLayoutNodes };
}
