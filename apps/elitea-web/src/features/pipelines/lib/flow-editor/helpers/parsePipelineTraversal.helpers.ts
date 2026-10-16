/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/parsePipeline.helpers.js:449-599` (unit A2c) —
 * `goThroughNodesTree` (the recursive YAML-node-graph walker that dispatches
 * to a branch handler per node shape) and `parseNodes` (its entry point,
 * seeding the synthetic End node).
 *
 * **Disclosed refactor (behaviour-preserving):** the baseline's
 * `goThroughNodesTree` has a `while (!isEnd)` loop whose body always sets
 * `isEnd = true` on every path (every `if`/`else` branch either calls a
 * handler or the End-edge fallback, then sets `isEnd = true`) — so the loop
 * runs its body exactly 0 or 1 times; it never actually loops. This port
 * drops the vestigial `while`/`isEnd` bookkeeping in favour of a single
 * dispatch (identical control flow, no observable difference — every branch
 * still executes the exact same handler, still exactly once).
 *
 * Each branch handler (`./parsePipelineLegacyBranches.helpers.ts`,
 * `./parsePipelineModernBranches.helpers.ts`) now RETURNS its branch target
 * ids instead of recursing itself (see those files' doc comments for why);
 * this function performs the recursive call, one per returned branch,
 * exactly where the baseline's handlers used to.
 */
import { checkAndAddEdge, checkAndAddNode } from './parsePipelineGraphPrimitives.helpers';
import { handleConditionNode, handleDecisionNode } from './parsePipelineLegacyBranches.helpers';
import {
  handleHitlNode,
  handleNewDecisionNode,
  handleRouterNode,
  handleTransitionNode,
} from './parsePipelineModernBranches.helpers';
import { getNodePosition } from './parsePipelineState.helpers';
import { EDGE_PREFIX, ORIENTATION, PipelineNodeTypes, type Orientation } from '../constants/flowEditor.constants';
import type { FlowGraphEdge, FlowGraphNode, YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';

export const goThroughNodesTree = (
  yamlNodes: readonly YamlPipelineNode[],
  rootNodeId: string | undefined,
  nodes: FlowGraphNode[],
  edges: FlowGraphEdge[],
  interrupt_after: readonly string[],
  interrupt_before: readonly string[],
  orientation?: Orientation,
): void => {
  const currentJsonNode = yamlNodes.find(node => node?.id === rootNodeId);
  if (!currentJsonNode) return;

  const { id, type } = currentJsonNode;
  checkAndAddNode({ nodes, id, type, data: { label: id }, orientation });

  const remainingYamlNodes = yamlNodes.filter(node => node.id !== rootNodeId);
  const recurse = (branches: readonly string[]): void => {
    branches.forEach(branch => {
      goThroughNodesTree(remainingYamlNodes, branch, nodes, edges, interrupt_after, interrupt_before, orientation);
    });
  };

  if (type === PipelineNodeTypes.Router) {
    // Router-node `routes` is always the array form (`FlowEditorConstants.
    // createRouterNodeData` initializes `routes: []`) — narrowed here, not
    // re-validated, matching the baseline's own author-discipline convention.
    recurse(
      handleRouterNode({
        interrupt_before,
        interrupt_after,
        currentJsonNode: { ...currentJsonNode, routes: currentJsonNode.routes as readonly string[] | undefined },
        nodes,
        edges,
        orientation,
      }).branches,
    );
    return;
  }
  if (type === PipelineNodeTypes.Hitl) {
    // HITL-node `routes` is always the `{ [action]: targetId }` object form
    // (`createHitlNodeData`'s `routes: { approve, edit, reject }`).
    recurse(
      handleHitlNode({
        interrupt_before,
        interrupt_after,
        currentJsonNode: {
          ...currentJsonNode,
          routes: currentJsonNode.routes as Readonly<Record<string, string>> | undefined,
        },
        nodes,
        edges,
        orientation,
      }).branches,
    );
    return;
  }
  if (currentJsonNode.condition) {
    // legacy condition node
    recurse(handleConditionNode({ interrupt_before, interrupt_after, currentJsonNode, nodes, edges, orientation }).branches);
    return;
  }
  if (currentJsonNode.decision) {
    // legacy decision node
    recurse(handleDecisionNode({ interrupt_before, interrupt_after, currentJsonNode, nodes, edges, orientation }).branches);
    return;
  }
  if (type === PipelineNodeTypes.Decision) {
    // new decision node
    recurse(handleNewDecisionNode({ interrupt_before, interrupt_after, currentJsonNode, nodes, edges, orientation }).branches);
    return;
  }
  if (currentJsonNode.transition && currentJsonNode.transition !== PipelineNodeTypes.End) {
    recurse(
      handleTransitionNode({
        interrupt_before,
        interrupt_after,
        currentJsonNode: { id, transition: currentJsonNode.transition },
        nodes,
        edges,
        orientation,
      }).branches,
    );
    return;
  }
  checkAndAddEdge({ edges, edgeId: `${EDGE_PREFIX}${id}---EliteAPipelineEnd`, source: id, target: PipelineNodeTypes.End });
};

export const parseNodes = (
  yamlJson: YamlPipelineDocument | undefined,
  orientation: Orientation = ORIENTATION.vertical,
): { readonly nodes: FlowGraphNode[]; readonly edges: FlowGraphEdge[] } => {
  const nodes: FlowGraphNode[] = [
    {
      id: PipelineNodeTypes.End,
      type: PipelineNodeTypes.End,
      data: { label: 'End' },
      position: getNodePosition([], orientation),
    },
  ];
  const edges: FlowGraphEdge[] = [];
  if (yamlJson) {
    const yamlNodes = (yamlJson.nodes ?? []).filter((node): node is YamlPipelineNode => Boolean(node));
    const { entry_point, interrupt_after, interrupt_before } = yamlJson;
    const realInterruptBefore = Array.isArray(interrupt_before) ? interrupt_before : [];
    const realInterruptAfter = Array.isArray(interrupt_after) ? interrupt_after : [];
    goThroughNodesTree(yamlNodes, entry_point, nodes, edges, realInterruptAfter, realInterruptBefore, orientation);
    yamlNodes.forEach(node => {
      if (!nodes.find(parsedNode => parsedNode.id === node.id)) {
        goThroughNodesTree(yamlNodes, node.id, nodes, edges, realInterruptAfter, realInterruptBefore, orientation);
      }
    });
    // Keep authored pause labels on all stored-node routes, including END.
    // Synthetic legacy branch edges retain their existing label behavior.
    const storedNodeIds = new Set(yamlNodes.map((node) => node.id));
    edges.forEach((edge, index) => {
      if (edge.target !== PipelineNodeTypes.End && !storedNodeIds.has(edge.target)) return;
      if (realInterruptAfter.includes(edge.source) || realInterruptBefore.includes(edge.target)) {
        edges[index] = { ...edge, data: { ...edge.data, label: 'interrupt' } };
      }
    });
  }

  return { nodes, edges };
};
