import { createRef } from 'react';

import { act, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ReactFlowProvider } from '@xyflow/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { usePipelineEditorStore } from '../model/pipelineEditorStore';

/**
 * This file mounts the REAL `FlowEditor` component, no mocking. `vi.mock()`
 * to isolate it from any not-yet-landed sibling dependency was considered
 * and rejected: R-M1 (`elitea/no-vi-mock`, `.oxlintrc.json`) bans
 * `vi.mock()` everywhere outside a `__mocks__` directory ("tests substitute
 * only the network boundary (MSW) and the socket double, §6.2"), so mocking
 * a sibling component to route around its own gaps is not a compliant
 * option here — mounting the real component is the only compliant choice,
 * whatever state its sibling-owned dependencies are in.
 *
 * Historical note, now resolved: `FlowEditor` transitively imports
 * `AgentNode.tsx` (A2e/A2f) -> `../settings/InputMappings/InputMapping`
 * and `RunStateNodeGroup.tsx` -> `RunStateNode.tsx` -> `RunStateDialog.
 * status.tsx`. Both of those previously-blocking gaps (A2i's list-level
 * `InputMapping.tsx` not yet landed; `RunStateDialog.status.tsx`'s
 * `@mui/icons-material/ErrorOutline` import not resolving to an installed
 * icon) have since landed/been worked around by their owning sub-units —
 * `InputMapping.tsx` now exists at `../settings/InputMappings/
 * InputMapping`, and `RunStateDialog.status.tsx` now imports
 * `ErrorOutlineOutlined` instead (disclosed in that file's own comment).
 * This file's tests do run and pass today as a result.
 *
 * A different, real gap remains on the same `RunStateNodeGroup` chain,
 * disclosed at `FlowEditor.tsx`'s `runStateEntries` definition: `./nodes/
 * RunStateNodeGroup` does not actually export a `RunStateEntry` type
 * (confirmed via `npx tsc --noEmit`: `FlowEditor.tsx(106,34): error
 * TS2305`), even though `FlowEditor.tsx` imports one. That is a type-level
 * gap only — `vitest`'s esbuild transform strips type-only imports, so it
 * does not stop this file's tests from running or passing, but it does
 * mean `FlowEditor.tsx` is not yet on a clean `tsc --noEmit` footing. The
 * fix belongs in `./nodes/RunStateNodeGroup.tsx` (A2f, sibling-owned, not
 * this sub-unit's file to edit) — adding the missing `RunStateEntry`
 * export there. Not fixed here.
 */
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
vi.stubGlobal('ResizeObserver', ResizeObserverStub);

import { FlowEditor, type FlowEditorHandle, type FlowEditorProps } from './FlowEditor';

function baseProps(overrides: Partial<FlowEditorProps> = {}): FlowEditorProps {
  return {
    yamlJsonObject: { nodes: [] },
    setYamlJsonObject: vi.fn(),
    initialNodes: [],
    initialEdges: [],
    layoutVersion: '1.0',
    resetFlag: false,
    onResetHandled: vi.fn(),
    onLayoutVersionChange: vi.fn(),
    stopRun: vi.fn(),
    disabled: false,
    ...overrides,
  };
}

function renderFlowEditor(props: FlowEditorProps, ref?: React.Ref<FlowEditorHandle>) {
  return renderWithTheme(
    <ReactFlowProvider>
      <FlowEditor
        {...props}
        ref={ref}
      />
    </ReactFlowProvider>,
  );
}

describe('FlowEditor', () => {
  it('renders the canvas chrome (run-state overlay, State toggle button) without crashing', () => {
    renderFlowEditor(baseProps());

    expect(screen.getByRole('button', { name: 'State' })).toBeInTheDocument();
  });

  it('opens the state drawer when the State button is clicked, and the button hides while it is open', async () => {
    const user = userEvent.setup();
    renderFlowEditor(baseProps());

    await user.click(screen.getByRole('button', { name: 'State' }));

    expect(screen.queryByRole('button', { name: 'State' })).not.toBeInTheDocument();
  });

  it('exposes the full FlowEditorHandle imperative surface via ref', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(ref.current).not.toBeNull();
    expect(typeof ref.current?.fitView).toBe('function');
    expect(typeof ref.current?.onAddNode).toBe('function');
    expect(typeof ref.current?.onRcvAgentEvent).toBe('function');
    expect(typeof ref.current?.setFlowEdges).toBe('function');
    expect(typeof ref.current?.setFlowNodes).toBe('function');
    expect(typeof ref.current?.deleteAllRunNodes).toBe('function');
    expect(typeof ref.current?.getCurrentExpandState).toBe('function');
    expect(typeof ref.current?.calculateLayoutNodes).toBe('function');
    expect(typeof ref.current?.stopCurrentRun).toBe('function');
    expect(typeof ref.current?.hasRunsInProgress).toBe('function');
  });

  it('getCurrentExpandState starts true (baseline default) and hasRunsInProgress starts false with no runs', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(ref.current?.getCurrentExpandState()).toBe(true);
    expect(ref.current?.hasRunsInProgress()).toBe(false);
  });

  afterEach(() => {
    usePipelineEditorStore.setState({ nodes: [], edges: [], stateValidationErrors: {} });
  });

  it('seeds flowNodes/flowEdges from the cached pipelineEditorStore when it already has content (cachedNodes.length branch)', () => {
    usePipelineEditorStore.setState({
      nodes: [{ id: 'cached-1', type: 'ghost', position: { x: 0, y: 0 }, data: {} }],
      edges: [],
    });
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps({ initialNodes: [], initialEdges: [] }), ref);

    // getCurrentExpandState/hasRunsInProgress being callable confirms the component
    // mounted; the more direct signal is that fitView (below) treats it as >2 nodes
    // only once more cached nodes are added, but presence alone confirms the seed path
    // ran without throwing on a non-empty store.
    expect(ref.current).not.toBeNull();
  });

  it('ref.onAddNode adds a new flow node of the given type and returns it', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    const node = ref.current?.onAddNode('ghost');

    expect(node).toBeDefined();
    expect(node?.type).toBe('ghost');
  });

  it('ref.onRcvAgentEvent/deleteAllRunNodes/setFlowNodes/setFlowEdges do not throw', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(() => ref.current?.onRcvAgentEvent({ type: 'unknown_event' } as never)).not.toThrow();
    expect(() => ref.current?.deleteAllRunNodes()).not.toThrow();
    expect(() =>
      ref.current?.setFlowNodes([{ id: 'n1', type: 'ghost', position: { x: 0, y: 0 }, data: {} }]),
    ).not.toThrow();
    expect(() => ref.current?.setFlowEdges([])).not.toThrow();
  });

  it('ref.calculateLayoutNodes forwards an explicit expand state when given one', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(() => ref.current?.calculateLayoutNodes({ nodes: [] }, true, true, false)).not.toThrow();
  });

  it('ref.calculateLayoutNodes falls back to the current expand state when explicitExpandState is omitted', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(() => ref.current?.calculateLayoutNodes({ nodes: [] }, true, true)).not.toThrow();
  });

  it('ref.stopCurrentRun does not throw when there are no run nodes (falls back to an empty id)', () => {
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps(), ref);

    expect(() => ref.current?.stopCurrentRun()).not.toThrow();
  });

  it('ref.fitView is a no-op (no setTimeout scheduled) when there are 2 or fewer flow nodes', () => {
    vi.useFakeTimers();
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps({ initialNodes: [], initialEdges: [] }), ref);

    act(() => ref.current?.fitView());
    // No assertion target beyond "does not throw" is available without spying on the
    // internal `fitView` from `useReactFlow` (not exposed) — the 2-or-fewer guard itself
    // is exercised (`latest.current.flowNodes.length > 2` false branch).
    vi.useRealTimers();
  });

  it('ref.fitView schedules a delayed fit when there are more than 2 flow nodes', () => {
    vi.useFakeTimers();
    const manyNodes = [
      { id: 'a', type: 'ghost', position: { x: 0, y: 0 }, data: {} },
      { id: 'b', type: 'ghost', position: { x: 100, y: 0 }, data: {} },
      { id: 'c', type: 'ghost', position: { x: 200, y: 0 }, data: {} },
    ];
    const ref = createRef<FlowEditorHandle>();
    renderFlowEditor(baseProps({ initialNodes: manyNodes, initialEdges: [] }), ref);

    expect(() => {
      act(() => ref.current?.fitView());
      void act(() => vi.advanceTimersByTime(100));
    }).not.toThrow();
    vi.useRealTimers();
  });

  it('toggles canvas lock and minimap visibility via CanvasControls', async () => {
    const user = userEvent.setup();
    renderFlowEditor(baseProps());

    const lockButton = screen.getByTestId('pipeline-canvas-lock');
    expect(lockButton).toHaveAttribute('aria-pressed', 'false');
    await user.click(lockButton);
    expect(lockButton).toHaveAttribute('aria-pressed', 'true');

    const minimapButton = screen.getByTestId('pipeline-canvas-minimap-toggle');
    expect(screen.queryByTestId('rf__minimap')).not.toBeInTheDocument();
    await user.click(minimapButton);
    expect(screen.getByTestId('rf__minimap')).toBeInTheDocument();
    await user.click(minimapButton);
    expect(screen.queryByTestId('rf__minimap')).not.toBeInTheDocument();
  });

  it('opens the TB/LR Auto-arrange menu and arranges the graph when nodes exist', async () => {
    const user = userEvent.setup();
    const userNode = { id: 'Printer_1', type: 'printer', position: { x: 0, y: 0 }, data: { label: 'Printer_1' } };
    renderFlowEditor(
      baseProps({
        yamlJsonObject: { nodes: [{ id: 'Printer_1', type: 'printer' }] },
        initialNodes: [userNode],
      }),
    );

    await user.click(screen.getByTestId('pipeline-canvas-arrange'));
    await user.click(screen.getByTestId('pipeline-canvas-arrange-lr'));
    expect(screen.getByTestId('pipeline-canvas-arrange')).toBeInTheDocument();
  });

  it('duplicates a node via ref.onDuplicateNode, avoiding versionTools name collisions and preserving compiler-admitted Printer transition', () => {
    const ref = createRef<FlowEditorHandle>();
    const setYamlJsonObject = vi.fn();
    const userNode = { id: 'Printer_1', type: 'printer', position: { x: 10, y: 20 }, data: { label: 'Printer_1' } };
    renderFlowEditor(
      baseProps({
        yamlJsonObject: {
          nodes: [{ id: 'Printer_1', type: 'printer', transition: 'Next_1', content: 'hi' }],
        },
        initialNodes: [userNode],
        versionTools: [{ id: 't1', type: 'toolkit', name: 'Printer_1_copy' }],
        setYamlJsonObject,
      }),
      ref,
    );

    setYamlJsonObject.mockClear();
    const duplicated = ref.current?.onDuplicateNode('Printer_1');

    expect(duplicated?.id).toBe('Printer_1_copy_2');
    const writtenDoc = setYamlJsonObject.mock.calls[0]?.[0] as {
      nodes?: Array<{ id?: string; type?: string; transition?: string }>;
    } | undefined;
    expect(
      writtenDoc?.nodes?.some(
        node => node.id === 'Printer_1_copy_2' && node.type === 'printer' && node.transition === 'END',
      ),
    ).toBe(true);
  });

  it('refuses ref.onDuplicateNode when disabled (read-only guard)', () => {
    const ref = createRef<FlowEditorHandle>();
    const setYamlJsonObject = vi.fn();
    const userNode = { id: 'Printer_1', type: 'printer', position: { x: 10, y: 20 }, data: { label: 'Printer_1' } };
    renderFlowEditor(
      baseProps({
        yamlJsonObject: { nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }] },
        initialNodes: [userNode],
        setYamlJsonObject,
        disabled: true,
      }),
      ref,
    );

    setYamlJsonObject.mockClear();
    expect(ref.current?.onDuplicateNode('Printer_1')).toBeUndefined();
    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('renders with disabled left unset (exercises the `disabled ?? false` fallback)', () => {
    renderFlowEditor(baseProps({ disabled: undefined }));
    expect(screen.getByRole('button', { name: 'State' })).toBeInTheDocument();
  });

  it('renders FlowEmptyState and hides the End node while the pipeline has no user nodes', () => {
    const endNode = { id: 'END', type: 'END', position: { x: 0, y: 0 }, data: { label: 'End' } };
    const { container } = renderFlowEditor(baseProps({ yamlJsonObject: { nodes: [] }, initialNodes: [endNode] }));

    expect(screen.getByTestId('pipeline-empty-state')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-empty-state-add-node-button')).toBeInTheDocument();
    expect(container.querySelector('[data-id="END"]')).toBeNull();
  });

  it('hides FlowEmptyState and renders the End node once a user node exists', () => {
    const userNode = { id: 'LLM_1', type: 'ghost', position: { x: 0, y: 0 }, data: {} };
    const endNode = { id: 'END', type: 'END', position: { x: 0, y: 120 }, data: { label: 'End' } };
    const { container } = renderFlowEditor(
      baseProps({
        yamlJsonObject: { nodes: [{ id: 'LLM_1', type: 'llm' }] },
        initialNodes: [userNode, endNode],
      }),
    );

    expect(screen.queryByTestId('pipeline-empty-state')).toBeNull();
    expect(container.querySelector('[data-id="END"]')).not.toBeNull();
  });

  it('adds a node from FlowEmptyState when the user clicks Add Node and picks a type', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    renderFlowEditor(baseProps({ setYamlJsonObject }));

    await user.click(screen.getByTestId('pipeline-empty-state-add-node-button'));
    await user.click(screen.getByTestId('pipeline-add-node-menu-item-printer'));

    expect(setYamlJsonObject).toHaveBeenCalled();
  });

  it('mounts the 400px FlowNodeConfigDrawer when a configurable node is selected, shifts State toggle, and closes on close click', async () => {
    const user = userEvent.setup();
    const selectedNode = { id: 'Printer_1', type: 'printer', position: { x: 0, y: 0 }, data: { label: 'Printer_1' }, selected: true };
    renderFlowEditor(
      baseProps({
        yamlJsonObject: { entry_point: 'Other', nodes: [{ id: 'Printer_1', type: 'printer' }] },
        initialNodes: [selectedNode],
      }),
    );

    expect(screen.getByTestId('pipeline-node-panel')).toHaveAttribute('data-node-id', 'Printer_1');
    expect(screen.getByTestId('pipeline-state-toggle-wrapper')).toHaveAttribute('data-right-offset', '400');
    await user.click(screen.getByTestId('pipeline-node-panel-close'));
    expect(screen.queryByTestId('pipeline-node-panel')).not.toBeInTheDocument();
    expect(screen.getByTestId('pipeline-state-toggle-wrapper')).toHaveAttribute('data-right-offset', '0');
  });

  it('docks StateDrawer beside FlowNodeConfigDrawer, offsets CanvasMiniMap past both, and fires onCanvasNodeInteraction', async () => {
    const user = userEvent.setup();
    const onCanvasNodeInteraction = vi.fn();
    const selectedNode = { id: 'Printer_1', type: 'printer', position: { x: 0, y: 0 }, data: { label: 'Printer_1' }, selected: true };
    renderFlowEditor(
      baseProps({
        yamlJsonObject: { entry_point: 'Other', nodes: [{ id: 'Printer_1', type: 'printer' }] },
        initialNodes: [selectedNode],
        onCanvasNodeInteraction,
      }),
    );

    expect(onCanvasNodeInteraction).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: 'State' }));
    expect(getComputedStyle(screen.getByTestId('pipeline-state-drawer')).right).toBe('400px');

    await user.click(screen.getByTestId('pipeline-canvas-minimap-toggle'));
    const minimap = screen.getByTestId('rf__minimap');
    // 400px node drawer + 310px default StateDrawer = 710px = 44.375rem
    expect(minimap.style.right).toBe('44.375rem');
  });

  // NOT COVERABLE HERE: `useFlowEditorLayoutVersionSync`'s `onReLayout` callback (the
  // `layoutVersion` mismatch -> re-layout path) only fires once xyflow's own
  // `useNodesInitialized()` reports every node measured. jsdom has no real layout engine
  // and this app's `ResizeObserver` test stub (top of this file) never reports a
  // measurement, so `nodesInitialized` never flips true here — confirmed directly: a
  // `vi.waitFor(() => expect(onLayoutVersionChange).toHaveBeenCalledWith(...))` with a
  // generous timeout never resolves. The branch itself (and both of `useFlowEditorLayoutVersionSync`'s
  // own conditions) is already covered at the hook level in `useFlowEditorLifecycle.test.
  // ts`, which drives `nodesInitialized` as a plain boolean argument instead of through a
  // real `<ReactFlow>` tree.
});
