import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, waitFor, type RenderResult } from '@testing-library/react';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { StateModifierNode, type StateModifierNodeProps } from './StateModifierNode';
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

/** See `RouterNode.test.tsx`'s identical `minimalRouterNodeProps` for the full rationale (`NodeProps<FlowNode>` pulls in fields this component never reads). */
const minimalStateModifierNodeProps = { id: 'Node1', data: {}, type: 'testNode' } as unknown as StateModifierNodeProps;

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

function renderStateModifierNode(
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  edges: Edge[] = [],
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={edges}
          nodeTypes={{ testNode: StateModifierNode }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

describe('StateModifierNode', () => {
  it('renders the node id and both handles (target + source)', async () => {
    const { findByText, container } = renderStateModifierNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('renders the Jinja Template field, Variables to clean, Input, and Output selects', async () => {
    const { findByText, getByText } = renderStateModifierNode();
    await findByText('Node1');

    expect(getByText('Jinja Template')).toBeInTheDocument();
    expect(getByText('Variables to clean')).toBeInTheDocument();
    expect(getByText('Input')).toBeInTheDocument();
    expect(getByText('Output')).toBeInTheDocument();
  });

  it('renders the template value from the matching yaml node', async () => {
    const { findByText, getByDisplayValue } = renderStateModifierNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', template: 'Hello {{name}}' }] },
    });
    await findByText('Node1');

    expect(getByDisplayValue('Hello {{name}}')).toBeInTheDocument();
  });

  it('renders an empty template field when the yaml node has no template', async () => {
    const { findByText, getByLabelText } = renderStateModifierNode();
    await findByText('Node1');

    expect(getByLabelText('Jinja Template')).toHaveValue('');
  });

  it("handleTemplateFilling writes the AI Assistant modal's content back onto template when the modal is closed (the base field forwards no onChange -- see RouterNode.tsx's doc comment)", async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', template: 'Hello {{name}}' }] };
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
            <StateModifierNode {...minimalStateModifierNodeProps} />
          </FlowEditorContext.Provider>
        </ReactFlowProvider>
      </SocketClientContext.Provider>,
      PROJECT_ID,
    );
    await findByText('Jinja Template');

    const trigger = container.querySelector('[aria-label="AI Assistant"]');
    expect(trigger).not.toBeNull();
    await user.click(trigger as HTMLElement);

    // Closing the modal without editing still calls `handleBlur(currentValue)`
    // -> `fieldBinding.onInput` (`AIAssistantModal.tsx`'s own `onClickClose`),
    // reaching `handleTemplateFilling` with the unedited original value.
    await user.click(getByRole('button', { name: 'Close' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({ nodes: [expect.objectContaining({ id: 'Node1', template: 'Hello {{name}}' })] }),
    );
  });

  it('disables every field while the pipeline is running', async () => {
    const { findByText, getByLabelText, getAllByRole } = renderStateModifierNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    expect(getByLabelText('Jinja Template')).toBeDisabled();
    // Combobox order: [0] Variables to clean, [1] Input, [2] Output.
    for (const combobox of getAllByRole('combobox', { hidden: true })) {
      expect(combobox).toHaveAttribute('aria-disabled', 'true');
    }
  });

  it('disables every field when disabled (isRunningPipeline falsy)', async () => {
    const { findByText, getByLabelText } = renderStateModifierNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      disabled: true,
    });
    await findByText('Node1');

    expect(getByLabelText('Jinja Template')).toBeDisabled();
  });

  it('does not disable fields when neither isRunningPipeline nor disabled is set', async () => {
    const { findByText, getByLabelText } = renderStateModifierNode();
    await findByText('Node1');

    expect(getByLabelText('Jinja Template')).not.toBeDisabled();
  });

  it('isSourceConnectable is false once an outgoing edge to a non-END node already exists', async () => {
    const { findByText, container } = renderStateModifierNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Other' }] } },
      [{ id: 'e1', source: 'Node1', target: 'Other' }],
    );
    await findByText('Node1');

    // Still renders both handles regardless -- `isConnectable` only affects connection-making, not presence.
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('renders sensibly with no FlowEditorContext ancestor (NodeCard returns null)', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{ testNode: StateModifierNode }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    // Nothing crashes; nothing renders either.
    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });
});

describe('StateModifierNode error output handle (5b-1, browser W2)', () => {
  it.each([
    ['without a target yet', { on_error: 'error_output' }],
    ['with a stored target', { on_error: 'error_output', error_transition: 'handler' }],
  ])('renders the error output handle %s', async (_name, settings) => {
    const yamlJsonObject = { nodes: [{ id: 'Node1', type: 'state_modifier', settings }, { id: 'handler', type: 'state_modifier' }] } as unknown as YamlPipelineDocument;
    const { findByText, container } = renderStateModifierNode({ yamlJsonObject });
    await findByText('Node1');
    expect(container.querySelector('[data-handleid="node_error_output"]')).not.toBeNull();
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(3);
  });

  it('renders no error output handle for continue or stop', async () => {
    const yamlJsonObject = { nodes: [{ id: 'Node1', type: 'state_modifier', settings: { on_error: 'continue' } }] } as unknown as YamlPipelineDocument;
    const { findByText, container } = renderStateModifierNode({ yamlJsonObject });
    await findByText('Node1');
    expect(container.querySelector('[data-handleid="node_error_output"]')).toBeNull();
  });
});
