/**
 * Ported verbatim from `apps/elitea-ui/src/[fsd]/features/pipelines/
 * flow-editor/lib/hooks/useConnectNodes.hooks.js` (100 lines, unit A2d) —
 * `onConnect` for `@xyflow/react`'s `<ReactFlow>`, dispatching to the
 * right `connectionOperations.helpers.ts` handler based on the
 * source/target node types.
 */
import { useCallback } from 'react';

import type { Connection } from '@xyflow/react';

import * as ConnectionOperationsHelpers from '../helpers/connectionOperations.helpers';
import { handleToConditionNodeConnection, handleToDecisionNodeConnection } from '../helpers/connectionOperations.toNode.helpers';
import * as EdgeOperationsHelpers from '../helpers/edgeOperations.helpers';
import type { NodeIdRenameMap } from '../helpers/edgeOperations.helpers';
import { connectErrorRoute, ERROR_OUTPUT_HANDLE } from '../helpers/nodeErrorRoute.helpers';
import * as NodeTypeHelpers from '../helpers/nodeType.helpers';
import type { FlowNode, SetFlowEdges, SetFlowNodes, SetYamlJsonObject, YamlPipelineDocumentRef } from '../reactFlowTypes';

export interface UseConnectNodesArgs {
  readonly flowNodes: readonly FlowNode[];
  readonly yamlJsonObjectRef: YamlPipelineDocumentRef;
  readonly setFlowNodes: SetFlowNodes;
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly setFlowEdges: SetFlowEdges;
  readonly disabled?: boolean;
}

interface DispatchArgs {
  readonly connection: Connection;
  readonly flowNodes: readonly FlowNode[];
  readonly yamlJsonObjectRef: YamlPipelineDocumentRef;
  readonly setFlowNodes: SetFlowNodes;
  readonly setYamlJsonObject: SetYamlJsonObject;
}

interface DispatchResult {
  readonly result: ConnectionOperationsHelpers.ConnectionResult | null;
  readonly shouldChangeNodeIdMap: NodeIdRenameMap;
}

type DispatchRule = readonly [predicate: (args: DispatchArgs) => boolean, handle: (args: DispatchArgs) => DispatchResult];

/**
 * Ordered [predicate, handler] table — the exact same order as the
 * baseline's if/else-if chain (`useConnectNodes.hooks.js:24-79`), just
 * data-driven instead of a long if/else ladder, to stay under §3.5's
 * cyclomatic-complexity budget without changing which handler wins when
 * more than one predicate could match (order is significant and preserved
 * 1:1 — e.g. a decision-to-decision connection must still resolve via
 * "from decision", checked before "to decision").
 */
const DISPATCH_RULES: readonly DispatchRule[] = [
  // 5b-1 (C6): the error output handle writes `settings.error_transition` only.
  [
    ({ connection }) => connection.sourceHandle === ERROR_OUTPUT_HANDLE,
    ({ connection, yamlJsonObjectRef, setYamlJsonObject }) => {
      const document = yamlJsonObjectRef.current;
      if (document) setYamlJsonObject(connectErrorRoute(document, connection.source, connection.target));
      return {
        result: { removeEdgePredicate: (edge) => edge.source === connection.source && edge.sourceHandle === ERROR_OUTPUT_HANDLE },
        shouldChangeNodeIdMap: {},
      };
    },
  ],
  [
    ({ connection }) => NodeTypeHelpers.isFromHitlNode(connection),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject }) => ({
      result: ConnectionOperationsHelpers.handleFromHitlNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject }),
      shouldChangeNodeIdMap: {},
    }),
  ],
  [
    ({ connection }) => NodeTypeHelpers.isFromConditionNode(connection),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes }) => ({
      result: ConnectionOperationsHelpers.handleFromConditionNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes }),
      shouldChangeNodeIdMap: {},
    }),
  ],
  [
    ({ connection }) => NodeTypeHelpers.isConnectToConditionNode(connection),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes, flowNodes }) => {
      const result = handleToConditionNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes, flowNodes });
      return { result, shouldChangeNodeIdMap: result?.shouldChangeNodeIdMap ?? {} };
    },
  ],
  [
    ({ connection }) => NodeTypeHelpers.isFromRouterNode(connection),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject }) => ({
      result: ConnectionOperationsHelpers.handleFromRouterNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject }),
      shouldChangeNodeIdMap: {},
    }),
  ],
  [
    ({ connection, yamlJsonObjectRef }) => NodeTypeHelpers.isFromDecisionNode({ connection, yamlJsonObjectRef }),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes }) => ({
      result: ConnectionOperationsHelpers.handleFromDecisionNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes }),
      shouldChangeNodeIdMap: {},
    }),
  ],
  [
    ({ connection, yamlJsonObjectRef }) => NodeTypeHelpers.isConnectToDecisionNode({ connection, yamlJsonObjectRef }),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes, flowNodes }) => {
      const result = handleToDecisionNodeConnection({ connection, yamlJsonObjectRef, setYamlJsonObject, setFlowNodes, flowNodes });
      return { result, shouldChangeNodeIdMap: result?.shouldChangeNodeIdMap ?? {} };
    },
  ],
  [
    ({ connection }) => NodeTypeHelpers.isConnectToEndNode(connection),
    ({ connection, yamlJsonObjectRef, setYamlJsonObject }) => ({
      result: ConnectionOperationsHelpers.handleConnectionToEndNode({ connection, yamlJsonObjectRef, setYamlJsonObject }) ?? null,
      shouldChangeNodeIdMap: {},
    }),
  ],
];

/**
 * Determines connection type and routes to the matching `connectionOperations`
 * handler — split out of `useConnectNodes`'s callback purely to stay under
 * the §3.5 cyclomatic-complexity budget.
 */
function dispatchConnection(args: DispatchArgs): DispatchResult {
  const rule = DISPATCH_RULES.find(([predicate]) => predicate(args));
  if (rule) return rule[1](args);

  const { connection, yamlJsonObjectRef, setYamlJsonObject } = args;
  return { result: ConnectionOperationsHelpers.handleNormalConnection({ connection, yamlJsonObjectRef, setYamlJsonObject }), shouldChangeNodeIdMap: {} };
}

export function useConnectNodes({
  flowNodes,
  yamlJsonObjectRef,
  setFlowNodes,
  setYamlJsonObject,
  setFlowEdges,
  disabled,
}: UseConnectNodesArgs): (connection: Connection) => void {
  return useCallback(
    (connection: Connection) => {
      if (disabled) return;

      const { result, shouldChangeNodeIdMap } = dispatchConnection({ connection, flowNodes, yamlJsonObjectRef, setFlowNodes, setYamlJsonObject });

      // Early return if connection was rejected
      if (!result) return;

      // Create and apply edge changes
      const { showInterruptLabel = false, edgeToRemove = '', removeEdgePredicate } = result;
      const newEdge = EdgeOperationsHelpers.createNewEdge(connection, showInterruptLabel);
      ConnectionOperationsHelpers.applyEdgeChanges(setFlowEdges, newEdge, shouldChangeNodeIdMap, edgeToRemove, removeEdgePredicate);
    },
    [disabled, flowNodes, setFlowEdges, setFlowNodes, setYamlJsonObject, yamlJsonObjectRef],
  );
}
