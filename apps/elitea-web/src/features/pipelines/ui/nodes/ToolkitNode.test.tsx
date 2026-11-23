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
import { ToolkitNode, type ToolkitNodeProps } from './ToolkitNode';

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

function renderToolkitNode(props: Partial<ToolkitNodeProps>, flowEditorOverrides: Partial<FlowEditorContextValue> = {}): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [{ id: 'Node1', toolkit_name: 'my-github' }],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });
  const fullProps: ToolkitNodeProps = { id: 'Node1', ...props };

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: fullProps.data ?? {} }]}
          edges={[]}
          nodeTypes={{
            testNode: () => (
              <>
                <ToolkitNode {...fullProps} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: false }}>
                  <ToolkitNode {...fullProps} />
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

describe('ToolkitNode', () => {
  it('shows the "Structured output" switch (showStructuredOutput is hard-coded true, unlike ToolNode/FunctionNode)', async () => {
    const { findByText } = renderToolkitNode({});
    expect(await findByText('Structured output')).toBeInTheDocument();
  });

  it("filters out an 'application' typed versionTools entry -- its label never shows as the selected toolkit", async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'MyAgentApp' }];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MyAgentApp' }] };
    const { findByText, getByTestId } = renderToolkitNode({ versionTools }, { yamlJsonObject });
    await findByText('Structured output');
    expect(getByTestId('pipeline-toolkit-node-picker')).not.toHaveTextContent('MyAgentApp');
  });

  it("filters out an mcp-meta versionTools entry -- its label never shows as the selected toolkit", async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      { type: 'custom_mcp', toolkit_name: 'my-mcp-tool', meta: { mcp: true } },
    ];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'my-mcp-tool' }] };
    const { findByText, getByTestId } = renderToolkitNode({ versionTools }, { yamlJsonObject });
    await findByText('Structured output');
    expect(getByTestId('pipeline-toolkit-node-picker')).not.toHaveTextContent('my-mcp-tool');
  });

  it('keeps a plain (non-application, non-mcp) toolkit entry -- its label shows as the selected toolkit', async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const { findByText } = renderToolkitNode({ versionTools });
    expect(await findByText('my-github')).toBeInTheDocument();
  });

  it('forwards selected/data through to the underlying BaseToolNode without throwing', () => {
    expect(() => renderToolkitNode({ selected: true, data: { isPerforming: true } })).not.toThrow();
  });
});
