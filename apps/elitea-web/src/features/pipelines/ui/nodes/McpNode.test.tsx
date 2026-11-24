import { cleanup, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { McpNode, type McpNodeProps } from './McpNode';

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

function renderMcpNode(props: Partial<McpNodeProps>, flowEditorOverrides: Partial<FlowEditorContextValue> = {}): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [{ id: 'Node1', toolkit_name: 'my-mcp' }],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });
  const fullProps: McpNodeProps = { id: 'Node1', ...props };

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: fullProps.data ?? {} }]}
          edges={[]}
          nodeTypes={{
            testNode: () => (
              <>
                <McpNode {...fullProps} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <McpNode {...fullProps} />
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

describe('McpNode', () => {
  it('shows the "Structured output" switch (showStructuredOutput is hard-coded true)', async () => {
    const { findByText } = renderMcpNode({});
    expect(await findByText('Structured output')).toBeInTheDocument();
  });

  it("filters out an 'application' typed versionTools entry -- its label never shows as the selected toolkit", async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'MyAgentApp' }];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MyAgentApp' }] };
    const { findByText, getByTestId } = renderMcpNode({ versionTools }, { yamlJsonObject });
    await findByText('Structured output');
    expect(getByTestId('pipeline-mcp-node-picker')).not.toHaveTextContent('MyAgentApp');
  });

  it("keeps a non-application entry whose meta.mcp is true -- its label shows as the selected toolkit (unlike ToolkitNode, which excludes it)", async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      { type: 'custom_mcp', toolkit_name: 'my-mcp-tool', meta: { mcp: true } },
    ];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'my-mcp-tool' }] };
    const { findByTestId } = renderMcpNode({ versionTools }, { yamlJsonObject });
    expect(await findByTestId('pipeline-mcp-node-picker')).toHaveTextContent('my-mcp-tool');
  });

  it("keeps a non-application entry whose type equals the Mcp node type -- its label shows as the selected toolkit", async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'mcp', toolkit_name: 'raw-mcp-type' }];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'raw-mcp-type' }] };
    const { findByTestId } = renderMcpNode({ versionTools }, { yamlJsonObject });
    expect(await findByTestId('pipeline-mcp-node-picker')).toHaveTextContent('raw-mcp-type');
  });

  it('filters out a plain non-mcp toolkit entry -- its label never shows as the selected toolkit', async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'my-github' }] };
    const { findByText, getByTestId } = renderMcpNode({ versionTools }, { yamlJsonObject });
    await findByText('Structured output');
    expect(getByTestId('pipeline-mcp-node-picker')).not.toHaveTextContent('my-github');
  });

  it('forwards selected/data through to the underlying BaseToolNode without throwing', () => {
    expect(() => renderMcpNode({ selected: true, data: { isPerforming: true } })).not.toThrow();
  });
});
