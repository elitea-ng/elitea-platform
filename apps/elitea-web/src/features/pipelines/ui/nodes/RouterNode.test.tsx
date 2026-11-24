import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, type RenderResult } from '@testing-library/react';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlConditionSpec, YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { RouterNode, type RouterNodeProps } from './RouterNode';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';

// The AI Assistant trigger POSTs `predict_llm`, which the Go router mounts in
// no profile, so `AIAssistantInput` hides it by default. See
// `shared/config/backendCapabilities`. These tests assert the trigger's own
// behaviour, so they turn the capability on.
beforeEach(() => {
  setBackendCapabilityForTests('llmPredictStreaming', true);
});

afterEach(() => {
  resetBackendCapabilitiesForTests();
});


installCodeMirrorTestPolyfills();

const PROJECT_ID = 'proj-1';

/**
 * `RouterNode` (unlike `PrinterNode`/`CodeNode`) declares `extends
 * NodeProps<FlowNode>`, which pulls in a dozen `@xyflow/react`-internal
 * fields (`dragging`, `selectable`, `deletable`, ...) this component never
 * reads. Cast rather than fabricate the full shape, matching this batch's
 * established "runtime shape matches, types don't structurally unify"
 * convention (used throughout this port for the identical situation).
 */
const minimalRouterNodeProps = { id: 'Node1', data: {}, type: 'testNode' } as unknown as RouterNodeProps;

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

afterEach(() => {
  cleanup();
});

function renderRouterNode(flowEditorOverrides: Partial<FlowEditorContextValue> = {}, edges: Edge[] = []): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={edges}
          nodeTypes={{
            testNode: props => (
              <>
                <RouterNode {...props} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <RouterNode {...props} />
                </NodeConfigPanelContext.Provider>
              </>
            ),
          }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

describe('RouterNode', () => {
  it('renders the node id and all three handles (target + routes source + default-output source)', async () => {
    const { findByText, container } = renderRouterNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(3);
  });

  it('renders the Condition field, the Routes select, and the Default output chip/select', async () => {
    const { findByText, getByText, getAllByText } = renderRouterNode();
    await findByText('Node1');

    expect(getByText('Condition')).toBeInTheDocument();
    expect(getByText('Routes')).toBeInTheDocument();
    expect(getAllByText('Default output').length).toBeGreaterThan(0);
  });

  it('renders the condition value from the matching yaml node in the Condition field', async () => {
    // `RouterNode.tsx`'s own doc comment: for a Router node the baseline
    // stores a plain string under the `condition` key even though it's
    // declared `YamlConditionSpec` (sized for the legacy Condition node) --
    // the same cast `RouterNode.tsx` itself applies when reading it back.
    const { findByText, getByDisplayValue } = renderRouterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', condition: 'x > 1' as unknown as YamlConditionSpec }] },
    });
    await findByText('Node1');

    expect(getByDisplayValue('x > 1')).toBeInTheDocument();
  });

  it('typing directly into the Condition field writes the new value onto condition (regression coverage for the adversarial-review-confirmed AIAssistantInput bug documented in this file\'s own module doc comment -- its base field used to forward no onChange at all)', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', condition: 'x > 1' as unknown as YamlConditionSpec }],
    };
    const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, setYamlJsonObject });

    // Mounted directly (not via `nodeTypes`/an actual `<ReactFlow>` node) --
    // same rationale as the "handleConditionFilling ... modal" test below:
    // the Condition field has no `nopan nodrag` shield, so a real `<ReactFlow>`
    // canvas bubbles its mousedown into d3-drag's node-drag-start handler.
    const { findByText, getByDisplayValue } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <FlowEditorContext.Provider value={flowEditorValue}>
          <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
            <RouterNode {...minimalRouterNodeProps} />
          </NodeConfigPanelContext.Provider>
        </FlowEditorContext.Provider>
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await findByText('Condition');

    const conditionField = getByDisplayValue('x > 1');
    await user.type(conditionField, '0');

    expect(setYamlJsonObject).toHaveBeenCalled();
    const lastCall = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    const updatedNode = lastCall?.nodes?.find(node => node.id === 'Node1');
    expect(updatedNode?.condition).toBe('x > 10');
  });

  it("handleConditionFilling writes the AI Assistant modal's content back onto condition when the modal is closed", async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', condition: 'x > 1' as unknown as YamlConditionSpec }],
    };
    const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, setYamlJsonObject });
    const socketClient = createTestSocketClient();

    // Mounted directly (not via `nodeTypes`/an actual `<ReactFlow>` node) --
    // see `PrinterNode.test.tsx`'s identical helper doc comment for the full
    // rationale (the AI Assistant trigger `IconButton` has no `nopan nodrag`
    // shield, and `AIAssistantModal` needs a live `SocketClientContext`).
    const { findByText, container, getByRole } = renderWithRouterAndProject(
      <SocketClientContext.Provider value={socketClient}>
        <ReactFlowProvider>
          <FlowEditorContext.Provider value={flowEditorValue}>
            <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
              <RouterNode {...minimalRouterNodeProps} />
            </NodeConfigPanelContext.Provider>
          </FlowEditorContext.Provider>
        </ReactFlowProvider>
      </SocketClientContext.Provider>,
      PROJECT_ID,
    );
    await findByText('Condition');

    const trigger = container.querySelector('[aria-label="AI Assistant"]');
    expect(trigger).not.toBeNull();
    await user.click(trigger as HTMLElement);

    // Closing the modal without editing still calls `handleBlur(currentValue)`
    // -> `fieldBinding.onInput` (`AIAssistantModal.tsx`'s own `onClickClose`),
    // reaching `handleConditionFilling` with the unedited original value.
    await user.click(getByRole('button', { name: 'Close' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({ nodes: [expect.objectContaining({ id: 'Node1', condition: 'x > 1' })] }),
    );
  });

  it('defaults the default-output select to END when default_output is unset', async () => {
    const { findByText, getByText } = renderRouterNode();
    await findByText('Node1');

    expect(getByText('END')).toBeInTheDocument();
  });

  it('selecting a routes target adds an edge and writes the routes array', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const setFlowEdges = vi.fn();

    const { findByText, getAllByRole, getByRole } = renderRouterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Target1' }] },
      setYamlJsonObject,
      setFlowEdges,
    });
    await findByText('Node1');

    // Combobox order: [0] Routes (multi-select), [1] Input (multi-select), [2] Default output.
    const comboboxes = getAllByRole('combobox', { hidden: true });
    await user.click(comboboxes[0] as HTMLElement);
    await user.click(getByRole('option', { name: 'Target1' }));

    const nextDoc = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const routerNode = nextDoc?.nodes?.find(node => node.id === 'Node1');
    expect(routerNode?.['routes']).toEqual(['Target1']);
    expect(setFlowEdges).toHaveBeenCalled();
  });

  it('selecting a default-output target writes default_output and adds a non-interrupt edge', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const setFlowEdges = vi.fn();

    const { findByText, getAllByRole, getByRole } = renderRouterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Target1' }] },
      setYamlJsonObject,
      setFlowEdges,
    });
    await findByText('Node1');

    const comboboxes = getAllByRole('combobox', { hidden: true });
    await user.click(comboboxes[1] as HTMLElement);
    await user.click(getByRole('option', { name: 'Target1' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', default_output: 'Target1' }), expect.objectContaining({ id: 'Target1' })],
      }),
    );
    expect(setFlowEdges).toHaveBeenCalledTimes(1);
    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: unknown[]) => unknown[];
    expect(updater([])).toEqual([
      expect.objectContaining({
        id: 'xy-edge__Node1default_output---Target1',
        source: 'Node1',
        sourceHandle: 'routerNode_default_output',
        target: 'Target1',
        data: {},
      }),
    ]);
  });

  it('marks the default-output edge as an interrupt when the target is already in interrupt_before', async () => {
    const user = userEvent.setup();
    const setFlowEdges = vi.fn();

    const { findByText, getAllByRole, getByRole } = renderRouterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Target1' }], interrupt_before: ['Target1'] },
      setFlowEdges,
    });
    await findByText('Node1');

    const comboboxes = getAllByRole('combobox', { hidden: true });
    await user.click(comboboxes[1] as HTMLElement);
    await user.click(getByRole('option', { name: 'Target1' }));

    const updater = setFlowEdges.mock.calls[0]?.[0] as (prev: unknown[]) => unknown[];
    expect(updater([])).toEqual([expect.objectContaining({ data: { label: 'interrupt' } })]);
  });

  it('disables every field while the pipeline is running', async () => {
    const { findByText, getAllByRole } = renderRouterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    for (const combobox of getAllByRole('combobox', { hidden: true })) {
      expect(combobox).toHaveAttribute('aria-disabled', 'true');
    }
    for (const textbox of getAllByRole('textbox', { hidden: true })) {
      expect(textbox).toBeDisabled();
    }
  });

  it('does not error when this node already has an incoming edge and an existing default-output edge (both connectable flags false)', async () => {
    const { findByText, container } = renderRouterNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Other' }] } },
      [
        { id: 'e1', source: 'Other', target: 'Node1' },
        { id: 'e2', source: 'Node1', sourceHandle: 'routerNode_default_output', target: 'Other' },
      ],
    );
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(3);
  });
});
