import { cleanup, fireEvent, waitFor, within, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { BaseToolNode, computeFunctionOptions, type BaseToolNodeProps } from './BaseToolNode';

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
});

afterEach(() => {
  resetGeneratedClient();
  cleanup();
});

const versionTools: readonly PipelineToolEntry[] = [
  { type: 'github', toolkit_name: 'my-github', settings: { selected_tools: ['create_issue'] } },
];

function renderBaseToolNode(
  props: Partial<BaseToolNodeProps>,
  flowEditorOverrides: Partial<FlowEditorContextValue> = {},
): RenderResult {
  const yamlJsonObject: YamlPipelineDocument = flowEditorOverrides.yamlJsonObject ?? {
    nodes: [{ id: 'Node1', toolkit_name: 'my-github' }],
  };
  const flowEditorValue = buildFlowEditorContextValue({ yamlJsonObject, ...flowEditorOverrides });

  const fullProps: BaseToolNodeProps = {
    id: 'Node1',
    nodeType: 'toolkit',
    versionTools,
    ...props,
  };

  return renderWithRouterAndProject(
    <ReactFlowProvider>
      <FlowEditorContext.Provider value={flowEditorValue}>
        <ReactFlow
          nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: fullProps.data ?? {} }]}
          edges={[]}
          nodeTypes={{ testNode: () => <BaseToolNode {...fullProps} /> }}
        />
      </FlowEditorContext.Provider>
    </ReactFlowProvider>,
    PROJECT_ID,
  );
}

const DISCOVER = `${BASE}/elitea_core/toolkit_discover_tools/prompt_lib/${PROJECT_ID}/:toolkitType`;

describe('BaseToolNode', () => {
  describe('with an empty toolkit-type catalogue', () => {
    beforeEach(() => {
      server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})));
      // The catalogue read (#440) runs for a toolkit without its own
      // `selected_tools`. A per-test `server.use` still wins: msw puts
      // run-time handlers ahead of this one.
      server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [], total: 0 })));
    });

    it('renders the node id as the card name and both handles', async () => {
      const { container, getAllByRole } = renderBaseToolNode({});
      await waitFor(() => expect(getAllByRole('combobox', { hidden: true }).length).toBeGreaterThan(0));
      expect(container.querySelectorAll('.react-flow__handle')).toHaveLength(2);
    });

    it('shows the selected toolkit label once toolkit_name resolves to a filtered-in versionTools entry', async () => {
      const { findByText } = renderBaseToolNode({});
      expect(await findByText('my-github')).toBeInTheDocument();
    });

    it('does not render a "Tool" sub-select when the selected toolkit has no explicit selected_tools and no dynamic schema is available', async () => {
      const noSelectionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
      const { getAllByRole, findByText } = renderBaseToolNode({ versionTools: noSelectionTools });
      await findByText('my-github');
      // Toolkit ToolSelect + InputSelect + OutputSelect -- no fourth "Tool" combobox.
      expect(getAllByRole('combobox', { hidden: true })).toHaveLength(3);
    });

    it('renders a "Tool" sub-select once the selected toolkit has explicit selected_tools', async () => {
      const { getAllByRole, findByText } = renderBaseToolNode({});
      await findByText('my-github');
      await waitFor(() => expect(getAllByRole('combobox', { hidden: true })).toHaveLength(4));
    });

    it('renders the Structured output switch only when showStructuredOutput is true', async () => {
      const { findByText, queryByText } = renderBaseToolNode({ showStructuredOutput: true });
      await findByText('my-github');
      expect(queryByText('Structured output')).toBeInTheDocument();
    });

    it('omits the Structured output switch when showStructuredOutput is false (default)', async () => {
      const { findByText, queryByText } = renderBaseToolNode({});
      await findByText('my-github');
      expect(queryByText('Structured output')).not.toBeInTheDocument();
    });

    it('disables the toolkit select and every switch when isRunningPipeline is true', async () => {
      const { getAllByRole, findByText } = renderBaseToolNode({}, { isRunningPipeline: true });
      await findByText('my-github');
      // The toolkit ToolSelect is always the first combobox in DOM order.
      await waitFor(() => expect(getAllByRole('combobox', { hidden: true })[0]).toHaveAttribute('aria-disabled', 'true'));
      for (const switchControl of getAllByRole('switch', { hidden: true })) {
        expect(switchControl).toBeDisabled();
      }
    });

    it('does not disable the toolkit select when isRunningPipeline is false', async () => {
      const { getAllByRole, findByText } = renderBaseToolNode({}, { isRunningPipeline: false });
      await findByText('my-github');
      await waitFor(() => expect(getAllByRole('combobox', { hidden: true })[0]).not.toHaveAttribute('aria-disabled', 'true'));
    });

    it('renders the TriggerTypeSelector when the node is the pipeline entry point', async () => {
      const yamlJsonObject: YamlPipelineDocument = {
        nodes: [{ id: 'Node1', toolkit_name: 'my-github' }],
        entry_point: 'Node1',
      };
      const { findByText, getByText } = renderBaseToolNode({}, { yamlJsonObject });
      await findByText('my-github');
      expect(getByText('Trigger')).toBeInTheDocument();
    });

    it('does not render the TriggerTypeSelector when the node is not the entry point', async () => {
      const { findByText, queryByText } = renderBaseToolNode({});
      await findByText('my-github');
      expect(queryByText('Trigger')).not.toBeInTheDocument();
    });
  });

  describe('with a toolkit-type catalogue exposing static selected_tools schemas', () => {
    beforeEach(() => {
      server.use(
        http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () =>
          HttpResponse.json({
            github: {
              properties: {
                selected_tools: { args_schemas: { create_issue: {}, list_issues: {} } },
              },
            },
          }),
        ),
      );
    });

    it('intersects an explicit selected_tools list against the schema-available tool names', async () => {
      // 'list_issues' is NOT in this node's own explicit selection (only
      // 'create_issue' is), and 'delete_issue' is in neither -- the
      // rendered "Tool" combobox must still appear (a real intersection,
      // not an empty result) because 'create_issue' survives the filter.
      const { getAllByRole, findByText } = renderBaseToolNode({});
      await findByText('my-github');
      await waitFor(() => expect(getAllByRole('combobox', { hidden: true })).toHaveLength(4));
    });
  });
});

/**
 * Direct unit coverage for `computeFunctionOptions` (confirmed
 * adversarial-review finding #3, `BaseToolNode.tsx:61`) -- exercised here
 * rather than through `<BaseToolNode>` because the object-shaped
 * `selected_tools` entry this fix normalizes cannot be expressed through
 * `BaseToolNodeProps.versionTools: readonly PipelineToolEntry[]` without an
 * unsafe cast: `PipelineToolEntry.settings.selected_tools` (`../select/
 * pipelineToolEntry.types.ts`, out of this cluster's scope) is typed
 * `readonly string[]`, narrower than the real runtime shape (see
 * `computeFunctionOptions`'s own "ROUTING NOTE"). `computeFunctionOptions`'s
 * own local parameter type is loose (`Readonly<Record<string, unknown>>`),
 * so no cast is needed to call it directly with an object-shaped entry.
 */
describe('computeFunctionOptions', () => {
  const noAvailableTools = (): readonly string[] => [];

  it('normalizes an object-shaped selected_tools entry via getToolName when no schema-derived availableTools exist (explicit-only branch)', () => {
    const selectedToolkit = {
      type: 'github',
      settings: { selected_tools: [{ name: 'create_issue', description: 'Create an issue', path: '/issues' }] },
    };

    const result = computeFunctionOptions(selectedToolkit, noAvailableTools, []);

    // Previously: `{ label: item, value: item }` with `item` the raw
    // object -- rendered as the literal string "[object Object]".
    expect(result).toEqual([{ label: 'create_issue', value: 'create_issue' }]);
  });

  it('normalizes an object-shaped selected_tools entry via getToolName in the schema-intersected branch, instead of dropping it', () => {
    const selectedToolkit = {
      type: 'github',
      settings: { selected_tools: [{ name: 'create_issue', description: 'Create an issue', path: '/issues' }] },
    };
    const getSelectedTools = (): readonly string[] => ['create_issue', 'list_issues'];

    const result = computeFunctionOptions(selectedToolkit, getSelectedTools, []);

    // Previously: `availableTools.includes(tool)` compared the raw object
    // against a string array and never matched -- the entry was silently
    // dropped from the intersected list.
    expect(result).toEqual([{ label: 'create_issue', value: 'create_issue' }]);
  });

  it('still drops an object-shaped entry whose normalized name is absent from the schema-derived availableTools', () => {
    const selectedToolkit = {
      type: 'github',
      settings: { selected_tools: [{ name: 'delete_issue' }] },
    };
    const getSelectedTools = (): readonly string[] => ['create_issue', 'list_issues'];

    expect(computeFunctionOptions(selectedToolkit, getSelectedTools, [])).toEqual([]);
  });

  it('keeps working for plain string entries (no regression to the existing string path)', () => {
    const selectedToolkit = { type: 'github', settings: { selected_tools: ['list_issues', 'create_issue'] } };

    const result = computeFunctionOptions(selectedToolkit, noAvailableTools, []);

    expect(result).toEqual([
      { label: 'create_issue', value: 'create_issue' },
      { label: 'list_issues', value: 'list_issues' },
    ]);
  });
});

/**
 * #440. The node used to render nothing when the option list was empty,
 * whatever the cause, so a toolkit with no tools and a lost read drew the
 * same screen. The cases below discriminate the three outcomes.
 */
describe('BaseToolNode dynamic tool catalogue (#440)', () => {
  /** A toolkit with no `selected_tools` of its own, so the catalogue tier is the one in use. */
  const dynamicVersionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];

  beforeEach(() => {
    server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})));
  });

  it('lists the tools the backend publishes for the selected toolkit', async () => {
    server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [{ id: '1', name: 'alpha_op', type: 'github' }], total: 1 })));

    const { getAllByRole, findByText, queryByTestId } = renderBaseToolNode({ versionTools: dynamicVersionTools });

    await findByText('my-github');
    // Toolkit ToolSelect + the "Tool" select + InputSelect + OutputSelect.
    await waitFor(() => expect(getAllByRole('combobox', { hidden: true })).toHaveLength(4));
    expect(queryByTestId('tool-list-error')).not.toBeInTheDocument();
  });

  it('shows an error with a retry, not an empty node, when the catalogue read fails', async () => {
    let schemaReads = 0;
    let catalogueReads = 0;
    server.use(
      http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => {
        schemaReads += 1;
        return HttpResponse.json({});
      }),
    );
    server.use(
      http.post(DISCOVER, () => {
        catalogueReads += 1;
        return HttpResponse.json({ error: 'read available tools failed' }, { status: 500 });
      }),
    );

    const { findByTestId, findByText, getAllByRole } = renderBaseToolNode({ versionTools: dynamicVersionTools });

    await findByText('my-github');
    const alert = await findByTestId('tool-list-error');
    expect(alert).toBeInTheDocument();
    // No "Tool" select stands in for the failure.
    expect(getAllByRole('combobox', { hidden: true })).toHaveLength(3);

    // The retry runs BOTH reads that feed this picker.
    const beforeSchemaReads = schemaReads;
    const beforeCatalogueReads = catalogueReads;
    const retry = within(alert).getByText('Retry');
    fireEvent.click(retry);
    await waitFor(() => expect(catalogueReads).toBe(beforeCatalogueReads + 1));
    await waitFor(() => expect(schemaReads).toBe(beforeSchemaReads + 1));
  });

  it('shows no error and no picker when the read succeeds with no tools', async () => {
    let requestCount = 0;
    server.use(
      http.post(DISCOVER, () => {
        requestCount += 1;
        return HttpResponse.json({ tools: [], total: 0 });
      }),
    );

    const { findByText, getAllByRole, queryByTestId } = renderBaseToolNode({ versionTools: dynamicVersionTools });

    await findByText('my-github');
    await waitFor(() => expect(requestCount).toBe(1));
    expect(queryByTestId('tool-list-error')).not.toBeInTheDocument();
    expect(getAllByRole('combobox', { hidden: true })).toHaveLength(3);
  });

  it('shows an error when the toolkit type schema read fails and leaves the picker empty', async () => {
    server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({ error: 'schemas unavailable' }, { status: 500 })));
    server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [], total: 0 })));

    const { findByTestId } = renderBaseToolNode({ versionTools: dynamicVersionTools });

    expect(await findByTestId('tool-list-error')).toBeInTheDocument();
  });
});
