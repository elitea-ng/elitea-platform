import { cleanup, waitFor, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { AgentNode, partitionAgentMappingKeys, type AgentNodeProps } from './AgentNode';

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

function renderAgentNode(
  props: Partial<AgentNodeProps>,
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  edges: readonly FlowEdge[] = [],
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [{ id: 'Node1' }],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });
  const fullProps: AgentNodeProps = { id: 'Node1', ...props } as AgentNodeProps;

  const flowNode: FlowNode = { id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: fullProps.data ?? {} };

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[flowNode]}
          edges={[...edges]}
          nodeTypes={{
            testNode: () => (
              <>
                <AgentNode {...fullProps} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <AgentNode {...fullProps} />
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

describe('AgentNode', () => {
  it('renders the node id as the card name and both handles', async () => {
    const { container, getAllByRole } = renderAgentNode({});
    await waitFor(() => expect(getAllByRole('combobox', { hidden: true }).length).toBeGreaterThan(0));
    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('shows no orphan warning when no tool is bound', async () => {
    const { queryByText, findAllByText } = renderAgentNode({});
    await findAllByText('Agent');
    expect(queryByText('Agent not found — select a replacement or delete this node')).not.toBeInTheDocument();
  });

  it('shows the orphan warning when a tool is bound but versionTools has no Application-typed entries at all', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MissingAgent' }] };
    const { findByText } = renderAgentNode({ versionTools: [] }, { yamlJsonObject });
    expect(await findByText('Agent not found — select a replacement or delete this node')).toBeInTheDocument();
  });

  it('shows the orphan warning when the bound tool name matches no Application-typed versionTools entry', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MissingAgent' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'OtherAgent' }];
    const { findByText } = renderAgentNode({ versionTools }, { yamlJsonObject });
    expect(await findByText('Agent not found — select a replacement or delete this node')).toBeInTheDocument();
  });

  it('shows no orphan warning when the bound tool matches an Application-typed versionTools entry', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MyAgent' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'MyAgent' }];
    const { queryByText, findByText } = renderAgentNode({ versionTools }, { yamlJsonObject });
    // Selected label appears once ToolSelect resolves the Application entry.
    await findByText('MyAgent');
    expect(queryByText('Agent not found — select a replacement or delete this node')).not.toBeInTheDocument();
  });

  it('ignores a non-Application-typed versionTools entry when checking for orphan status -- still orphaned even though the name exists among non-Application tools', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'my-github' }] };
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const { findByText } = renderAgentNode({ versionTools }, { yamlJsonObject });
    expect(await findByText('Agent not found — select a replacement or delete this node')).toBeInTheDocument();
  });

  it('marks the source handle non-connectable once an outgoing edge to a non-END target already exists', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1' }, { id: 'Node2' }] };
    const edges: readonly FlowEdge[] = [{ id: 'e1', source: 'Node1', target: 'Node2' }];
    const { container, getAllByRole } = renderAgentNode({}, { yamlJsonObject }, edges);
    await waitFor(() => expect(getAllByRole('combobox', { hidden: true }).length).toBeGreaterThan(0));

    const sourceHandle = container.querySelector('.react-flow__handle.source');
    expect(sourceHandle?.className.split(' ')).not.toContain('connectable');
  });

  it('keeps the source handle connectable when the only outgoing edge targets END', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1' }] };
    const edges: readonly FlowEdge[] = [{ id: 'e1', source: 'Node1', target: 'END' }];
    const { container, getAllByRole } = renderAgentNode({}, { yamlJsonObject }, edges);
    await waitFor(() => expect(getAllByRole('combobox', { hidden: true }).length).toBeGreaterThan(0));

    const sourceHandle = container.querySelector('.react-flow__handle.source');
    expect(sourceHandle?.className.split(' ')).toContain('connectable');
  });

  it('disables the toolkit select and input/output pickers when isRunningPipeline is true', async () => {
    const { getAllByRole } = renderAgentNode({}, { isRunningPipeline: true });
    await waitFor(() => {
      const comboboxes = getAllByRole('combobox', { hidden: true });
      expect(comboboxes.length).toBeGreaterThan(0);
      for (const combobox of comboboxes) {
        expect(combobox).toHaveAttribute('aria-disabled', 'true');
      }
    });
  });

  it('does not disable the toolkit select when isRunningPipeline and disabled are both false', async () => {
    const { getAllByRole } = renderAgentNode({}, { isRunningPipeline: false, disabled: false });
    await waitFor(() => {
      const comboboxes = getAllByRole('combobox', { hidden: true });
      expect(comboboxes.length).toBeGreaterThan(0);
      expect(comboboxes[0]).not.toHaveAttribute('aria-disabled', 'true');
    });
  });

  it('marks the node as the pipeline entry point and renders the Trigger selector', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1' }], entry_point: 'Node1' };
    const { getByText, getAllByRole } = renderAgentNode({}, { yamlJsonObject });
    await waitFor(() => expect(getAllByRole('combobox', { hidden: true }).length).toBeGreaterThan(0));
    expect(getByText('Trigger')).toBeInTheDocument();
  });

  it('renders without a versionTools prop at all (defaults to an empty list)', () => {
    expect(() => renderAgentNode({ versionTools: undefined })).not.toThrow();
  });

  it('renders AgentVersionSelect read-only in Basic when onVersionChange is omitted', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', type: 'agent', tool: 'Researcher' }],
    };
    const versionTools = [
      { id: '10', name: 'Researcher', type: 'application', settings: { application_id: 10, application_version_id: 1 } },
    ];
    const { findByTestId } = renderAgentNode({ versionTools }, { yamlJsonObject });
    const versionContainer = await findByTestId('pipeline-agent-node-version-select');
    const combobox = versionContainer.querySelector('[role="combobox"]');
    expect(combobox).toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps Basic and Advanced mapping keys strictly disjoint and deduplicated', () => {
    const withChatHistoryRequired = partitionAgentMappingKeys(
      { task: {}, chat_history: {}, extra_var: {} },
      ['task', 'chat_history', 'task'],
    );
    expect(withChatHistoryRequired.basicKeys).toEqual(['task', 'chat_history']);
    expect(withChatHistoryRequired.advancedKeys).toEqual(['extra_var']);

    const defaultSplit = partitionAgentMappingKeys(
      { task: {}, chat_history: {}, optional_a: {} },
      ['task'],
    );
    expect(defaultSplit.basicKeys).toEqual(['task']);
    expect(defaultSplit.advancedKeys).toEqual(['chat_history', 'optional_a']);
  });
});
