import { cleanup, waitFor, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { SubgraphNode, type SubgraphNodeProps } from './SubgraphNode';

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
  // Empty toolkit-type catalogue -- SubgraphNode's own toolkit-name resolution
  // is not under test here, only the isRunningPipeline/disabled -> ToolSelect/
  // CommonInterruptSettings `disabled` wiring.
  server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})));
});

afterEach(() => {
  resetGeneratedClient();
  cleanup();
});

function renderSubgraphNode(
  flowEditorOverrides: Partial<FlowEditorContextValue>,
  nodeProps: Partial<SubgraphNodeProps> = {},
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });
  const fullProps: SubgraphNodeProps = { id: 'Node1', ...nodeProps } as SubgraphNodeProps;

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{
            testNode: () => (
              <>
                <SubgraphNode {...fullProps} />
                <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: true }}>
                  <SubgraphNode {...fullProps} />
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

describe('SubgraphNode', () => {
  // Reproduces the confirmed HIGH finding: `isRunningPipeline ?? disabled`
  // (bug) evaluates to `false` (not disabled) whenever `isRunningPipeline`
  // is the real, non-optional boolean `false` -- `??` only falls through to
  // `disabled` for `null`/`undefined`. `isRunningPipeline || disabled`
  // (fixed, matching baseline `SubgraphNode.jsx:96,102`) correctly disables
  // in this exact combination.
  it('disables the toolkit select and interrupt-settings switches when isRunningPipeline is false and disabled is true', async () => {
    const { getByRole, getAllByRole } = renderSubgraphNode({ isRunningPipeline: false, disabled: true });

    // `hidden: true` -- React Flow renders every node with inline
    // `visibility: hidden` until its own `ResizeObserver` measurement fires
    // (the test env's `ResizeObserver` stub above never calls back, matching
    // this slice's other node tests, e.g. `EndNode.test.tsx`), which
    // `getByRole`'s default accessibility filtering otherwise treats as
    // "not queryable" even though the element is fully present.
    await waitFor(() => expect(getByRole('combobox', { hidden: true })).toHaveAttribute('aria-disabled', 'true'));

    const switches = getAllByRole('switch', { hidden: true });
    expect(switches.length).toBeGreaterThan(0);
    for (const switchControl of switches) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('does not disable the toolkit select when isRunningPipeline and disabled are both false', async () => {
    const { getByRole } = renderSubgraphNode({ isRunningPipeline: false, disabled: false });

    await waitFor(() => expect(getByRole('combobox', { hidden: true })).not.toHaveAttribute('aria-disabled', 'true'));

    /*
     * The two interrupt switches are permanently disabled — an intersection
     * choice, not a per-node state: the native Rust runtime refuses any
     * pipeline declaring static interrupts (`compiler.rs:470-474`) while the
     * SDK worker honours them, so the editor authors only what both accept.
     * See `ui/settings/CommonInterruptSettings.tsx`. Only the switches that
     * really do track `disabled` are asserted here — and a Subgraph node
     * passes `showStructuredOutput={false}`, so it has none. The combobox
     * assertion above is this test's whole subject.
     */
  });

  it('keeps an Application-typed, Pipeline-agent_type versionTools entry -- its label shows as the selected toolkit', async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      { type: 'application', name: 'MySubgraph', agent_type: 'pipeline' },
    ];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MySubgraph' }] };
    const { findByText } = renderSubgraphNode({ yamlJsonObject }, { versionTools });
    expect(await findByText('MySubgraph')).toBeInTheDocument();
  });

  it("filters out an Application-typed entry whose agent_type is NOT 'pipeline'", async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      { type: 'application', name: 'MyAgent', agent_type: 'agent' },
    ];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'MyAgent' }] };
    const { findByRole, queryByText } = renderSubgraphNode({ yamlJsonObject }, { versionTools });
    await findByRole('combobox', { hidden: true });
    expect(queryByText('MyAgent')).not.toBeInTheDocument();
  });

  it('filters out a non-Application-typed versionTools entry even with agent_type set to pipeline', async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      { type: 'github', toolkit_name: 'my-github', agent_type: 'pipeline' },
    ];
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'my-github' }] };
    const { findByRole, queryByText } = renderSubgraphNode({ yamlJsonObject }, { versionTools });
    await findByRole('combobox', { hidden: true });
    expect(queryByText('my-github')).not.toBeInTheDocument();
  });
});
