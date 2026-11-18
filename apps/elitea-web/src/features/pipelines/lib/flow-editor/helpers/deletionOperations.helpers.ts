/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/deletionOperations.helpers.js` (330 lines, unit A2d) —
 * node/edge deletion: confirm-dialog copy, YAML reference cleanup on node
 * delete, and the per-source-node-type dispatch for edge delete.
 *
 * Collaborators (`../constants/flowEditor.constants`, `./nodeOperations.
 * helpers`, `./nodeType.helpers`, `./yamlUpdate.helpers`) are unit A2c's
 * landed, real exports — not stubs.
 */
import { clearDeletedErrorTarget, clearErrorRoute, ERROR_OUTPUT_HANDLE } from './nodeErrorRoute.helpers';
import {
  CONDITION_NODE_ID_SUFFIX,
  DECISION_NODE_ID_SUFFIX,
  DEFAULT_OUTPUT,
  HITL_HANDLE_ID_SUFFIX,
  PipelineNodeTypes,
} from '../constants/flowEditor.constants';
import type { YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../reactFlowTypes';
import * as FlowNodeUpdateHelpers from './flowNodeUpdate.helpers';
import * as DeletionRepairHelpers from './deletionRepair.helpers';
import * as NodeOperationsHelpers from './nodeOperations.helpers';
import * as NodeTypeHelpers from './nodeType.helpers';
import * as YamlUpdateHelpers from './yamlUpdate.helpers';

export function getConfirmContent(nodes: readonly unknown[], edges: readonly unknown[]): string {
  if (nodes.length && edges.length) return 'Are you sure to delete the selected nodes and edges ';

  if (nodes.length) {
    if (nodes.length < 2) return 'Are you sure to delete the selected node ';
    return 'Are you sure to delete the selected nodes ';
  }

  if (edges.length) {
    if (edges.length < 2) return 'Are you sure to delete the selected edge ';
    return 'Are you sure to delete the selected edges ';
  }

  return '';
}

/**
 * Updates a single YAML node by ID with a transformation function.
 *
 * `as YamlPipelineDocument` below (and at every other cast in this file):
 * `YamlPipelineDocument`/`YamlPipelineNode` are unit A2c's types, declared
 * with plain `field?: T` (no explicit `| undefined`). Under this repo's
 * `exactOptionalPropertyTypes: true`, spreading a source object into a
 * fresh literal that also assigns a field an explicitly-`| undefined`-typed
 * expression (`x?.y` chains, `condition: undefined` clears, etc. — all
 * baseline-faithful) makes that fresh object literal's INFERRED type
 * include `| undefined` on that field, which structurally fails against
 * the narrower declared type even though the VALUE is identical to simply
 * omitting the key (this repo's own `strict`+`exactOptionalPropertyTypes`
 * combination, not a real behavioural gap). A2c's own same-shaped helpers
 * (`yamlUpdate.helpers.ts`'s `updateYamlNodeProperty`) dodge this by
 * staying generic over `TNode extends Record<string, unknown>` all the way
 * through; this file's functions are already exported with the CONCRETE
 * `YamlPipelineDocument`/`YamlPipelineNode` signatures other sub-units
 * expect, so the cast is applied at the return boundary instead of
 * reshaping every signature.
 */
function updateYamlNodeById(
  yamlJsonObject: YamlPipelineDocument,
  nodeId: string,
  updateFn: (node: YamlPipelineNode) => YamlPipelineNode,
): YamlPipelineDocument {
  return {
    ...yamlJsonObject,
    nodes: yamlJsonObject.nodes?.map(yamlNode => (yamlNode.id === nodeId ? updateFn(yamlNode) : yamlNode)),
  } as YamlPipelineDocument;
}

/** Updates a single flow node by ID with a transformation function. */
function updateFlowNodeById(
  flowNodes: readonly FlowNode[],
  nodeId: string,
  updateFn: (node: FlowNode) => FlowNode,
): FlowNode[] {
  return flowNodes.map(node => (node.id === nodeId ? updateFn(node) : node));
}

interface YamlAndFlowNodes {
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly flowNodes: FlowNode[];
}

/** Updates both YAML and flow nodes with paired transformations. */
function updateYamlAndFlowNodes(
  yamlJsonObject: YamlPipelineDocument,
  flowNodes: readonly FlowNode[],
  yamlNodeId: string,
  flowNodeId: string,
  yamlUpdateFn: (node: YamlPipelineNode) => YamlPipelineNode,
  flowUpdateFn: (node: FlowNode) => FlowNode,
): YamlAndFlowNodes {
  return {
    yamlJsonObject: updateYamlNodeById(yamlJsonObject, yamlNodeId, yamlUpdateFn),
    flowNodes: updateFlowNodeById(flowNodes, flowNodeId, flowUpdateFn),
  };
}

/** Clears a node property and sets transition to End. */
function clearNodePropertyAndSetEnd<TNode extends Record<string, unknown>>(
  yamlNode: TNode,
  propertyName: string,
): TNode {
  return {
    ...yamlNode,
    [propertyName]: undefined,
    transition: PipelineNodeTypes.End,
  };
}

/** Generates a new timestamped node for renaming. */
function generateRenamedNode(node: FlowNode, nodeType: string, suffix: string): FlowNode {
  return FlowNodeUpdateHelpers.renameFlowNode(node, NodeOperationsHelpers.generateTimestampedNodeId(nodeType, suffix));
}

/** Generic handler for clearing a node property (condition or decision). */
function handleSpecialNodeDeletion(
  node: FlowNode,
  yamlJsonObject: YamlPipelineDocument,
  suffix: string,
  clearFn: (yamlNode: YamlPipelineNode) => YamlPipelineNode,
): YamlPipelineDocument {
  const ownerNodeId = NodeOperationsHelpers.getOwnerNodeId(node.id, suffix);
  return updateYamlNodeById(yamlJsonObject, ownerNodeId, clearFn);
}

export function handleConditionNodeDeletion(node: FlowNode, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  return handleSpecialNodeDeletion(node, yamlJsonObject, CONDITION_NODE_ID_SUFFIX, YamlUpdateHelpers.clearYamlNodeCondition);
}

export function handleLegacyDecisionNodeDeletion(node: FlowNode, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  return handleSpecialNodeDeletion(node, yamlJsonObject, DECISION_NODE_ID_SUFFIX, YamlUpdateHelpers.clearYamlNodeDecision);
}

export function cleanupNodeReferences(yamlNode: YamlPipelineNode, nodeId: string): YamlPipelineNode {
  if (yamlNode.condition && yamlNode.type !== PipelineNodeTypes.Router) {
    const repaired = DeletionRepairHelpers.repairTargetIfMatches(yamlNode.condition[DEFAULT_OUTPUT], nodeId);
    if (repaired === undefined) return yamlNode;
    return YamlUpdateHelpers.updateYamlNodeCondition(yamlNode, { [DEFAULT_OUTPUT]: repaired });
  }

  if (yamlNode.decision) {
    const repaired = DeletionRepairHelpers.repairTargetIfMatches(yamlNode.decision[DEFAULT_OUTPUT], nodeId);
    if (repaired === undefined) return yamlNode;
    return YamlUpdateHelpers.updateYamlNodeDecision(yamlNode, { [DEFAULT_OUTPUT]: repaired });
  }

  if (yamlNode.type === PipelineNodeTypes.Router || yamlNode.type === PipelineNodeTypes.Decision) {
    return DeletionRepairHelpers.repairBranchNode(yamlNode, nodeId);
  }

  if (yamlNode.type === PipelineNodeTypes.Hitl) {
    const routes = yamlNode.routes as Record<string, string> | undefined;
    // `transition: undefined` is kept from the original: `RawHitlNodeDefinition`
    // is `#[serde(deny_unknown_fields)]` and declares no `transition`
    // (`hitl.rs:103-117`), so a stray one refuses the whole document. js-yaml
    // omits undefined values when it dumps, which is how this strips the key.
    return {
      ...yamlNode,
      transition: undefined,
      ...(routes === undefined ? {} : { routes: DeletionRepairHelpers.repairHitlRoutes(routes, nodeId) }),
    } as unknown as YamlPipelineNode;
  }

  const repairedTransition = DeletionRepairHelpers.repairTargetIfMatches(yamlNode.transition, nodeId);
  if (repairedTransition === undefined) return yamlNode;
  return YamlUpdateHelpers.updateYamlNodeTransition(yamlNode, repairedTransition);
}

export function handleNormalNodeDeletion(node: FlowNode, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  const result = {
    ...yamlJsonObject,
    nodes: yamlJsonObject.nodes
      ?.filter(yamlNode => yamlNode.id !== node.id)
      .map(yamlNode => clearDeletedErrorTarget(cleanupNodeReferences(yamlNode, node.id), node.id)),
    // Clear entry_point if the deleted node was the entry point
    ...(yamlJsonObject.entry_point === node.id ? { entry_point: undefined } : {}),
  } as YamlPipelineDocument;
  return YamlUpdateHelpers.removeInterruptReferences(result, node.id);
}

/**
 * The 9 `handleEdgeTo*`/`handleEdgeFrom*` functions below are all called —
 * every one of them is dispatched from `processEdgeDeletion`'s own
 * if-chain further down in THIS file (verified directly: each name appears
 * exactly once as a call target there). None is imported anywhere outside
 * this module (confirmed via repo-wide grep), including
 * `deletionOperations.helpers.test.ts`, which only exercises them
 * indirectly through `processEdgeDeletion`/`handleNormalNodeDeletion` — so
 * knip is right that the `export` keyword itself is unnecessary, not that
 * the dispatch is broken. `useDeleteItems.ts` (the real caller) already
 * calls `processEdgeDeletion` once per deleted edge, which reaches every
 * one of these branches; there is no missing wiring here.
 */
function handleEdgeToGhostNode(edge: FlowEdge, newFlowNodes: readonly FlowNode[]): FlowNode[] {
  return newFlowNodes.filter(node => node.id !== edge.target);
}

function handleEdgeToConditionNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument, newFlowNodes: readonly FlowNode[]): YamlAndFlowNodes {
  return updateYamlAndFlowNodes(
    yamlJsonObject,
    newFlowNodes,
    edge.source,
    edge.target,
    yamlNode => clearNodePropertyAndSetEnd(yamlNode, 'condition'),
    node => generateRenamedNode(node, 'Condition', CONDITION_NODE_ID_SUFFIX),
  );
}

function handleEdgeToLegacyDecisionNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument, newFlowNodes: readonly FlowNode[]): YamlAndFlowNodes {
  return updateYamlAndFlowNodes(
    yamlJsonObject,
    newFlowNodes,
    edge.source,
    edge.target,
    yamlNode => clearNodePropertyAndSetEnd(yamlNode, 'decision'),
    node => generateRenamedNode(node, 'Decision', DECISION_NODE_ID_SUFFIX),
  );
}

function handleEdgeFromConditionNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument, newFlowNodes: readonly FlowNode[]): YamlAndFlowNodes {
  const ownerId = NodeOperationsHelpers.getOwnerNodeId(edge.source, CONDITION_NODE_ID_SUFFIX);
  const isDefault = NodeTypeHelpers.isDefaultOutputHandle(edge.sourceHandle);

  return updateYamlAndFlowNodes(
    yamlJsonObject,
    newFlowNodes,
    ownerId,
    edge.source,
    yamlNode =>
      YamlUpdateHelpers.updateYamlNodeCondition(
        yamlNode,
        isDefault
          ? { [DEFAULT_OUTPUT]: '' }
          : { conditional_outputs: NodeOperationsHelpers.removeNodeIdFromArray(yamlNode.condition?.conditional_outputs, edge.target) },
      ),
    node => FlowNodeUpdateHelpers.updateFlowNodeConditionOutput(node, isDefault, edge.target),
  );
}

function handleEdgeFromLegacyDecisionNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument, newFlowNodes: readonly FlowNode[]): YamlAndFlowNodes {
  const ownerId = NodeOperationsHelpers.getOwnerNodeId(edge.source, DECISION_NODE_ID_SUFFIX);
  const isDefault = NodeTypeHelpers.isDefaultOutputHandle(edge.sourceHandle);

  if (!isDefault) {
    return { yamlJsonObject, flowNodes: [...newFlowNodes] };
  }

  return updateYamlAndFlowNodes(
    yamlJsonObject,
    newFlowNodes,
    ownerId,
    edge.source,
    yamlNode => YamlUpdateHelpers.updateYamlNodeDecision(yamlNode, { [DEFAULT_OUTPUT]: '' }),
    node => FlowNodeUpdateHelpers.updateFlowNodeDecisionOutput(node, isDefault),
  );
}

function handleEdgeFromNewDecisionNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument, newFlowNodes: readonly FlowNode[]): YamlAndFlowNodes {
  const isDefault = NodeTypeHelpers.isDefaultOutputHandle(edge.sourceHandle);

  return updateYamlAndFlowNodes(
    yamlJsonObject,
    newFlowNodes,
    edge.source,
    edge.source,
    yamlNode =>
      (isDefault
        ? { ...yamlNode, [DEFAULT_OUTPUT]: PipelineNodeTypes.End }
        : { ...yamlNode, nodes: NodeOperationsHelpers.removeNodeIdFromArray(yamlNode.nodes, edge.target) }) as YamlPipelineNode,
    node => node, // Flow node doesn't need update - nodes array is only in YAML
  );
}

function handleEdgeFromRouterNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  const isDefault = NodeTypeHelpers.isDefaultOutputHandle(edge.sourceHandle);

  return updateYamlNodeById(yamlJsonObject, edge.source, yamlNode => ({
    ...yamlNode,
    ...(isDefault
      ? { [DEFAULT_OUTPUT]: PipelineNodeTypes.End }
      : { routes: NodeOperationsHelpers.removeNodeIdFromArray(yamlNode.routes as readonly string[] | undefined, edge.target) }),
  } as YamlPipelineNode));
}

function handleEdgeFromHitlNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  const action = edge.sourceHandle?.replace(`${HITL_HANDLE_ID_SUFFIX}_`, '');

  if (!action) {
    return yamlJsonObject;
  }

  return updateYamlNodeById(yamlJsonObject, edge.source, yamlNode => {
    const routes = { ...(yamlNode.routes as Record<string, string> | undefined) };
    // Same repair as `repairHitlRoutes`: drop `edit`, point the rest at END.
    // `''` is refused by `validate_routes` (`hitl.rs:459-466`) and by the
    // admission gate's `node.route-target`, which is what disabled Save.
    if (action === DeletionRepairHelpers.HITL_EDIT_ACTION) {
      delete routes[action];
    } else {
      routes[action] = PipelineNodeTypes.End;
    }
    return { ...yamlNode, transition: undefined, routes } as unknown as YamlPipelineNode;
  });
}

function handleEdgeFromNormalNode(edge: FlowEdge, yamlJsonObject: YamlPipelineDocument): YamlPipelineDocument {
  return updateYamlNodeById(yamlJsonObject, edge.source, yamlNode => YamlUpdateHelpers.updateYamlNodeTransition(yamlNode, PipelineNodeTypes.End));
}

/**
 * `NodeTypeHelpers`'s predicates (unit A2c) take a plain `{ type?: string }`
 * (no explicit `| undefined`); `FlowNode` (`@xyflow/react`'s `Node<T>`)
 * declares `type?: string | undefined` via its own generic default. Same
 * `exactOptionalPropertyTypes` formalism mismatch as this file's other
 * casts (see `updateYamlNodeById`'s doc comment) -- both describe the
 * identical runtime node.
 */
function asNodeTypeShape(node: FlowNode): { readonly type?: string; readonly id: string } {
  return node as { readonly type?: string; readonly id: string };
}

export function processEdgeDeletion(
  edge: FlowEdge,
  flowNodes: readonly FlowNode[],
  yamlJsonObject: YamlPipelineDocument,
  newFlowNodes: readonly FlowNode[],
): YamlAndFlowNodes {
  // 5b-1 (C6): the error output edge is `settings.error_transition`, never a transition.
  if (edge.sourceHandle === ERROR_OUTPUT_HANDLE) {
    return { yamlJsonObject: clearErrorRoute(yamlJsonObject, edge.source), flowNodes: [...newFlowNodes] };
  }
  const targetNode = flowNodes.find(node => node.id === edge.target);
  if (!targetNode) {
    return { yamlJsonObject, flowNodes: [...newFlowNodes] };
  }

  // Handle edge to special node types
  if (NodeTypeHelpers.isGhostNode(asNodeTypeShape(targetNode))) {
    return { yamlJsonObject, flowNodes: handleEdgeToGhostNode(edge, newFlowNodes) };
  }

  if (NodeTypeHelpers.isConditionNode(asNodeTypeShape(targetNode))) {
    return handleEdgeToConditionNode(edge, yamlJsonObject, newFlowNodes);
  }

  if (NodeTypeHelpers.isLegacyDecisionNode(asNodeTypeShape(targetNode))) {
    return handleEdgeToLegacyDecisionNode(edge, yamlJsonObject, newFlowNodes);
  }

  // Handle edge from special node types
  const sourceNode = flowNodes.find(node => node.id === edge.source);
  if (!sourceNode) {
    return { yamlJsonObject, flowNodes: [...newFlowNodes] };
  }

  if (NodeTypeHelpers.isConditionNode(asNodeTypeShape(sourceNode))) {
    return handleEdgeFromConditionNode(edge, yamlJsonObject, newFlowNodes);
  }

  if (NodeTypeHelpers.isDecisionNode(asNodeTypeShape(sourceNode))) {
    if (NodeTypeHelpers.isLegacyDecisionNode(asNodeTypeShape(sourceNode))) {
      return handleEdgeFromLegacyDecisionNode(edge, yamlJsonObject, newFlowNodes);
    }
    return handleEdgeFromNewDecisionNode(edge, yamlJsonObject, newFlowNodes);
  }

  if (NodeTypeHelpers.isHitlHandle(edge.sourceHandle)) {
    return { yamlJsonObject: handleEdgeFromHitlNode(edge, yamlJsonObject), flowNodes: [...newFlowNodes] };
  }

  if (NodeTypeHelpers.isRouterHandle(edge.sourceHandle)) {
    return { yamlJsonObject: handleEdgeFromRouterNode(edge, yamlJsonObject), flowNodes: [...newFlowNodes] };
  }

  return { yamlJsonObject: handleEdgeFromNormalNode(edge, yamlJsonObject), flowNodes: [...newFlowNodes] };
}
