import userEvent from '@testing-library/user-event';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, waitFor, type RenderResult } from '@testing-library/react';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { PrinterNode } from './PrinterNode';

const PROJECT_ID = 'proj-1';

installCodeMirrorTestPolyfills();

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

beforeEach(() => {
  // The AI Assistant triggers are hidden while `predict_llm` is unmounted —
  // see `shared/config/backendCapabilities`.
  setBackendCapabilityForTests('llmPredictStreaming', true);
});

afterEach(() => {
  cleanup();
  resetBackendCapabilitiesForTests();
});

function renderPrinterNode(flowEditorOverrides: Partial<FlowEditorContextValue> = {}, edges: Edge[] = []): RenderResult {
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
                <PrinterNode {...props} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <PrinterNode {...props} />
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

/**
 * Mounts `PrinterNode` directly (not via `nodeTypes`/an actual `<ReactFlow>`
 * node) -- used only for the one test below that clicks into
 * `SimpleLLMInputItem`'s own "Type" `SingleSelect` (a real, disclosed,
 * out-of-scope bug: that select -- unlike every `nopan nodrag`-wrapped
 * select this slice's OWNED files use -- has no such shield, so a mousedown
 * on it bubbles into React Flow's real draggable node wrapper and throws
 * out of `d3-drag` when mounted inside an actual `<ReactFlow>` tree; not
 * `SimpleLLMInputItem.tsx`'s own file, out of this group's scope to fix).
 * `CustomHandle` degrades gracefully with no real node id in context (logs
 * React Flow's internal error code 010, does not throw), so this still
 * renders everything meaningful about `PrinterNode` itself.
 */
function renderPrinterNodeBare(flowEditorOverrides: Partial<FlowEditorContextValue> = {}): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
          <PrinterNode id="Node1" />
        </NodeConfigPanelContext.Provider>
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

describe('PrinterNode', () => {
  it('renders the node id and both handles (target + source)', async () => {
    const { findByText, container } = renderPrinterNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('renders the printer input-mapping row and the Final Message field', async () => {
    const { findByText, getAllByText, getByLabelText } = renderPrinterNode();
    await findByText('Node1');

    expect(getAllByText('Printer').length).toBeGreaterThan(0);
    expect(getByLabelText('Final Message')).toBeInTheDocument();
  });

  it('initialises the printer input mapping to a fixed empty string when unset', async () => {
    const { findByText, getAllByText } = renderPrinterNode();
    await findByText('Node1');

    // `SimpleLLMInputItem`'s Type select defaults to "Fixed" for the printer row.
    expect(getAllByText('Fixed').length).toBeGreaterThan(0);
  });

  it('renders an existing final_message value in the Final Message field', async () => {
    const { findByText, getByDisplayValue } = renderPrinterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', final_message: 'Done!' }] },
    });
    await findByText('Node1');

    expect(getByDisplayValue('Done!')).toBeInTheDocument();
  });

  it('handleFinalMessageChange writes the AI Assistant modal\'s content back onto final_message when the modal is closed (the base field forwards no onChange -- see RouterNode.tsx\'s doc comment)', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', final_message: 'original message' }] };
    const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, setYamlJsonObject });
    const socketClient = createTestSocketClient();

    // Mounted directly (not via `nodeTypes`/an actual `<ReactFlow>` node, same
    // rationale as `renderPrinterNodeBare` above) -- the AI Assistant trigger
    // `IconButton` (`../AIAssistantInput.tsx`, out of this group's scope) has
    // no `nopan nodrag` shield either, reproducing the identical `d3-drag`
    // crash if clicked inside a real node wrapper. `AIAssistantModal` also
    // needs a live `SocketClientContext` (its streaming hook throws without
    // one) -- provided here via the established `createTestSocketClient()`
    // double (`AIAssistantInput.test.tsx`'s own precedent).
    const { findByText, container, getByRole } = renderWithRouterAndProject(
      <SocketClientContext.Provider value={socketClient}>
        <ReactFlowProvider>
          <FlowEditorContext.Provider value={flowEditorValue}>
            <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
              <PrinterNode id="Node1" />
            </NodeConfigPanelContext.Provider>
          </FlowEditorContext.Provider>
        </ReactFlowProvider>
      </SocketClientContext.Provider>,
      PROJECT_ID,
    );
    await findByText('Printer');

    // Two "AI Assistant" triggers exist (printer row + Final Message); the second is Final Message's own.
    const triggers = container.querySelectorAll('[aria-label="AI Assistant"]');
    expect(triggers).toHaveLength(2);
    await user.click(triggers[1] as HTMLElement);

    // Closing the modal without editing still calls `handleBlur(currentValue)`
    // -> `fieldBinding.onInput` (`AIAssistantModal.tsx`'s own `onClickClose`),
    // reaching `handleFinalMessageChange` with the unedited original value.
    await user.click(getByRole('button', { name: 'Close' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({ nodes: [expect.objectContaining({ id: 'Node1', final_message: 'original message' })] }),
    );
  });

  it('reads an existing printer input mapping value from the yaml node', async () => {
    const { findByText, getByDisplayValue } = renderPrinterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', input_mapping: { printer: { type: 'fixed', value: 'Result: {{x}}' } } }] },
    });
    await findByText('Node1');

    expect(getByDisplayValue('Result: {{x}}')).toBeInTheDocument();
  });

  it('changing the printer mapping type to Variable swaps the value field to a state-variable select and resets its value', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn<(next: YamlPipelineDocument) => void>();
    const { findByText, getAllByRole, getByRole } = renderPrinterNodeBare({
      yamlJsonObject: {
        nodes: [{ id: 'Node1', input_mapping: { printer: { type: 'fixed', value: 'was fixed' } } }],
        state: { input: { type: 'str' } },
      },
      setYamlJsonObject,
    });
    await findByText('Printer');

    // Combobox order: [0] printer's own Type select.
    const typeSelect = getAllByRole('combobox', { hidden: true })[0] as HTMLElement;
    await user.click(typeSelect);
    await user.click(getByRole('option', { name: 'Variable' }));

    const nextDoc = setYamlJsonObject.mock.calls[0]?.[0];
    const updatedNode = nextDoc?.nodes?.find(node => node.id === 'Node1');
    expect(updatedNode?.['input_mapping']).toEqual(expect.objectContaining({ printer: { type: 'variable', value: '' } }));
  });

  it('disables the printer mapping and Final Message fields while the pipeline is running', async () => {
    const { findByText, getAllByRole, getByLabelText } = renderPrinterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    expect(getAllByRole('combobox', { hidden: true })[0]).toHaveAttribute('aria-disabled', 'true');
    expect(getByLabelText('Final Message')).toBeDisabled();
  });

  it('disables fields when disabled is set (isRunningPipeline falsy)', async () => {
    const { findByText, getByLabelText } = renderPrinterNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      disabled: true,
    });
    await findByText('Node1');

    expect(getByLabelText('Final Message')).toBeDisabled();
  });

  it('does not throw with no FlowEditorContext ancestor -- every context read defaults gracefully, even though NodeCard itself then renders null', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{ testNode: PrinterNode }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });
});
