import type { ReactNode } from 'react';
import { act, renderHook } from '@testing-library/react';
import { ReactFlowProvider, useReactFlow, type ReactFlowInstance } from '@xyflow/react';
import { describe, expect, it, vi } from 'vitest';

import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode, YamlPipelineDocumentRef } from '../reactFlowTypes';
import { useIncompleteEdge, type UseIncompleteEdgeArgs } from './useIncompleteEdge';

function makeRef(doc: YamlPipelineDocument): YamlPipelineDocumentRef {
  return { current: doc };
}

function makeConnectEndEvent(clientX: number, clientY: number): MouseEvent {
  return { clientX, clientY } as unknown as MouseEvent;
}

/** Renders the hook inside a real `ReactFlowProvider`, seeded with the given nodes/edges, and also exposes the live `useReactFlow()` instance for assertions. */
function renderIncompleteEdge(
  args: Partial<UseIncompleteEdgeArgs> & { readonly yamlJsonObjectRef: YamlPipelineDocumentRef },
  initialNodes: FlowNode[] = [],
  initialEdges: FlowEdge[] = [],
) {
  let flow: ReactFlowInstance<FlowNode, FlowEdge> | undefined;
  const fullArgs: UseIncompleteEdgeArgs = {
    onConnect: vi.fn(),
    onNodeCreateAtPosition: vi.fn(() => ({ id: 'new-node' })),
    ...args,
  };

  const { result } = renderHook(
    () => {
      flow = useReactFlow<FlowNode, FlowEdge>();
      return useIncompleteEdge(fullArgs);
    },
    {
      // `initialNodes`/`initialEdges` only seed the store's first render -- `useReactFlow()`'s
      // own `setNodes`/`setEdges` are queued through `BatchProvider` and only actually commit
      // back into the store when `hasDefaultNodes`/`hasDefaultEdges` is true, i.e. when the
      // *uncontrolled* `defaultNodes`/`defaultEdges` props are used instead (verified directly
      // against `@xyflow/react`'s own `BatchProvider`/`getInitialState` source: without this,
      // every `setNodes`/`setEdges` call this hook makes is silently dropped in a test harness
      // that never mounts an actual `<ReactFlow onNodesChange>` to consume the diff).
      wrapper: ({ children }: { children: ReactNode }) => (
        <ReactFlowProvider
          defaultNodes={initialNodes}
          defaultEdges={initialEdges}
        >
          {children}
        </ReactFlowProvider>
      ),
    },
  );

  return { result, args: fullArgs, getFlow: () => flow! };
}

describe('useIncompleteEdge: onConnectEnd', () => {
  it('is a no-op when the connection is already valid', () => {
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) });

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: true, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });

    expect(result.current.showConnectionDropdown).toBe(false);
    expect(getFlow().getNodes()).toEqual([]);
  });

  it('is a no-op when dragging from a target handle', () => {
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) });

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'target', id: null }, fromNode: { id: 'A' } });
    });

    expect(result.current.showConnectionDropdown).toBe(false);
    expect(getFlow().getNodes()).toEqual([]);
  });

  it('is a no-op when disabled', () => {
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }), disabled: true });

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });

    expect(result.current.showConnectionDropdown).toBe(false);
    expect(getFlow().getNodes()).toEqual([]);
  });

  it('is a no-op when there is no source node', () => {
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) });

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: null });
    });

    expect(result.current.showConnectionDropdown).toBe(false);
    expect(getFlow().getNodes()).toEqual([]);
  });

  it('spawns a ghost node + edge and opens the connection dropdown on a real incomplete drag', () => {
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }) }, [sourceNode]);

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(100, 200), {
        isValid: false,
        fromHandle: { type: 'source', id: 'out-1' },
        fromNode: { id: 'A' },
      });
    });

    expect(result.current.showConnectionDropdown).toBe(true);
    expect(result.current.dropdownPosition).toEqual({ x: 100, y: 200 });
    expect(result.current.currentGhostNode?.id).toMatch(/^ghost-\d+$/);
    expect(result.current.currentGhostNode?.type).toBe('ghost');

    const nodes = getFlow().getNodes();
    expect(nodes).toHaveLength(2);
    expect(nodes.find(n => n.type === 'ghost')).toBeDefined();
    const edges = getFlow().getEdges();
    expect(edges).toHaveLength(1);
    expect(edges[0]).toMatchObject({ source: 'A', sourceHandle: 'out-1' });
  });

  it('supports touch events by reading changedTouches instead of clientX/clientY directly', () => {
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }) }, [sourceNode]);
    const touchEvent = { changedTouches: [{ clientX: 42, clientY: 84 }] } as unknown as TouchEvent;

    act(() => {
      result.current.onConnectEnd(touchEvent, { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });

    expect(result.current.dropdownPosition).toEqual({ x: 42, y: 84 });
  });
});

describe('useIncompleteEdge: onReconnect / onReconnectEnd', () => {
  it('onReconnect re-targets the edge via reconnectEdge', () => {
    const edge: FlowEdge = { id: 'e1', source: 'A', target: 'B' };
    const { result, getFlow } = renderIncompleteEdge(
      { yamlJsonObjectRef: makeRef({ nodes: [] }) },
      [
        { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} },
        { id: 'B', type: 'agent', position: { x: 0, y: 0 }, data: {} },
        { id: 'C', type: 'agent', position: { x: 0, y: 0 }, data: {} },
      ],
      [edge],
    );

    act(() => {
      result.current.onReconnect(edge, { source: 'A', target: 'C', sourceHandle: null, targetHandle: null });
    });

    const edges = getFlow().getEdges();
    expect(edges).toHaveLength(1);
    expect(edges[0]).toMatchObject({ source: 'A', target: 'C' });
  });

  it('onReconnectEnd removes the reconnected-away ghost target node + edge when releasing from the source handle', () => {
    const ghost: FlowNode = { id: 'ghost-1', type: 'ghost', position: { x: 0, y: 0 }, data: {} };
    const other: FlowNode = { id: 'B', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const edge: FlowEdge = { id: 'e1', source: 'A', target: 'ghost-1' };
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) }, [ghost, other], [edge]);

    act(() => {
      result.current.onReconnectEnd(undefined, edge, 'source');
    });

    expect(getFlow().getNodes().map(n => n.id)).toEqual(['B']);
    expect(getFlow().getEdges()).toEqual([]);
  });

  it('onReconnectEnd leaves nodes/edges untouched when releasing from the target handle', () => {
    const ghost: FlowNode = { id: 'ghost-1', type: 'ghost', position: { x: 0, y: 0 }, data: {} };
    const edge: FlowEdge = { id: 'e1', source: 'A', target: 'ghost-1' };
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) }, [ghost], [edge]);

    act(() => {
      result.current.onReconnectEnd(undefined, edge, 'target');
    });

    expect(getFlow().getNodes().map(n => n.id)).toEqual(['ghost-1']);
    expect(getFlow().getEdges()).toHaveLength(1);
  });
});

describe('useIncompleteEdge: dropdown selection / creation', () => {
  it('handleDropdownClose cleans up the ghost node/edge and resets dropdown state', () => {
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const { result, getFlow } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }) }, [sourceNode]);

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });
    act(() => {
      result.current.handleDropdownClose();
    });

    expect(result.current.showConnectionDropdown).toBe(false);
    expect(result.current.currentGhostNode).toBeNull();
    expect(getFlow().getNodes()).toEqual([sourceNode]);
    expect(getFlow().getEdges()).toEqual([]);
  });

  it('handleNodeSelect cleans up the ghost, calls onConnect with the resolved connection, and resets dropdown state', () => {
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const onConnect = vi.fn();
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }), onConnect }, [sourceNode]);

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: 'out-1' }, fromNode: { id: 'A' } });
    });
    act(() => {
      result.current.handleNodeSelect({ id: 'ExistingTarget' });
    });

    expect(onConnect).toHaveBeenCalledWith({ source: 'A', target: 'ExistingTarget', sourceHandle: 'out-1', targetHandle: null });
    expect(result.current.showConnectionDropdown).toBe(false);
  });

  it('handleNodeSelect is a no-op when there is no pending ghost node', () => {
    const onConnect = vi.fn();
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }), onConnect });

    act(() => {
      result.current.handleNodeSelect({ id: 'X' });
    });

    expect(onConnect).not.toHaveBeenCalled();
  });

  it('handleNodeCreate creates the node at the ghost position and connects to it (async via setTimeout)', () => {
    vi.useFakeTimers();
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const onConnect = vi.fn();
    const onNodeCreateAtPosition = vi.fn(() => ({ id: 'NewNode' }));
    const { result } = renderIncompleteEdge(
      { yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }), onConnect, onNodeCreateAtPosition },
      [sourceNode],
    );

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: 'out-1' }, fromNode: { id: 'A' } });
    });
    const ghostPosition = result.current.currentGhostNode?.position;
    act(() => {
      result.current.handleNodeCreate('agent');
    });

    expect(onNodeCreateAtPosition).toHaveBeenCalledWith('agent', ghostPosition);
    expect(onConnect).not.toHaveBeenCalled();
    expect(result.current.showConnectionDropdown).toBe(false);

    act(() => {
      vi.runAllTimers();
    });

    expect(onConnect).toHaveBeenCalledWith({ source: 'A', target: 'NewNode', sourceHandle: 'out-1', targetHandle: null });
    vi.useRealTimers();
  });

  it('handleNodeCreate connects nothing and resets the dropdown when the node was not created (refused document write)', () => {
    vi.useFakeTimers();
    try {
      const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
      const onConnect = vi.fn();
      const onNodeCreateAtPosition = vi.fn(() => undefined);
      const { result, getFlow } = renderIncompleteEdge(
        { yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }), onConnect, onNodeCreateAtPosition },
        [sourceNode],
      );

      act(() => {
        result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: 'out-1' }, fromNode: { id: 'A' } });
      });
      act(() => {
        result.current.handleNodeCreate('agent');
      });
      act(() => {
        vi.runAllTimers();
      });

      expect(onNodeCreateAtPosition).toHaveBeenCalledTimes(1);
      expect(onConnect).not.toHaveBeenCalled();
      expect(result.current.showConnectionDropdown).toBe(false);
      expect(result.current.currentGhostNode).toBeNull();
      expect(getFlow().getNodes()).toEqual([sourceNode]);
      expect(getFlow().getEdges()).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('handleNodeCreate is a no-op when there is no pending ghost node', () => {
    const onNodeCreateAtPosition = vi.fn();
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }), onNodeCreateAtPosition });

    act(() => {
      result.current.handleNodeCreate('agent');
    });

    expect(onNodeCreateAtPosition).not.toHaveBeenCalled();
  });
});

describe('useIncompleteEdge: availableTargets / availableNodeTypes', () => {
  it('excludes the source itself, ghost nodes, and already-connected targets; sorts End last', () => {
    const nodes: FlowNode[] = [
      { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} },
      { id: 'B', type: 'agent', position: { x: 0, y: 0 }, data: {} },
      { id: 'D', type: 'agent', position: { x: 0, y: 0 }, data: {} },
      { id: 'ghost-1', type: 'ghost', position: { x: 0, y: 0 }, data: {} },
      { id: 'END', type: 'END', position: { x: 0, y: 0 }, data: {} },
    ];
    const edges: FlowEdge[] = [{ id: 'e1', source: 'A', target: 'B' }];
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }) }, nodes, edges);

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });

    // B is already connected (edge A->B), the ghost node and A itself are excluded; D and END remain, END sorted last.
    expect(result.current.availableTargets.map(n => n.id)).toEqual(['D', 'END']);
  });

  it('availableNodeTypes returns the allowed node types for a plain (non-special) source', () => {
    const sourceNode: FlowNode = { id: 'A', type: 'agent', position: { x: 0, y: 0 }, data: {} };
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [{ id: 'A' }] }) }, [sourceNode]);

    act(() => {
      result.current.onConnectEnd(makeConnectEndEvent(10, 10), { isValid: false, fromHandle: { type: 'source', id: null }, fromNode: { id: 'A' } });
    });

    expect(result.current.availableNodeTypes).toContain('agent');
    expect(result.current.availableNodeTypes).not.toContain('END');
    expect(result.current.availableNodeTypes).not.toContain('ghost');
    // Condition/Decision node creation is only forbidden from a special source -- allowed here.
    expect(result.current.availableNodeTypes).toContain('condition');
  });

  it('availableTargets/availableNodeTypes are empty before any source has been selected', () => {
    const { result } = renderIncompleteEdge({ yamlJsonObjectRef: makeRef({ nodes: [] }) });
    expect(result.current.availableTargets).toEqual([]);
    expect(result.current.availableNodeTypes).toEqual([]);
  });
});
