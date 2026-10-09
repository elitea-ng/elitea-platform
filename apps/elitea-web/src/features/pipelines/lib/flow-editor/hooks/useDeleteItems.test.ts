import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode, SetFlowEdges, SetFlowNodes } from '../reactFlowTypes';
import { useDeleteItems } from './useDeleteItems';

function flowNode(id: string, type = 'agent', selected = false): FlowNode {
  return { id, type, position: { x: 0, y: 0 }, data: {}, selected };
}

function makeStatefulSetFlowNodes(initial: readonly FlowNode[]) {
  let nodes: readonly FlowNode[] = initial;
  const setFlowNodes = vi.fn<SetFlowNodes>(updater => {
    nodes = typeof updater === 'function' ? updater(nodes as FlowNode[]) : updater;
  });
  return { setFlowNodes, getNodes: () => nodes };
}

function makeStatefulSetFlowEdges(initial: readonly FlowEdge[]) {
  let edges: readonly FlowEdge[] = initial;
  const setFlowEdges = vi.fn<SetFlowEdges>(updater => {
    edges = typeof updater === 'function' ? updater(edges as FlowEdge[]) : updater;
  });
  return { setFlowEdges, getEdges: () => edges };
}

function pressDelete(): void {
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Delete', code: 'Delete', bubbles: true }));
}

describe('useDeleteItems: onBeforeDelete', () => {
  it('opens the confirm dialog with the combined nodes+edges copy and records the selection; always returns false', () => {
    const setYamlJsonObject = vi.fn();
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const nodeA = flowNode('A', 'agent', true);
    const edgeE: FlowEdge = { id: 'e1', source: 'A', target: 'B', selected: true };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }, { id: 'B' }] },
        flowNodes: [nodeA],
        flowEdges: [edgeE],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    let returnValue: boolean | undefined;
    act(() => {
      returnValue = result.current.onBeforeDelete({ nodes: [nodeA], edges: [edgeE] });
    });

    expect(returnValue).toBe(false);
    expect(result.current.showDeleteConfirmDlg).toBe(true);
    expect(result.current.nodesToDelete).toEqual([nodeA]);
    expect(result.current.confirmContent).toBe('Are you sure to delete the selected nodes and edges ');
  });

  it('is a no-op when disabled', () => {
    const setYamlJsonObject = vi.fn();
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const nodeA = flowNode('A', 'agent', true);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }] },
        flowNodes: [nodeA],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
        disabled: true,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [nodeA], edges: [] });
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
    expect(result.current.nodesToDelete).toEqual([]);
  });

  it('is a no-op when neither nodes nor edges are supplied', () => {
    const setYamlJsonObject = vi.fn();
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [] },
        flowNodes: [],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [], edges: [] });
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
  });
});

describe('useDeleteItems: handleDeleteNode', () => {
  it('finds the node and its connected edges, and opens the single-node confirm dialog', () => {
    const setYamlJsonObject = vi.fn();
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const nodeA = flowNode('A');
    const nodeB = flowNode('B');
    const edgeAB: FlowEdge = { id: 'e1', source: 'A', target: 'B' };
    const edgeUnrelated: FlowEdge = { id: 'e2', source: 'X', target: 'Y' };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }, { id: 'B' }] },
        flowNodes: [nodeA, nodeB],
        flowEdges: [edgeAB, edgeUnrelated],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.handleDeleteNode('A');
    });

    expect(result.current.showDeleteConfirmDlg).toBe(true);
    expect(result.current.nodesToDelete).toEqual([nodeA]);
    expect(result.current.confirmContent).toBe('Are you sure to delete this node ');
  });
});

describe('useDeleteItems: onConfirmDelete / onCancelDelete', () => {
  it('deletes a normal node + its outgoing edge: updates yaml, drops the node and edge from the flow, closes the dialog', () => {
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A');
    const nodeB = flowNode('B');
    const edgeAB: FlowEdge = { id: 'e1', source: 'A', target: 'B' };
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([nodeA, nodeB]);
    const { setFlowEdges, getEdges } = makeStatefulSetFlowEdges([edgeAB]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'A', transition: 'B' }, { id: 'B' }], entry_point: 'A' };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [nodeA, nodeB],
        flowEdges: [edgeAB],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [nodeA], edges: [edgeAB] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
    expect(result.current.nodesToDelete).toEqual([]);
    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(written.nodes?.map(n => n.id)).toEqual(['B']);
    expect(written.entry_point).toBeUndefined();
    expect(getNodes().map(n => n.id)).toEqual(['B']);
    expect(getEdges()).toEqual([]);
  });

  it('keeps the canvas when the document write is refused (the document still holds the node and edge)', () => {
    const setYamlJsonObject = vi.fn(() => false);
    const nodeA = flowNode('A');
    const nodeB = flowNode('B');
    const edgeAB: FlowEdge = { id: 'e1', source: 'A', target: 'B' };
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([nodeA, nodeB]);
    const { setFlowEdges, getEdges } = makeStatefulSetFlowEdges([edgeAB]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A', transition: 'B' }, { id: 'B' }], entry_point: 'A' },
        flowNodes: [nodeA, nodeB],
        flowEdges: [edgeAB],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [nodeA], edges: [edgeAB] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    expect(getNodes().map(n => n.id)).toEqual(['A', 'B']);
    expect(getEdges()).toEqual([edgeAB]);
    expect(result.current.showDeleteConfirmDlg).toBe(false);
    expect(result.current.nodesToDelete).toEqual([]);
  });

  it('filters out an End-type node from the yaml/flow-node deletion pass (baseline never deletes the synthetic End node)', () => {
    const setYamlJsonObject = vi.fn();
    const endNode = flowNode('END', 'END');
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([endNode]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'A' }] };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [endNode],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [endNode], edges: [] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    // The End node was excluded from `filteredNodes`, so it's never removed from the flow-node list either.
    expect(getNodes()).toEqual([endNode]);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(written.nodes?.map(n => n.id)).toEqual(['A']);
  });

  it('dispatches a condition-node deletion to clear the owner node condition instead of removing it from nodes[]', () => {
    const setYamlJsonObject = vi.fn();
    const conditionNode = flowNode('A~~~ConditionNode', 'condition');
    const { setFlowNodes } = makeStatefulSetFlowNodes([conditionNode]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'A', condition: { default_output: 'B' } }] };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [conditionNode],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [conditionNode], edges: [] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    // The owner node ('A') is still present -- only its condition was cleared.
    expect(written.nodes?.map(n => n.id)).toEqual(['A']);
    expect(written.nodes?.[0]?.condition).toBeUndefined();
  });

  it('dispatches a legacy-decision-node deletion to clear the owner node decision instead of removing it from nodes[]', () => {
    const setYamlJsonObject = vi.fn();
    const decisionNode = flowNode('A~~~DecisionNode', 'decision');
    const { setFlowNodes } = makeStatefulSetFlowNodes([decisionNode]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'A', decision: { default_output: 'B' } }] };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [decisionNode],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [decisionNode], edges: [] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(written.nodes?.map(n => n.id)).toEqual(['A']);
    expect(written.nodes?.[0]?.decision).toBeUndefined();
  });

  it('onCancelDelete closes the dialog and clears the pending selection without touching the yaml/flow state', () => {
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A');
    const { setFlowNodes } = makeStatefulSetFlowNodes([nodeA]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }] },
        flowNodes: [nodeA],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [nodeA], edges: [] });
    });
    act(() => {
      result.current.onCancelDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
    expect(result.current.nodesToDelete).toEqual([]);
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });
});

describe('useDeleteItems: Delete-key trigger', () => {
  it('opens the confirm dialog for the current selection when Delete is pressed and the canvas is visible', () => {
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A', 'agent', true);
    const { setFlowNodes } = makeStatefulSetFlowNodes([nodeA]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }] },
        flowNodes: [nodeA],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(true);
    expect(result.current.nodesToDelete).toEqual([nodeA]);
    expect(result.current.confirmContent).toBe('Are you sure to delete the selected node ');
  });

  it('does nothing on Delete when the canvas display is "none"', () => {
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A', 'agent', true);
    const { setFlowNodes } = makeStatefulSetFlowNodes([nodeA]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'none',
        yamlJsonObject: { nodes: [{ id: 'A' }] },
        flowNodes: [nodeA],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
  });

  it('opens the confirm dialog for a selected edge (not just nodes) when Delete is pressed', () => {
    const setYamlJsonObject = vi.fn();
    const selectedEdge: FlowEdge = { id: 'e1', source: 'A', target: 'B', selected: true };
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([selectedEdge]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [] },
        flowNodes: [],
        flowEdges: [selectedEdge],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(true);
    expect(result.current.confirmContent).toBe('Are you sure to delete the selected edge ');
  });

  it('queues the doomed node’s connected edges too, so the source transition is repaired to END rather than blanked', () => {
    /*
     * The measured defect (see `useDeleteKeyTrigger`'s own doc comment): with
     * only the SELECTED edges queued, deleting `B` left `A.transition: ''` —
     * a target that is neither `END` nor a `valid_graph_id`, which the
     * editor's own admission gate refuses, so Save went disabled and the
     * pipeline could not be stored at all. Backspace and the node card's
     * Delete both produced `END` on the identical graph.
     *
     * `edgeAB` is deliberately NOT `selected`: selecting the node is all a
     * user does.
     */
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A', 'llm');
    const nodeB = flowNode('B', 'llm', true);
    const edgeAB: FlowEdge = { id: 'e1', source: 'A', target: 'B' };
    const { setFlowNodes } = makeStatefulSetFlowNodes([nodeA, nodeB]);
    const { setFlowEdges, getEdges } = makeStatefulSetFlowEdges([edgeAB]);
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'A', transition: 'B' }, { id: 'B', transition: 'END' }],
      entry_point: 'A',
    };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [nodeA, nodeB],
        flowEdges: [edgeAB],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });
    // The copy still describes what the USER selected — one node, no edges.
    expect(result.current.confirmContent).toBe('Are you sure to delete the selected node ');

    act(() => {
      result.current.onConfirmDelete();
    });

    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(written.nodes?.map(node => node.id)).toEqual(['A']);
    expect(written.nodes?.[0]?.transition).toBe('END');
    expect(getEdges()).toEqual([]);
  });

  it('does nothing on Delete when nothing is selected', () => {
    const setYamlJsonObject = vi.fn();
    const nodeA = flowNode('A', 'agent', false);
    const { setFlowNodes } = makeStatefulSetFlowNodes([nodeA]);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject: { nodes: [{ id: 'A' }] },
        flowNodes: [nodeA],
        flowEdges: [],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });

    expect(result.current.showDeleteConfirmDlg).toBe(false);
  });
});

describe('useDeleteItems: the End node is not a doomed node', () => {
  it('selecting End and pressing Delete leaves every END target intact in the stored document', () => {
    /*
     * The End node is a normal, selectable React Flow node
     * (`parsePipelineTraversal.helpers.ts:127-132` pushes `{id:'END'}` with no
     * `selectable:false`), and `onDelete` filters it out of node deletion. When
     * `doomedNodeIds` was built from the RAW selection, selecting End and
     * pressing Delete queued EVERY edge terminating at END. `onDelete` removes
     * no YAML node (End is filtered out), but the queued edges are still run
     * through `processEdgeDeletion` and then dropped from `flowEdges` — so both
     * arrows into END vanish from the canvas while the document still routes
     * there, and canvas and document disagree until a reload.
     *
     * The assertion is on the surviving EDGES, not on the document: the
     * repair-to-END fix in `deletionOperations.helpers.ts` means the stored
     * targets read `END` either way, so a document-only assertion cannot
     * discriminate (verified by reverting this fix — it stayed green).
     */
    const setYamlJsonObject = vi.fn();
    const endNode = flowNode('END', 'END', true);
    const router = flowNode('Router_1', 'router');
    const llm = flowNode('LLM_1', 'llm');
    const edgeRouterEnd: FlowEdge = { id: 'e1', source: 'Router_1', target: 'END', sourceHandle: 'routerNode_default_output' };
    const edgeLlmEnd: FlowEdge = { id: 'e2', source: 'LLM_1', target: 'END' };
    const { setFlowNodes } = makeStatefulSetFlowNodes([router, llm, endNode]);
    const { setFlowEdges, getEdges } = makeStatefulSetFlowEdges([edgeRouterEnd, edgeLlmEnd]);
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [
        { id: 'Router_1', type: 'router', routes: ['LLM_1'], default_output: 'END' },
        { id: 'LLM_1', type: 'llm', transition: 'END' },
      ],
      entry_point: 'Router_1',
    };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [router, llm, endNode],
        flowEdges: [edgeRouterEnd, edgeLlmEnd],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      pressDelete();
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    // Neither arrow into END may be removed: the user deleted nothing.
    expect(getEdges().map(edge => edge.id)).toEqual(['e1', 'e2']);
    const stored = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    expect(stored.nodes?.find(n => n.id === 'Router_1')?.default_output).toBe('END');
    expect(stored.nodes?.find(n => n.id === 'LLM_1')?.transition).toBe('END');
  });
});

describe('useDeleteItems: the stored document does not depend on React evaluating an updater eagerly', () => {
  it('publishes the edge repairs even when setFlowNodes defers the updater', () => {
    /*
     * `setFlowNodes` is React Flow's `useNodesState` setter — a plain React
     * `useState` dispatch. React only evaluates a function updater at dispatch
     * time via the eager-state fast path, which is SKIPPED whenever the owning
     * fiber already has pending lanes; otherwise the updater runs during the
     * later render pass. Computing the repairs inside that updater meant
     * `setYamlJsonObject` (called on the next line) stored a document with none
     * of them. This mock never invokes the updater, which is exactly the
     * deferred ordering; the assertion is on the STORED document.
     */
    const setYamlJsonObject = vi.fn();
    // Deliberately does NOT call the updater — models the deferred ordering.
    const setFlowNodes = vi.fn<SetFlowNodes>(() => undefined);
    const { setFlowEdges } = makeStatefulSetFlowEdges([]);
    const nodeA = flowNode('A', 'llm');
    const nodeB = flowNode('B', 'llm');
    const edgeAB: FlowEdge = { id: 'e1', source: 'A', target: 'B', selected: true };
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'A', type: 'llm', transition: 'B' }, { id: 'B', type: 'llm', transition: 'END' }],
      entry_point: 'A',
    };

    const { result } = renderHook(() =>
      useDeleteItems({
        display: 'flex',
        yamlJsonObject,
        flowNodes: [nodeA, nodeB],
        flowEdges: [edgeAB],
        setYamlJsonObject,
        setFlowNodes,
        setFlowEdges,
      }),
    );

    act(() => {
      result.current.onBeforeDelete({ nodes: [], edges: [edgeAB] });
    });
    act(() => {
      result.current.onConfirmDelete();
    });

    const stored = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    expect(stored.nodes?.find(n => n.id === 'A')?.transition).toBe('END');
  });
});
