import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { FlowEdge, FlowNode } from '../lib/flow-editor/reactFlowTypes';
import {
  extractSxDisplay,
  useFlowEditorInitialFitView,
  useFlowEditorLayoutVersionSync,
  useFlowEditorPersistence,
  useFlowEditorReset,
  useFlowEditorResizeObserver,
  useYamlJsonObjectRef,
} from './useFlowEditorLifecycle';

function makeNode(id: string): FlowNode {
  return { id, type: 'agent', position: { x: 0, y: 0 }, data: {} };
}

function makeEdge(id: string, source: string, target: string): FlowEdge {
  return { id, source, target };
}

describe('useYamlJsonObjectRef', () => {
  it('mirrors the latest yamlJsonObject into a ref after each render', () => {
    const { result, rerender } = renderHook(({ doc }) => useYamlJsonObjectRef(doc), {
      initialProps: { doc: { entry_point: 'a' } },
    });
    expect(result.current.current).toEqual({ entry_point: 'a' });

    rerender({ doc: { entry_point: 'b' } });
    expect(result.current.current).toEqual({ entry_point: 'b' });
  });
});

describe('useFlowEditorReset', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('does nothing when resetFlag is false', () => {
    const setFlowNodes = vi.fn();
    const onResetHandled = vi.fn();
    renderHook(() =>
      useFlowEditorReset({
        resetFlag: false,
        initialNodes: [makeNode('a')],
        initialEdges: [],
        setFlowNodes,
        setFlowEdges: vi.fn(),
        onResetRunParseStatus: vi.fn(),
        onResetHandled,
        persistNodes: vi.fn(),
        persistEdges: vi.fn(),
        fitView: vi.fn(),
      }),
    );
    expect(setFlowNodes).not.toHaveBeenCalled();
    expect(onResetHandled).not.toHaveBeenCalled();
  });

  it('snaps nodes/edges back to initial, clears run status, and acknowledges after persistence', () => {
    const setFlowNodes = vi.fn();
    const setFlowEdges = vi.fn();
    const onResetRunParseStatus = vi.fn();
    const onResetHandled = vi.fn();
    const initialNodes = [makeNode('a'), makeNode('b'), makeNode('c')];

    renderHook(() =>
      useFlowEditorReset({
        resetFlag: true,
        initialNodes,
        initialEdges: [],
        setFlowNodes,
        setFlowEdges,
        onResetRunParseStatus,
        onResetHandled,
        persistNodes: vi.fn(),
        persistEdges: vi.fn(),
        fitView: vi.fn(),
      }),
    );

    expect(setFlowNodes).toHaveBeenCalledWith(initialNodes);
    expect(setFlowEdges).toHaveBeenCalledWith([]);
    expect(onResetRunParseStatus).toHaveBeenCalled();
    expect(onResetHandled).not.toHaveBeenCalled();
    vi.advanceTimersByTime(150);
    expect(onResetHandled).toHaveBeenCalledTimes(1);
  });

  it('persists the reset graph and fits the view after the 150ms sync delay when there is real content', () => {
    const persistNodes = vi.fn();
    const persistEdges = vi.fn();
    const fitView = vi.fn();
    const initialNodes = [makeNode('a'), makeNode('b'), makeNode('c')];

    renderHook(() =>
      useFlowEditorReset({
        resetFlag: true,
        initialNodes,
        initialEdges: [],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        onResetRunParseStatus: vi.fn(),
        onResetHandled: vi.fn(),
        persistNodes,
        persistEdges,
        fitView,
      }),
    );

    expect(persistNodes).not.toHaveBeenCalled();
    vi.advanceTimersByTime(150);
    expect(persistNodes).toHaveBeenCalledWith(initialNodes);
    expect(persistEdges).toHaveBeenCalledWith([]);
    expect(fitView).toHaveBeenCalled();
  });

  it.each(['llm', 'code', 'hitl'])('fits the reset view for an authored %s node and END', (type) => {
    const persistNodes = vi.fn();
    const persistEdges = vi.fn();
    const fitView = vi.fn();
    const initialNodes = [
      { ...makeNode('authored'), type, position: { x: 60, y: 870 } },
      { ...makeNode('END'), type: 'END', position: { x: 60, y: 1043 } },
    ];
    const initialEdges = [makeEdge('authored-END', 'authored', 'END')];
    const setFlowNodes = vi.fn();
    const setFlowEdges = vi.fn();
    renderHook(() =>
      useFlowEditorReset({
        resetFlag: true,
        initialNodes,
        initialEdges,
        setFlowNodes,
        setFlowEdges,
        onResetRunParseStatus: vi.fn(),
        onResetHandled: vi.fn(),
        persistNodes,
        persistEdges,
        fitView,
      }),
    );
    expect(setFlowNodes).toHaveBeenCalledWith(initialNodes);
    expect(setFlowEdges).toHaveBeenCalledWith(initialEdges);
    vi.advanceTimersByTime(149);
    expect(fitView).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(persistNodes).toHaveBeenCalledWith(initialNodes);
    expect(persistEdges).toHaveBeenCalledWith(initialEdges);
    expect(fitView).toHaveBeenCalledTimes(1);
    expect(fitView.mock.invocationCallOrder[0]).toBeGreaterThan(persistEdges.mock.invocationCallOrder[0] ?? 0);
  });


  it.each([
    { label: 'empty graph', nodes: [] },
    { label: 'END placeholder', nodes: [{ ...makeNode('END'), type: 'END' }] },
    {
      label: 'entry/end placeholders',
      nodes: [
        { ...makeNode('entry_point'), type: 'entry_point' },
        { ...makeNode('END'), type: 'END' },
      ],
    },
  ])('does not fit the reset view for $label', ({ nodes }) => {
    const fitView = vi.fn();
    renderHook(() =>
      useFlowEditorReset({
        resetFlag: true,
        initialNodes: nodes,
        initialEdges: [],
        setFlowNodes: vi.fn(),
        setFlowEdges: vi.fn(),
        onResetRunParseStatus: vi.fn(),
        onResetHandled: vi.fn(),
        persistNodes: vi.fn(),
        persistEdges: vi.fn(),
        fitView,
      }),
    );
    vi.advanceTimersByTime(150);
    expect(fitView).not.toHaveBeenCalled();
  });
});

describe('useFlowEditorInitialFitView', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('fits the view after 100ms when there is more than the entry/end pair', () => {
    const fitView = vi.fn();
    renderHook(() => useFlowEditorInitialFitView(3, fitView));
    expect(fitView).not.toHaveBeenCalled();
    vi.advanceTimersByTime(100);
    expect(fitView).toHaveBeenCalledTimes(1);
  });

  it('never fits the view for 2 or fewer initial nodes', () => {
    const fitView = vi.fn();
    renderHook(() => useFlowEditorInitialFitView(2, fitView));
    vi.advanceTimersByTime(1000);
    expect(fitView).not.toHaveBeenCalled();
  });
});

describe('useFlowEditorPersistence', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('persists nodes after 100ms once they diverge from initialNodes', () => {
    const persistNodes = vi.fn();
    const flowNodes = [makeNode('a')];
    renderHook(() =>
      useFlowEditorPersistence({
        flowNodes,
        flowEdges: [],
        initialNodes: [],
        initialEdges: [],
        isRunningPipeline: false,
        persistNodes,
        persistEdges: vi.fn(),
      }),
    );
    vi.advanceTimersByTime(100);
    expect(persistNodes).toHaveBeenCalledWith(flowNodes);
  });

  it('does not persist nodes while a pipeline run is in progress', () => {
    const persistNodes = vi.fn();
    renderHook(() =>
      useFlowEditorPersistence({
        flowNodes: [makeNode('a')],
        flowEdges: [],
        initialNodes: [],
        initialEdges: [],
        isRunningPipeline: true,
        persistNodes,
        persistEdges: vi.fn(),
      }),
    );
    vi.advanceTimersByTime(100);
    expect(persistNodes).not.toHaveBeenCalled();
  });

  it('persists edges after 100ms once they diverge from initialEdges', () => {
    const persistEdges = vi.fn();
    const flowEdges = [makeEdge('e1', 'a', 'b')];
    renderHook(() =>
      useFlowEditorPersistence({
        flowNodes: [],
        flowEdges,
        initialNodes: [],
        initialEdges: [],
        isRunningPipeline: false,
        persistNodes: vi.fn(),
        persistEdges,
      }),
    );
    vi.advanceTimersByTime(100);
    expect(persistEdges).toHaveBeenCalledWith(flowEdges);
  });

  it('does nothing once flowNodes/flowEdges already match the initial graph', () => {
    const persistNodes = vi.fn();
    const persistEdges = vi.fn();
    const nodes = [makeNode('a')];
    const edges = [makeEdge('e1', 'a', 'b')];
    renderHook(() =>
      useFlowEditorPersistence({
        flowNodes: nodes,
        flowEdges: edges,
        initialNodes: nodes,
        initialEdges: edges,
        isRunningPipeline: false,
        persistNodes,
        persistEdges,
      }),
    );
    vi.advanceTimersByTime(200);
    expect(persistNodes).not.toHaveBeenCalled();
    expect(persistEdges).not.toHaveBeenCalled();
  });
});

describe('useFlowEditorLayoutVersionSync', () => {
  it('re-lays-out and reports the current version when the stored version lags and nodes are initialized', () => {
    const onReLayout = vi.fn();
    const onLayoutVersionChange = vi.fn();
    renderHook(() =>
      useFlowEditorLayoutVersionSync({
        layoutVersion: '0.9',
        currentLayoutVersion: '1.0',
        nodesInitialized: true,
        onReLayout,
        onLayoutVersionChange,
      }),
    );
    expect(onReLayout).toHaveBeenCalled();
    expect(onLayoutVersionChange).toHaveBeenCalledWith('1.0');
  });

  it('does nothing once the stored version already matches', () => {
    const onReLayout = vi.fn();
    renderHook(() =>
      useFlowEditorLayoutVersionSync({
        layoutVersion: '1.0',
        currentLayoutVersion: '1.0',
        nodesInitialized: true,
        onReLayout,
        onLayoutVersionChange: vi.fn(),
      }),
    );
    expect(onReLayout).not.toHaveBeenCalled();
  });

  it('waits until nodes are initialized before re-laying-out', () => {
    const onReLayout = vi.fn();
    renderHook(() =>
      useFlowEditorLayoutVersionSync({
        layoutVersion: '0.9',
        currentLayoutVersion: '1.0',
        nodesInitialized: false,
        onReLayout,
        onLayoutVersionChange: vi.fn(),
      }),
    );
    expect(onReLayout).not.toHaveBeenCalled();
  });
});

describe('useFlowEditorResizeObserver', () => {
  it('starts with the baseline literal fallback size before any element is attached', () => {
    const { result } = renderHook(() => useFlowEditorResizeObserver());
    expect(result.current.editorHeight).toBe(677);
    expect(result.current.editorWidth).toBe(622);
    expect(result.current.editorRef.current).toBeNull();
  });
});

describe('extractSxDisplay', () => {
  it('returns undefined for an undefined sx', () => {
    expect(extractSxDisplay(undefined)).toBeUndefined();
  });

  it('returns undefined for an array-form sx', () => {
    expect(extractSxDisplay([{ display: 'none' }])).toBeUndefined();
  });

  it('returns undefined for a function-form sx', () => {
    expect(extractSxDisplay(() => ({ display: 'none' }))).toBeUndefined();
  });

  it('returns undefined for a plain object sx with no display key', () => {
    expect(extractSxDisplay({ fontWeight: 700 })).toBeUndefined();
  });

  it('returns undefined when display is present but not a string', () => {
    expect(extractSxDisplay({ display: 123 as unknown as string })).toBeUndefined();
  });

  it('returns the display value for a plain object sx', () => {
    expect(extractSxDisplay({ display: 'none' })).toBe('none');
  });
});
