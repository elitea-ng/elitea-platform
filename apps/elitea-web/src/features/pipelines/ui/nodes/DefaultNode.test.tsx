import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, waitFor, within, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { DefaultNode } from './DefaultNode';

const BASE = '/api/v2';
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
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () => HttpResponse.json({ mcp_enabled: true })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  cleanup();
});

function renderDefaultNode(
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  versionTools?: readonly PipelineToolEntry[],
): RenderResult {
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
              <DefaultNode
                {...props}
                type="defaultType"
                {...(versionTools !== undefined ? { versionTools } : {})}
              />
            ),
          }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

/**
 * Mounts `DefaultNode` directly (not via `nodeTypes`/an actual `<ReactFlow>`
 * node) -- needed only for the toolkit-selection interaction tests below,
 * which click into `ToolSelect`'s own `SingleSelect` (`../select/
 * ToolSelect.tsx`, unit A2h, out of this group's scope to fix): that select
 * has no `nopan nodrag` shield, so a mousedown on it bubbles into React
 * Flow's real draggable node wrapper and throws out of `d3-drag` when
 * mounted inside an actual `<ReactFlow>` tree -- reproduced empirically the
 * same way `PrinterNode.test.tsx`'s identical helper doc comment covers.
 * `CustomHandle` degrades gracefully with no real node id in context (logs
 * React Flow's internal error code 010, does not throw).
 */
function renderDefaultNodeBare(
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
  versionTools?: readonly PipelineToolEntry[],
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? { nodes: [{ id: 'Node1' }] };
  const flowEditorValue = buildFlowEditorContextValue({ ...flowEditorOverrides, yamlJsonObject });

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <DefaultNode
          id="Node1"
          type="defaultType"
          {...(versionTools !== undefined ? { versionTools } : {})}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

describe('DefaultNode', () => {
  it('renders the node id and both handles (target + source)', async () => {
    const { findByText, container } = renderDefaultNode();
    await findByText('Node1');

    expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
  });

  it('renders the Toolkit, Input, and Output selects, plus interrupt settings', async () => {
    const { findByText, getByText } = renderDefaultNode();
    await findByText('Node1');

    expect(getByText('Toolkit')).toBeInTheDocument();
    expect(getByText('Input')).toBeInTheDocument();
    expect(getByText('Output')).toBeInTheDocument();
    expect(getByText('Interrupt before')).toBeInTheDocument();
    expect(getByText('Interrupt after')).toBeInTheDocument();
    expect(getByText('Structured output')).toBeInTheDocument();
  });

  it('renders no Tool (function) select when no toolkit is selected', async () => {
    const { findByText, queryByText } = renderDefaultNode();
    await findByText('Node1');

    expect(queryByText('Tool')).not.toBeInTheDocument();
  });

  it('renders the Tool (function) select, sorted by label, once a toolkit with settings.selected_tools is chosen', async () => {
    const versionTools: readonly PipelineToolEntry[] = [
      {
        type: 'github',
        name: 'github',
        toolkit_name: 'github',
        settings: { selected_tools: ['list_issues', 'create_issue'] },
      },
    ];
    const { findByText, getByText, getAllByRole, getByRole } = renderDefaultNode(
      { yamlJsonObject: { nodes: [{ id: 'Node1', toolkit_name: 'github' }] } },
      versionTools,
    );
    await findByText('Node1');

    expect(getByText('Tool')).toBeInTheDocument();

    // `computeDefaultNodeFunctionOptions` sorts alphabetically -- opening the
    // (now `nopan nodrag`-wrapped) Tool select confirms 'create_issue' sorts
    // before 'list_issues'. Combobox order: [0] Toolkit, [1] Tool.
    const user = userEvent.setup();
    await user.click(getAllByRole('combobox', { hidden: true })[1] as HTMLElement);
    const options = getByRole('listbox', { hidden: true });
    const optionLabels = within(options)
      .getAllByRole('option')
      .map(option => option.textContent);
    expect(optionLabels).toEqual(['create_issue', 'list_issues']);
  });

  it('mounts the CustomNodeInput JSON editor', async () => {
    const { findByText, container } = renderDefaultNode();
    await findByText('Node1');

    await waitFor(() => expect(container.querySelector('.cm-content')).toBeInTheDocument());
  });

  it('selecting an application-type toolkit writes tool (not toolkit_name) and clears any prior toolkit_name', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'application', name: 'My Agent' }];

    const { findByText, getAllByRole, getByRole } = renderDefaultNodeBare(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }] }, setYamlJsonObject },
      versionTools,
    );
    await findByText('Toolkit');

    // Combobox order: [0] Toolkit (ToolSelect), [1] Input, [2] Output.
    await user.click(getAllByRole('combobox', { hidden: true })[0] as HTMLElement);
    await user.click(getByRole('option', { name: 'My Agent', hidden: true }));

    expect(setYamlJsonObject).toHaveBeenLastCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', tool: 'My Agent' })],
      }),
    );
    const [nextDoc] = setYamlJsonObject.mock.lastCall as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).not.toHaveProperty('toolkit_name');
  });

  it('selecting a non-application toolkit writes toolkit_name (not tool)', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', name: 'github', toolkit_name: 'github' }];

    const { findByText, getAllByRole, getByRole } = renderDefaultNodeBare(
      { yamlJsonObject: { nodes: [{ id: 'Node1' }] }, setYamlJsonObject },
      versionTools,
    );
    await findByText('Toolkit');

    // Combobox order: [0] Toolkit (ToolSelect), [1] Input, [2] Output.
    await user.click(getAllByRole('combobox', { hidden: true })[0] as HTMLElement);
    await user.click(getByRole('option', { name: 'github', hidden: true }));

    expect(setYamlJsonObject).toHaveBeenLastCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'Node1', toolkit_name: 'github' })],
      }),
    );
    const [nextDoc] = setYamlJsonObject.mock.lastCall as [YamlPipelineDocument];
    expect(nextDoc.nodes?.[0]).not.toHaveProperty('tool');
  });

  it('clearing the toolkit selection resets toolkit_name/tool/input_mapping', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', name: 'github', toolkit_name: 'github' }];

    const { findByText, getAllByRole } = renderDefaultNodeBare(
      { yamlJsonObject: { nodes: [{ id: 'Node1', toolkit_name: 'github' }] }, setYamlJsonObject },
      versionTools,
    );
    await findByText('Toolkit');

    const combobox = getAllByRole('combobox', { hidden: true })[0] as HTMLElement;
    await user.click(combobox);
    // Re-selecting the already-selected option triggers `onClear` (`ToolSelect.test.tsx`'s own established pattern).
    await user.click(document.querySelector('[data-value="github"]') ?? combobox);

    expect(setYamlJsonObject).toHaveBeenLastCalledWith(
      expect.objectContaining({
        nodes: [{ id: 'Node1' }],
      }),
    );
  });

  it("does not throw with no FlowEditorContext ancestor -- applyDefaultNodeToolkitSelection's own yamlJsonObject/setYamlJsonObject guard exists for exactly this case, even though NodeCard itself then renders null", async () => {
    const versionTools: readonly PipelineToolEntry[] = [{ type: 'github', name: 'github', toolkit_name: 'github' }];

    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{
            testNode: props => (
              <DefaultNode
                {...props}
                type="defaultType"
                {...(versionTools !== undefined ? { versionTools } : {})}
              />
            ),
          }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });

  it('is marked as the entry point when yamlJsonObject.entry_point matches this node id (renders the trigger selector, no crash)', async () => {
    const { findByText, getByText } = renderDefaultNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }], entry_point: 'Node1' },
    });

    expect(await findByText('Trigger')).toBeInTheDocument();
    expect(getByText('Toolkit')).toBeInTheDocument();
  });

  it('disables every combobox and switch while the pipeline is running', async () => {
    const { findByText, getAllByRole } = renderDefaultNode({
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
    const { findByText, getAllByRole } = renderDefaultNode({
      yamlJsonObject: { nodes: [{ id: 'Node1' }] },
      disabled: true,
    });
    await findByText('Node1');

    for (const switchControl of getAllByRole('switch', { hidden: true })) {
      expect(switchControl).toBeDisabled();
    }
  });

  it('does not throw with no FlowEditorContext ancestor (NodeCard renders null)', async () => {
    const { container } = renderWithRouterAndProject(
      <ReactFlowProvider>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
          edges={[]}
          nodeTypes={{ testNode: props => <DefaultNode {...props} type="defaultType" /> }}
        />
      </ReactFlowProvider>,
      PROJECT_ID,
    );
    await waitFor(() => expect(container.querySelector('.react-flow')).toBeInTheDocument());

    expect(container.querySelector('.react-flow__handle')).not.toBeInTheDocument();
  });
});
