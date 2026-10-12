import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { PipelineNodeTypes } from '../lib/flow-editor/constants/flowEditor.constants';
import * as NewNodeRevealHelpers from '../lib/flow-editor/helpers/newNodeReveal.helpers';
import type { FlowNode } from '../lib/flow-editor/reactFlowTypes';
import { useFlowEditorNodeOperations } from './useFlowEditorNodeOperations';

function makeNode(id: string, type: string, x = 0, y = 0): FlowNode {
  return { id, type, position: { x, y }, data: {} };
}

/*
 * Unconditional, because the restore used to be the LAST line of each
 * fake-timer test: an assertion that threw skipped it and left fake timers
 * installed for the rest of the file, turning one real failure into a
 * cascade of unrelated timeouts.
 */
afterEach(() => {
  vi.useRealTimers();
});

describe('useFlowEditorNodeOperations', () => {
  it('onNodeCreateAtPosition creates a YAML node, sets it as flow node, and marks entry_point on the first non-Condition node', () => {
    const setFlowNodes = vi.fn();
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [],
        setFlowNodes,
        setFlowEdges: vi.fn(),
        setYamlJsonObject,
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    const created = result.current.onNodeCreateAtPosition(PipelineNodeTypes.Agent, { x: 10, y: 20 });

    expect(created?.type).toBe(PipelineNodeTypes.Agent);
    expect(created?.position).toEqual({ x: 10, y: 20 });
    expect(created?.selected).toBe(true);
    const setDoc = setYamlJsonObject.mock.calls[0]?.[0] as { entry_point?: string; nodes?: readonly { id: string; type: string }[] };
    expect(setDoc.entry_point).toBe(created?.id);
    expect(setDoc.nodes?.some(node => node.id === created?.id && node.type === PipelineNodeTypes.Agent)).toBe(true);
    expect(setFlowNodes).toHaveBeenCalled();
  });

  it('onNodeCreateAtPosition does NOT add a Condition-type node into yamlJsonObject.nodes (baseline: conditions live inline, not as YAML node entries)', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        setYamlJsonObject,
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    const created = result.current.onNodeCreateAtPosition(PipelineNodeTypes.Condition, { x: 0, y: 0 });

    expect(created?.data?.['label']).toBe('Condition');
    expect(created?.data?.['condition']).toEqual({ condition_input: [], condition_definition: '', conditional_outputs: [], default_output: '' });
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('does not overwrite an already-set entry_point', () => {
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        setYamlJsonObject,
        yamlJsonObjectRef: { current: { entry_point: 'Existing 1' } },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    result.current.onNodeCreateAtPosition(PipelineNodeTypes.Agent, { x: 0, y: 0 });

    expect(setYamlJsonObject).toHaveBeenCalledWith(expect.objectContaining({ entry_point: 'Existing 1' }));
  });

  it('onAddNode centres the new node in the current viewport and avoids overlapping existing nodes', () => {
    const setYamlJsonObject = vi.fn();
    const setFlowNodes = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [],
        setFlowNodes,
        setFlowEdges: vi.fn(),
        setYamlJsonObject,
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    const created = result.current.onAddNode(PipelineNodeTypes.Agent);

    // (800/2 - 230 - 0)/1 = 170, (600/2 - 200 - 0)/1 = 100 — FlowEditorHelpers.calculatePositionForNewNode's own formula, no existing nodes to dodge.
    expect(created?.position).toEqual({ x: 170, y: 100 });
  });

  it('onAddNode pans the viewport onto a card that the End-node stacking rule pushed below the fold', () => {
    // A pipeline canvas is never empty: an End node sits at the viewport
    // centre, so the FIRST added card is always stacked below it (End is 60
    // tall + a 40 gap => y=200) and most of its 460px body falls under the
    // 600px fold. Nothing used to bring it back into view.
    vi.useFakeTimers();
    const setCenter = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [makeNode('END', PipelineNodeTypes.End, 170, 100)],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        setYamlJsonObject: vi.fn(),
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter,
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    const created = result.current.onAddNode(PipelineNodeTypes.Agent);
    act(() => {
      vi.advanceTimersByTime(NewNodeRevealHelpers.NEW_NODE_REVEAL_DELAY_MS);
    });

    expect(created?.position).toEqual({ x: 170, y: 200 });
    // Centre of the new card (460 wide x 460 tall), at the CURRENT zoom —
    // not a fitView, which would rescale a canvas the user may have zoomed.
    expect(setCenter).toHaveBeenCalledWith(400, 430, { zoom: 1 });
  });

  it('onAddNode measures the LIVE pane at reveal time, not the size it had during the add', () => {
    // Adding an inadmissible node opens the graph-admission panel, which
    // re-flows the canvas: the pane is narrower a moment later than the
    // `editorWidth`/`editorHeight` the placement formula saw. Reusing those
    // stale numbers zooms the card wider than the pane it lands in.
    vi.useFakeTimers();
    const setCenter = vi.fn();
    const editorRef = { current: { offsetWidth: 480, offsetHeight: 600 } as unknown as HTMLElement };
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [makeNode('END', PipelineNodeTypes.End, 170, 100)],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        setYamlJsonObject: vi.fn(),
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter,
        getZoom: () => 1,
        editorRef,
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    result.current.onAddNode(PipelineNodeTypes.Agent);
    act(() => {
      vi.advanceTimersByTime(NewNodeRevealHelpers.NEW_NODE_REVEAL_DELAY_MS);
    });

    // (480 - 48) / 460 — the shrunken pane's width, not the 800 above.
    expect(setCenter).toHaveBeenCalledWith(400, 430, { zoom: (480 - 2 * NewNodeRevealHelpers.NEW_NODE_REVEAL_MARGIN) / 460 });
  });

  it('onAddNode leaves the viewport alone when the new card is already fully visible', () => {
    vi.useFakeTimers();
    const setCenter = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        setYamlJsonObject: vi.fn(),
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter,
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    result.current.onAddNode(PipelineNodeTypes.Agent);
    act(() => {
      vi.advanceTimersByTime(NewNodeRevealHelpers.NEW_NODE_REVEAL_DELAY_MS);
    });

    expect(setCenter).not.toHaveBeenCalled();
  });

  describe('when the document write is refused (setYamlJsonObject returns false)', () => {
    function renderWith(setYamlJsonObject: () => boolean | void, setFlowNodes = vi.fn(), setCenter = vi.fn()) {
      return renderHook(() =>
        useFlowEditorNodeOperations({
          flowNodes: [],
          setFlowNodes,
          setFlowEdges: vi.fn(),
          setYamlJsonObject,
          yamlJsonObjectRef: { current: {} },
          getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
          setCenter,
          getZoom: () => 1,
          editorRef: { current: null },
          editorWidth: 800,
          editorHeight: 600,
        }),
      );
    }

    it('onNodeCreateAtPosition returns undefined and adds no canvas node', () => {
      const setFlowNodes = vi.fn();
      const { result } = renderWith(() => false, setFlowNodes);

      expect(result.current.onNodeCreateAtPosition(PipelineNodeTypes.Agent, { x: 10, y: 20 })).toBeUndefined();
      expect(setFlowNodes).not.toHaveBeenCalled();
    });

    it('onAddNode returns undefined and never reveals a node that was not created', () => {
      vi.useFakeTimers();
      const setFlowNodes = vi.fn();
      const setCenter = vi.fn();
      // An End node at the centre stacks the new card below the fold, so a created node WOULD be revealed (see the reveal test above).
      const { result } = renderHook(() =>
        useFlowEditorNodeOperations({
          flowNodes: [makeNode('END', PipelineNodeTypes.End, 170, 100)],
          setFlowNodes,
          setFlowEdges: vi.fn(),
          setYamlJsonObject: () => false,
          yamlJsonObjectRef: { current: {} },
          getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
          setCenter,
          getZoom: () => 1,
          editorRef: { current: null },
          editorWidth: 800,
          editorHeight: 600,
        }),
      );

      expect(result.current.onAddNode(PipelineNodeTypes.Agent)).toBeUndefined();
      act(() => {
        vi.advanceTimersByTime(NewNodeRevealHelpers.NEW_NODE_REVEAL_DELAY_MS);
      });

      expect(setFlowNodes).not.toHaveBeenCalled();
      expect(setCenter).not.toHaveBeenCalled();
    });

    it('a setter that reports true (or nothing at all) still adds the node', () => {
      for (const setter of [() => true, () => undefined]) {
        const setFlowNodes = vi.fn();
        const { result } = renderWith(setter, setFlowNodes);

        expect(result.current.onNodeCreateAtPosition(PipelineNodeTypes.Agent, { x: 0, y: 0 })?.type).toBe(PipelineNodeTypes.Agent);
        expect(setFlowNodes).toHaveBeenCalledTimes(1);
      }
    });

    it('a Condition node is canvas-only (no document write), so a refusing setter is never consulted and it is still added', () => {
      const setYamlJsonObject = vi.fn(() => false);
      const setFlowNodes = vi.fn();
      const { result } = renderWith(setYamlJsonObject, setFlowNodes);

      expect(result.current.onNodeCreateAtPosition(PipelineNodeTypes.Condition, { x: 0, y: 0 })).toBeDefined();
      expect(setYamlJsonObject).not.toHaveBeenCalled();
      expect(setFlowNodes).toHaveBeenCalledTimes(1);
    });
  });

  it('calculateLayoutNodes re-parses the YAML into the canvas via setFlowNodes/setFlowEdges updaters', () => {
    const setFlowNodes = vi.fn();
    const setFlowEdges = vi.fn();
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [makeNode('a', PipelineNodeTypes.Agent, 5, 5)],
        setFlowNodes,
        setFlowEdges,
        setYamlJsonObject: vi.fn(),
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    result.current.calculateLayoutNodes({ entry_point: 'a', nodes: [{ id: 'a', type: PipelineNodeTypes.Agent }] }, false, false, true);

    expect(setFlowNodes).toHaveBeenCalledTimes(1);
    expect(setFlowEdges).toHaveBeenCalledTimes(1);
    const updater = setFlowNodes.mock.calls[0]?.[0] as (prev: FlowNode[]) => FlowNode[];
    const next = updater([makeNode('a', PipelineNodeTypes.Agent, 5, 5)]);
    // The baseline parser always appends an implicit End node to a single-node graph — `>= 1` + the real id present, not an exact count.
    expect(next.length).toBeGreaterThanOrEqual(1);
    expect(next.some(node => node.id === 'a')).toBe(true);
  });

  it('calculateLayoutNodes preserves an existing node position when layoutAll is false and the node is already on canvas', () => {
    const setFlowNodes = vi.fn();
    const existing = makeNode('Agent 1', PipelineNodeTypes.Agent, 5, 5);
    const { result } = renderHook(() =>
      useFlowEditorNodeOperations({
        flowNodes: [existing],
        setFlowNodes,
        setFlowEdges: vi.fn(),
        setYamlJsonObject: vi.fn(),
        yamlJsonObjectRef: { current: {} },
        getViewport: () => ({ x: 0, y: 0, zoom: 1 }),
        setCenter: vi.fn(),
        getZoom: () => 1,
        editorRef: { current: null },
        editorWidth: 800,
        editorHeight: 600,
      }),
    );

    result.current.calculateLayoutNodes({ entry_point: existing.id, nodes: [{ id: existing.id, type: PipelineNodeTypes.Agent }] }, false, false, true);

    const updater = setFlowNodes.mock.calls[0]?.[0] as (prev: FlowNode[]) => FlowNode[];
    const next = updater([existing]);
    const found = next.find(node => node.id === existing.id);
    // `layoutAll=false` -> when the parsed node's id/type match an existing canvas node, that node's own live position wins over the freshly-parsed one.
    expect(found?.position).toEqual({ x: 5, y: 5 });
  });
});
