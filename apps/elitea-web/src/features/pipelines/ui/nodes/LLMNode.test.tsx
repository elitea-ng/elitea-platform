import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, waitFor, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider, type Edge } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { LLMNode } from './LLMNode';

const BASE = '/api/v2';
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

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})));
});

afterEach(() => {
  resetGeneratedClient();
  cleanup();
});

function renderLLMNode(
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  versionTools?: readonly { readonly type?: string; readonly name?: string; readonly toolkit_name?: string; readonly tools?: readonly string[] }[],
  edges: Edge[] = [],
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: { versionTools } }]}
          edges={edges}
          nodeTypes={{
            testNode: props => (
              <>
                <LLMNode
                  {...props}
                  versionTools={versionTools}
                />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <LLMNode
                    {...props}
                    versionTools={versionTools}
                  />
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

describe('LLMNode', () => {
  it('renders the node id and both handles (target + source)', async () => {
    const { findByText, container } = renderLLMNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('renders one SimpleLLMInputs row per default input mapping key (System/Task/Chat history)', async () => {
    const { findByText, getByText } = renderLLMNode();
    await findByText('Node1');

    expect(getByText('System')).toBeInTheDocument();
    expect(getByText('Task')).toBeInTheDocument();
    expect(getByText('Chat history')).toBeInTheDocument();
  });

  it('renders the Input, Output, and Toolkits selects', async () => {
    const { findByText, getAllByText } = renderLLMNode();
    await findByText('Node1');

    expect(getAllByText('Input').length).toBeGreaterThan(0);
    expect(getAllByText('Output').length).toBeGreaterThan(0);
    expect(getAllByText('Toolkits').length).toBeGreaterThan(0);
  });

  it('renders the CommonInterruptSettings switches', async () => {
    const { findByText, getByText } = renderLLMNode();
    await findByText('Node1');

    expect(getByText('Interrupt before')).toBeInTheDocument();
    expect(getByText('Interrupt after')).toBeInTheDocument();
    expect(getByText('Structured output')).toBeInTheDocument();
  });

  it('renders one LLMToolsSelect accordion row per toolkit named in tool_names', async () => {
    const versionTools = [{ type: 'github', toolkit_name: 'github', tools: ['create_issue', 'list_issues'] }];
    const { findByText, getByText } = renderLLMNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1', tool_names: { github: ['create_issue'] } }] } },
      versionTools,
    );
    await findByText('Node1');

    // `LLMToolsSelect`'s own accordion title: `${toolkitName} (${selected}/${total})`.
    expect(getByText('github (1/2)')).toBeInTheDocument();
  });

  it('renders no LLMToolsSelect rows when tool_names is empty', async () => {
    const { findByText, queryByText } = renderLLMNode();
    await findByText('Node1');

    expect(queryByText(/\(\d+\/\d+\)/)).not.toBeInTheDocument();
  });

  it('disables every combobox and switch while the pipeline is running', async () => {
    const { findByText, getAllByRole } = renderLLMNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      isRunningPipeline: true,
    });
    await findByText('Node1');

    for (const combobox of getAllByRole('combobox', { hidden: true })) {
      expect(combobox).toHaveAttribute('aria-disabled', 'true');
    }
    for (const switchControl of getAllByRole('switch', { hidden: true })) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('disables fields when disabled is set (isRunningPipeline falsy)', async () => {
    const { findByText, getAllByRole } = renderLLMNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      disabled: true,
    });
    await findByText('Node1');

    for (const switchControl of getAllByRole('switch', { hidden: true })) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('does not disable fields when neither isRunningPipeline nor disabled is set', async () => {
    const { findByText, getAllByRole } = renderLLMNode();
    await findByText('Node1');

    /*
     * The two interrupt switches are permanently disabled — an intersection
     * choice, not a per-node state: the native Rust runtime refuses any
     * pipeline declaring static interrupts (`compiler.rs:470-474`) while the
     * SDK worker honours them, so the editor authors only what both accept.
     * See `ui/settings/CommonInterruptSettings.tsx`. Only the switches that
     * really do track `disabled` are asserted here.
     */
    // `CommonInterruptSettings` renders exactly three switches, in order:
    // interrupt-before, interrupt-after, structured-output. The first two are
    // matched by position rather than by accessible name because inside the
    // React-Flow canvas they are `visibility: hidden` in this environment
    // (see the `hidden: true` option every query here already carries), and
    // a hidden element computes an empty accessible name.
    const switches = getAllByRole('switch', { hidden: true });
    expect(switches).toHaveLength(3);
    expect(switches[2]).not.toBeDisabled();
  });

  it('does not throw with no FlowEditorContext ancestor (NodeCard renders null)', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{ testNode: LLMNode }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });
});
