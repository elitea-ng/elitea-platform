/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/layout.helpers.js` (102 lines, unit A2d) — the `@dagrejs/dagre`
 * auto-layout pass run whenever the canvas re-arranges nodes.
 *
 * **Disclosed dependency addition:** `@dagrejs/dagre` was not yet a
 * dependency of `apps/elitea-web` (orthogonal to `@xyflow/react`, which
 * only renders/positions, never computes a layout). Added at `1.1.5`, the
 * exact version pinned by the baseline (`apps/elitea-ui/package.json:44`,
 * `"^1.1.5"` — pinned without the caret per this repo's exact-version
 * convention, matching `@xyflow/react`'s own `"12.11.2"` entry).
 *
 * Split into several small helpers purely to stay under §3.5's
 * cyclomatic-complexity budget — `doLayout`'s control flow is unchanged
 * from the baseline, just no longer inlined into one function body.
 */
import * as dagre from '@dagrejs/dagre';

import { NodeHeightMap, ORIENTATION } from '../constants/flowEditor.constants';
import type { FlowEdge, FlowNode } from '../reactFlowTypes';

/**
 * The node label this pass stores and reads back.
 *
 * `@dagrejs/dagre@3` ships its own types instead of `@types/dagre`, and the
 * `@dagrejs/graphlib@4` types underneath it declare a label as
 * `Label = string | number | boolean | Record<string, any>`. So the UNPARAMETERISED
 * `dagre.graphlib.Graph` hands every `g.node()` read back as `any`, and each
 * destructure of it is an `oxlint typescript(no-unsafe-assignment)` error (4
 * sites, all in `arrangedNodeFor`). Naming the label type restores the checking
 * the 1.x `@types` package used to give for free.
 *
 * `x`/`y` are optional because `dagre.layout()` is what writes them — they do
 * not exist at `setNode()` time, and the type must not claim otherwise. A
 * `type` alias rather than an `interface` on purpose: only an alias gets the
 * implicit index signature that makes it assignable to graphlib's `Label`, which
 * is what lets `dagre.layout()` accept this graph.
 */
type LayoutNodeLabel = {
  label: string;
  width: number;
  height: number;
  x?: number;
  y?: number;
};

/** Edge labels carry nothing here — `setDefaultEdgeLabel` returns `{}`. */
type LayoutEdgeLabel = Record<string, unknown>;

/** The graph `doLayout` builds, with all three label slots named (see {@link LayoutNodeLabel}). */
type LayoutGraph = dagre.graphlib.Graph<dagre.GraphLabel, LayoutNodeLabel, LayoutEdgeLabel>;

export interface DoLayoutArgs {
  readonly nodes: readonly FlowNode[];
  readonly edges: readonly FlowEdge[];
  readonly flowNodes?: readonly FlowNode[];
  readonly orientation?: string;
  readonly expanded?: boolean;
}

export interface DoLayoutResult {
  readonly nodes: FlowNode[];
  readonly edges: FlowEdge[];
}

function computeNodeHeight(node: FlowNode, flowNodes: readonly FlowNode[] | undefined, expanded: boolean): number {
  if (!expanded) return 44;
  const foundFlowNode = flowNodes?.find(flowNode => flowNode.id === node.id);
  return (
    foundFlowNode?.measured?.height ||
    node.measured?.height ||
    (node as { height?: number }).height ||
    NodeHeightMap[node.type ?? ''] ||
    500
  );
}

interface PopulateGraphArgs {
  readonly nodes: readonly FlowNode[];
  readonly edges: readonly FlowEdge[];
  readonly flowNodes: readonly FlowNode[] | undefined;
  readonly expanded: boolean;
}

function populateGraph(g: LayoutGraph, { nodes, edges, flowNodes, expanded }: PopulateGraphArgs): void {
  for (const node of nodes) {
    g.setNode(node.id, {
      label: node.data.label ?? '',
      width: 460,
      height: computeNodeHeight(node, flowNodes, expanded ?? true),
    });
  }
  for (const edge of edges) {
    g.setEdge(edge.source, edge.target);
  }
}

function arrangedNodeFor(g: LayoutGraph, nodeId: string, nodes: readonly FlowNode[]): FlowNode | undefined {
  const node = g.node(nodeId);
  // `x`/`y` join the existing guard rather than adding a second `if`: they are
  // written by `dagre.layout()` above, so a node reaching here without them did
  // not take part in the layout and has no position to convert.
  if (!nodeId || !node || node.x === undefined || node.y === undefined) return undefined;

  const { x, y, width, height } = node;
  const nodeData = nodes.find(flowNode => flowNode.id === nodeId);
  if (!nodeData?.type) return undefined;

  return {
    id: nodeId,
    type: nodeData.type,
    // Convert from Dagre's center coordinates to React Flow's top-left coordinates.
    position: { x: x - width / 2, y: y - height / 2 },
    data: nodeData.data,
    // `nodeData?.selected` (baseline, `layout.helpers.js`) leaves `selected`
    // undefined rather than defaulting to `false` -- under this project's
    // `exactOptionalPropertyTypes`, an optional field must be omitted rather
    // than explicitly assigned `undefined`, so the property is only spread in
    // when a value actually exists.
    ...(nodeData?.selected !== undefined ? { selected: nodeData.selected } : {}),
    measured: { width, height },
  };
}

/** Dagre reduces multiple parallel edges between the same two nodes to one — restore every original edge. */
function restoreParallelEdges(g: LayoutGraph, edges: readonly FlowEdge[]): FlowEdge[] {
  const arrangedEdges: FlowEdge[] = [];
  for (const item of g.edges()) {
    const edgeData = edges.filter(edge => edge.source === item.v && edge.target === item.w);
    for (const edge of edgeData) {
      arrangedEdges.push({ ...edge });
    }
  }
  return arrangedEdges;
}

export function doLayout({ nodes, edges, flowNodes, orientation = ORIENTATION.vertical, expanded = true }: DoLayoutArgs): DoLayoutResult {
  const g: LayoutGraph = new dagre.graphlib.Graph();
  const isHorizontal = orientation === ORIENTATION.horizontal;

  g.setGraph({
    rankdir: isHorizontal ? 'LR' : 'TB',
    align: isHorizontal ? 'DL' : 'UL',
    nodesep: isHorizontal ? 400 : 700, // Increased node separation
    ranksep: 250, // Increased rank separation from 200px to 250px for edge labels
    marginx: 60, // Increased horizontal margin
    marginy: 60, // Increased vertical margin
  });
  g.setDefaultEdgeLabel(() => ({}));

  populateGraph(g, { nodes, edges, flowNodes, expanded });

  dagre.layout(g);

  const arrangedNode = g
    .nodes()
    .map(nodeId => arrangedNodeFor(g, nodeId, nodes))
    .filter((node): node is FlowNode => Boolean(node));

  return {
    nodes: arrangedNode,
    edges: restoreParallelEdges(g, edges),
  };
}
