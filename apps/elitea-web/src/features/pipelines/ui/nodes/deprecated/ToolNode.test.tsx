import { fireEvent, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import type { FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import type { YamlPipelineDocument } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { renderDeprecatedNode } from './deprecatedNodeTestUtils';
import { ToolNode } from './ToolNode';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

const versionTools: readonly PipelineToolEntry[] = [
  { type: 'github', toolkit_name: 'my-github', settings: { selected_tools: ['create_issue', 'list_issues'] } },
];

const BASE = '/api/v2';
const PROJECT_ID = 'project-1';
const DISCOVER = `${BASE}/elitea_core/toolkit_discover_tools/prompt_lib/${PROJECT_ID}/:toolkitType`;

function renderToolNode(
  yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'tool-1', type: 'tool', toolkit_name: 'my-github', task: 'do the thing' }] },
  tools: readonly PipelineToolEntry[] = versionTools,
) {
  const setYamlJsonObject = vi.fn();
  const contextValue: FlowEditorContextValue = {
    yamlJsonObject,
    setYamlJsonObject,
    setFlowNodes: vi.fn(),
    setFlowEdges: vi.fn(),
  };

  const result = renderDeprecatedNode(
    'tool-1',
    contextValue,
    <ToolNode
      id="tool-1"
      data={{}}
      versionTools={tools}
    />,
  );

  return { ...result, setYamlJsonObject };
}

describe('ToolNode', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: BASE });
    // The catalogue read (#440) runs for a toolkit without its own
    // `selected_tools`. A per-test `server.use` still wins.
    server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [], total: 0 })));
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('renders the task value', async () => {
    const { findByDisplayValue } = renderToolNode();
    expect(await findByDisplayValue('do the thing')).toBeInTheDocument();
  });

  it('shows a Tool sub-select once the selected toolkit has explicit selected_tools', async () => {
    // Rendered order: ToolSelect (toolkit picker), then the "Tool"
    // SingleSelect -- verified by presence only (not opened): a real
    // `mousedown` on a `Select` nested inside `<ReactFlow>`'s pane also
    // arms react-flow's own pane-level `d3-drag` zoom/pan listener, which
    // throws in jsdom (`d3-drag`'s `nodrag.js` dereferences a null
    // `view.document`) -- the actual option-filtering LOGIC this renders
    // is covered directly, DOM-free, in `useToolNodeEditing.test.ts`.
    const { findByDisplayValue } = renderToolNode();
    await findByDisplayValue('do the thing');
    const comboboxes = document.body.querySelectorAll('[role="combobox"]');
    expect(comboboxes.length).toBe(4);
  });

  it('persists a task edit via updateYamlNode', async () => {
    const { findByDisplayValue, setYamlJsonObject } = renderToolNode();

    const taskField = await findByDisplayValue('do the thing');
    fireEvent.change(taskField, { target: { value: 'do a new thing' } });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const [nextDoc] = setYamlJsonObject.mock.calls[0] as [YamlPipelineDocument];
    const nextNode = nextDoc.nodes?.find(node => node.id === 'tool-1');
    expect(nextNode?.task).toBe('do a new thing');
  });

  /**
   * #440. `functionOptions` came only from an explicit `selected_tools`
   * list, so a toolkit that publishes its tools at run time drew no picker
   * at all — the same screen a lost read drew. The cases below discriminate
   * the three outcomes.
   */
  describe('dynamic tool catalogue (#440)', () => {
    /** A toolkit with no `selected_tools` of its own, so the catalogue tier is the one in use. */
    const dynamicVersionTools: readonly PipelineToolEntry[] = [{ type: 'github', toolkit_name: 'my-github' }];
    const yaml: YamlPipelineDocument = { nodes: [{ id: 'tool-1', type: 'tool', toolkit_name: 'my-github', task: 'do the thing' }] };

    it('shows the Tool select once the backend publishes tools for the selected toolkit', async () => {
      server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [{ id: '1', name: 'alpha_op', type: 'github' }], total: 1 })));

      const { findByDisplayValue, queryByTestId } = renderToolNode(yaml, dynamicVersionTools);

      await findByDisplayValue('do the thing');
      await waitFor(() => expect(document.body.querySelectorAll('[role="combobox"]').length).toBe(4));
      expect(queryByTestId('tool-node-tool-list-error')).not.toBeInTheDocument();
    });

    it('shows an error with a retry, not a missing picker, when the catalogue read fails', async () => {
      server.use(http.post(DISCOVER, () => HttpResponse.json({ error: 'read available tools failed' }, { status: 500 })));

      const { findByTestId, findByDisplayValue } = renderToolNode(yaml, dynamicVersionTools);

      await findByDisplayValue('do the thing');
      expect(await findByTestId('tool-node-tool-list-error')).toBeInTheDocument();
      expect(document.body.querySelectorAll('[role="combobox"]').length).toBe(3);
    });

    it('shows no error and no picker when the read succeeds with no tools', async () => {
      let requestCount = 0;
      server.use(
        http.post(DISCOVER, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [], total: 0 });
        }),
      );

      const { findByDisplayValue, queryByTestId } = renderToolNode(yaml, dynamicVersionTools);

      await findByDisplayValue('do the thing');
      await waitFor(() => expect(requestCount).toBe(1));
      expect(queryByTestId('tool-node-tool-list-error')).not.toBeInTheDocument();
      expect(document.body.querySelectorAll('[role="combobox"]').length).toBe(3);
    });
  });
});
