import userEvent from '@testing-library/user-event';
import { beforeAll, describe, expect, it, vi } from 'vitest';
import { waitFor, type RenderResult } from '@testing-library/react';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { HITLNode } from './HITLNode';

const PROJECT_ID = 'proj-1';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

function renderHITLNode(flowEditorOverrides: Partial<FlowEditorContextValue> = {}): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{
            testNode: props => (
              <>
                <HITLNode {...props} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <HITLNode {...props} />
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

describe('HITLNode', () => {
  it('renders the node id and the four handles (input + approve/edit/reject)', async () => {
    const { findByText, container } = renderHITLNode();

    expect(await findByText('Node1')).toBeInTheDocument();
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(4);
  });

  it('renders the Input label, the User message chip, and the Router mapping accordion (expanded by default)', async () => {
    const { findByText, getByText, getAllByText } = renderHITLNode();

    expect(await findByText('Node1')).toBeInTheDocument();
    expect(getAllByText('Input').length).toBeGreaterThan(0);
    expect(getByText('User message')).toBeInTheDocument();
    expect(getByText('Router mapping')).toBeInTheDocument();
    expect(getByText('APPROVE')).toBeInTheDocument();
    expect(getByText('EDIT')).toBeInTheDocument();
    expect(getByText('REJECT')).toBeInTheDocument();
  });

  it('selecting a route target for the approve action writes routes.approve and adds an edge', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn<(next: YamlPipelineDocument) => void>();
    const setFlowEdges = vi.fn();

    const { findByText, getAllByRole, getByRole } = renderHITLNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }, { id: 'Target1' }] },
      setYamlJsonObject,
      setFlowEdges,
    });
    await findByText('Node1');

    // Combobox order: [0] user_message Type select, [1..3] approve/edit/reject route selects, [4] edit-state-key, [5] Input (Advanced).
    const routeSelects = getAllByRole('combobox', { hidden: true });
    await user.click(routeSelects[1] as HTMLElement);
    await user.click(getByRole('option', { name: 'Target1' }));

    const nextDoc = setYamlJsonObject.mock.calls[0]?.[0];
    const updatedNode = nextDoc?.nodes?.find(node => node.id === 'Node1');
    expect(updatedNode?.['routes']).toEqual({ approve: 'Target1' });
    expect(setFlowEdges).toHaveBeenCalled();
  });

  it('disables every route select while the pipeline is running', async () => {
    const { findByText, getAllByRole } = renderHITLNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    const routeSelects = getAllByRole('combobox', { hidden: true });
    // Combobox order: [0] Input, [1] Type, [2..4] approve/edit/reject, [5] edit-state-key.
    // All 6 are disabled while the pipeline is running.
    for (const select of routeSelects) {
      expect(select).toHaveAttribute('aria-disabled', 'true');
    }
  });

  it('disables the edit route select specifically when there is no configured edit route and no edit_state_key', async () => {
    const { findByText, getAllByRole } = renderHITLNode({ yamlJsonObject: { nodes: [{ id: 'Node1' }] } });
    await findByText('Node1');

    // approve(1), edit(2), reject(3).
    const routeSelects = getAllByRole('combobox', { hidden: true });
    expect(routeSelects[1]).not.toHaveAttribute('aria-disabled', 'true');
    expect(routeSelects[2]).toHaveAttribute('aria-disabled', 'true');
    expect(routeSelects[3]).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('enables the edit route select once an edit_state_key is set, and shows no validation error', async () => {
    const { findByText, getAllByRole, queryByText } = renderHITLNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', edit_state_key: 'my_key' }] },
    });
    await findByText('Node1');

    const routeSelects = getAllByRole('combobox', { hidden: true });
    expect(routeSelects[2]).not.toHaveAttribute('aria-disabled', 'true');
    expect(queryByText('Provide an edit state key before using the Edit route.')).not.toBeInTheDocument();
  });

  it('shows the validation error text when an edit route is configured but no edit_state_key is set', async () => {
    const { findByText, findAllByText } = renderHITLNode({
      yamlJsonObject: { nodes: [{ id: 'Node1', routes: { edit: 'Target1' } }, { id: 'Target1' }] },
    });
    await findByText('Node1');

    // Shown three times: the edit route's own FormHelperText, the edit-state-key
    // select's FormHelperText (it reuses the same `routeErrorText` as its own
    // `error` prop), and the standalone validation `Typography` beneath it.
    const matches = await findAllByText('Provide an edit state key before using the Edit route.');
    expect(matches).toHaveLength(3);
  });

  it('selecting a new edit state key value writes edit_state_key onto the node', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn<(next: YamlPipelineDocument) => void>();

    const { findByText, getAllByRole, getByRole } = renderHITLNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }], state: { input: { type: 'str' } } },
      setYamlJsonObject,
    });
    await findByText('Node1');

    const routeSelects = getAllByRole('combobox', { hidden: true });
    // The edit-state-key select is the 5th combobox (index 4).
    await user.click(routeSelects[4] as HTMLElement);
    await user.click(getByRole('option', { name: 'input' }));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({ nodes: [expect.objectContaining({ id: 'Node1', edit_state_key: 'input' })] }),
    );
  });

  it('is entirely inert (renders no handles) with no FlowEditorContext ancestor, since NodeCard itself requires one', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{
            testNode: props => (
              <>
                <HITLNode {...props} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <HITLNode {...props} />
                </NodeConfigPanelContext.Provider>
              </>
            ),
          }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    // Give the router a tick to settle -- `.react-flow` itself always renders, even though this node renders nothing inside it.
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });
});
