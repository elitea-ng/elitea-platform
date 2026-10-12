/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * hooks/useIncompleteEdge.hooks.js` (219 lines, unit A2d) — the "drag a
 * connection and drop it on empty canvas" flow: spawns a ghost node +
 * dropdown to pick an existing target or create a new node.
 */
import { useCallback, useMemo, useState } from 'react';

import { addEdge, reconnectEdge, useReactFlow, type Connection, type Edge } from '@xyflow/react';

import { PipelineNodeTypes } from '../constants/flowEditor.constants';
import * as FlowEditorHelpers from '../helpers/flowEditor.helpers';
import type { FlowEdge, FlowNode, YamlPipelineDocumentRef } from '../reactFlowTypes';

export interface UseIncompleteEdgeArgs {
  readonly onConnect: (connection: Connection) => void;
  readonly onNodeCreateAtPosition: (nodeType: string, position: { x: number; y: number }) => { id: string } | undefined;
  readonly yamlJsonObjectRef: YamlPipelineDocumentRef;
  readonly disabled?: boolean;
}

export interface UseIncompleteEdgeResult {
  readonly onConnectEnd: (event: MouseEvent | TouchEvent, connectionState: IncompleteConnectionState) => void;
  readonly onReconnect: (oldEdge: FlowEdge, newConnection: Connection) => void;
  readonly onReconnectEnd: (event: unknown, oldEdge: FlowEdge, handleType: 'source' | 'target') => void;
  readonly showConnectionDropdown: boolean;
  readonly dropdownPosition: { x: number; y: number } | null;
  readonly currentGhostNode: FlowNode | null;
  readonly handleDropdownClose: () => void;
  readonly handleNodeSelect: (selectedNode: { id: string }) => void;
  readonly handleNodeCreate: (nodeType: string) => void;
  readonly availableTargets: readonly FlowNode[];
  readonly availableNodeTypes: readonly string[];
}

interface IncompleteConnectionState {
  readonly isValid: boolean | null;
  readonly fromHandle: { readonly type: 'source' | 'target'; readonly id: string | null } | null;
  readonly fromNode: { readonly id: string } | null;
}

export function useIncompleteEdge({
  onConnect,
  onNodeCreateAtPosition,
  yamlJsonObjectRef,
  disabled,
}: UseIncompleteEdgeArgs): UseIncompleteEdgeResult {
  const reactFlowInstance = useReactFlow();
  const { setNodes, setEdges, screenToFlowPosition } = reactFlowInstance;
  const [showConnectionDropdown, setShowConnectionDropdown] = useState(false);
  const [dropdownPosition, setDropdownPosition] = useState<{ x: number; y: number } | null>(null);
  const [currentGhostNode, setCurrentGhostNode] = useState<FlowNode | null>(null);
  const [sourceNodeId, setSourceNodeId] = useState<string | null>(null);
  const [sourceHandle, setSourceHandle] = useState<string | null>(null);

  const cleanupGhostNode = useCallback(
    (ghostNode: FlowNode | null) => {
      if (ghostNode) {
        setNodes(nodes => nodes.filter(node => node.id !== ghostNode.id));
        setEdges(edges => edges.filter(edge => edge.target !== ghostNode.id));
      }
    },
    [setNodes, setEdges],
  );

  const resetDropdownState = useCallback(() => {
    setShowConnectionDropdown(false);
    setDropdownPosition(null);
    setCurrentGhostNode(null);
    setSourceNodeId(null);
    setSourceHandle(null);
  }, []);

  const onConnectEnd = useCallback(
    (event: MouseEvent | TouchEvent, connectionState: IncompleteConnectionState) => {
      if (connectionState.isValid || connectionState.fromHandle?.type === 'target' || disabled) {
        return;
      }

      const fromNodeId = connectionState.fromNode?.id;
      if (!fromNodeId) return;
      const { clientX, clientY } = 'changedTouches' in event ? event.changedTouches[0]! : event;

      const id = `ghost-${Date.now()}`;
      const newNode: FlowNode = {
        id,
        type: 'ghost',
        position: screenToFlowPosition({ x: clientX, y: clientY }),
        data: {},
        draggable: false,
        selectable: false,
        deletable: false,
      };

      const newEdge: Edge = {
        id: `${fromNodeId}->${id}`,
        source: fromNodeId,
        sourceHandle: connectionState.fromHandle?.id ?? null,
        target: id,
        reconnectable: 'target',
        type: 'custom',
      };

      setNodes(nodes => nodes.concat(newNode));
      setEdges(edges => addEdge(newEdge, edges));

      setCurrentGhostNode(newNode);
      setSourceNodeId(fromNodeId);
      setSourceHandle(connectionState.fromHandle?.id ?? null);
      setDropdownPosition({ x: clientX, y: clientY });
      setShowConnectionDropdown(true);
    },
    [disabled, screenToFlowPosition, setEdges, setNodes],
  );

  const onReconnect = useCallback(
    (oldEdge: FlowEdge, newConnection: Connection) => {
      setEdges(edges => reconnectEdge(oldEdge, newConnection, edges));
    },
    [setEdges],
  );

  const onReconnectEnd = useCallback(
    (_event: unknown, oldEdge: FlowEdge, handleType: 'source' | 'target') => {
      if (handleType === 'source') {
        setNodes(nodes =>
          nodes.filter(node => {
            const isGhost = node.type === 'ghost';
            const isTarget = node.id === oldEdge.target;
            return !(isGhost && isTarget);
          }),
        );
        setEdges(edges => edges.filter(edge => edge.id !== oldEdge.id));
      }
    },
    [setNodes, setEdges],
  );

  const handleDropdownClose = useCallback(() => {
    cleanupGhostNode(currentGhostNode);
    resetDropdownState();
  }, [currentGhostNode, cleanupGhostNode, resetDropdownState]);

  const handleNodeSelect = useCallback(
    (selectedNode: { id: string }) => {
      if (!currentGhostNode || !sourceNodeId) return;

      cleanupGhostNode(currentGhostNode);

      const connection: Connection = { source: sourceNodeId, target: selectedNode.id, sourceHandle, targetHandle: null };

      onConnect(connection);
      resetDropdownState();
    },
    [currentGhostNode, sourceNodeId, sourceHandle, onConnect, cleanupGhostNode, resetDropdownState],
  );

  const handleNodeCreate = useCallback(
    (nodeType: string) => {
      if (!currentGhostNode || !sourceNodeId) return;

      const newNode = onNodeCreateAtPosition(nodeType, currentGhostNode.position);

      cleanupGhostNode(currentGhostNode);
      if (!newNode) {
        resetDropdownState();
        return;
      }

      const connection: Connection = { source: sourceNodeId, target: newNode.id, sourceHandle, targetHandle: null };

      // Use a macrotask to ensure React has completed the render cycle before onConnect runs.
      setTimeout(() => {
        onConnect(connection);
      }, 0);

      resetDropdownState();
    },
    [currentGhostNode, sourceNodeId, sourceHandle, onNodeCreateAtPosition, onConnect, cleanupGhostNode, resetDropdownState],
  );

  const availableTargets = useMemo((): readonly FlowNode[] => {
    const flowNodes = reactFlowInstance.getNodes() as FlowNode[];
    const sourceNodeType = flowNodes.find(node => node.id === sourceNodeId)?.type;
    if (!sourceNodeId || !flowNodes) return [];

    const sourceNode = flowNodes.find(node => node.id === sourceNodeId);
    const sourceYamlNode = yamlJsonObjectRef.current?.nodes?.find(node => node.id === sourceNodeId);

    if (!sourceNode) return [];

    const allEdges = reactFlowInstance.getEdges();
    const sourceFlags = FlowEditorHelpers.getNodeTypeFlags(sourceNodeId, sourceHandle, sourceYamlNode);

    const connectedTargets = allEdges.filter(edge => edge.source === sourceNodeId).map(edge => edge.target);

    return flowNodes
      .filter(node => {
        if (node.id === sourceNodeId || node.type === 'ghost') return false;
        if (sourceNodeType === PipelineNodeTypes.Hitl && sourceHandle?.includes('edit') && node.type === PipelineNodeTypes.End) {
          return false;
        }
        if (connectedTargets.includes(node.id)) return false;

        const targetFlags = FlowEditorHelpers.getTargetNodeTypeFlags(node);
        return FlowEditorHelpers.canConnectToTarget(sourceFlags, targetFlags, sourceYamlNode);
      })
      .sort((a, b) => {
        if (a.id.toLowerCase() === 'end') return 1;
        if (b.id.toLowerCase() === 'end') return -1;
        return a.id.localeCompare(b.id);
      });
    // baseline's own deps array (useIncompleteEdge.hooks.js:194-195): `[sourceNodeId, sourceHandle]` only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceNodeId, sourceHandle]);

  const availableNodeTypes = useMemo((): readonly string[] => {
    const flowNodes = reactFlowInstance.getNodes();
    if (!sourceNodeId || !flowNodes) return [];

    const sourceNode = flowNodes.find(node => node.id === sourceNodeId);
    if (!sourceNode) return [];

    const sourceYamlNode = yamlJsonObjectRef.current?.nodes?.find(node => node.id === sourceNodeId);
    const sourceFlags = FlowEditorHelpers.getNodeTypeFlags(sourceNodeId, sourceHandle, sourceYamlNode);

    const allNodeTypes = FlowEditorHelpers.getAllowedNodeTypes();

    return allNodeTypes.filter(nodeType => FlowEditorHelpers.canCreateNodeType(nodeType, sourceFlags));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceNodeId, sourceHandle]);

  return {
    onConnectEnd,
    onReconnect,
    onReconnectEnd,
    showConnectionDropdown,
    dropdownPosition,
    currentGhostNode,
    handleDropdownClose,
    handleNodeSelect,
    handleNodeCreate,
    availableTargets,
    availableNodeTypes,
  };
}
