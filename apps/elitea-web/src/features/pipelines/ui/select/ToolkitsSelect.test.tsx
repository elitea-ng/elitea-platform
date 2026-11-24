import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { buildFlowEditorContextValue, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { ToolkitsSelect } from './ToolkitsSelect';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
      HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true }),
    ),
    http.get(`${BASE}/elitea_core/tools/prompt_lib/${PROJECT_ID}`, ({ request }) => {
      const url = new URL(request.url);
      if (url.searchParams.get('mcp') === 'true') {
        return HttpResponse.json({ rows: [], total: 0 });
      }
      return HttpResponse.json({
        rows: [{ id: 1, type: 'github', name: 'github', toolkit_name: 'github' }],
        total: 1,
      });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ToolkitsSelect', () => {
  it('renders the grouped Toolkits picker label', async () => {
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'LLM 1' }] } });

    const { findByText } = renderWithRouterAndProject(
      <FlowEditorContext.Provider value={contextValue}>
        <ToolkitsSelect
          id="LLM 1"
          versionTools={[{ type: 'github', toolkit_name: 'github' }]}
        />
      </FlowEditorContext.Provider>,
      PROJECT_ID,
    );

    expect(await findByText('Toolkits')).toBeInTheDocument();
  });

  it('excludes application-type tools from the grouped picker menu', async () => {
    const user = userEvent.setup();
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'LLM 1' }] } });

    const { findByTestId, queryByText } = renderWithRouterAndProject(
      <FlowEditorContext.Provider value={contextValue}>
        <ToolkitsSelect
          id="LLM 1"
          versionTools={[{ type: 'application', name: 'My Agent' }]}
        />
      </FlowEditorContext.Provider>,
      PROJECT_ID,
    );

    await user.click(await findByTestId('pipeline-llm-toolkits-picker'));
    expect(queryByText('My Agent')).not.toBeInTheDocument();
  });

  it('reads the current selection from tool_names keys', async () => {
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'LLM 1', tool_names: { github: [] } }] },
    });

    const { findByText } = renderWithRouterAndProject(
      <FlowEditorContext.Provider value={contextValue}>
        <ToolkitsSelect
          id="LLM 1"
          versionTools={[{ type: 'github', toolkit_name: 'github' }]}
        />
      </FlowEditorContext.Provider>,
      PROJECT_ID,
    );

    expect(await findByText('github')).toBeInTheDocument();
  });

  it('populates tool_names with the toolkit static tool list on selection', async () => {
    const user = userEvent.setup();
    const setYamlJsonObject = vi.fn();
    const contextValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'LLM 1' }] },
      setYamlJsonObject,
    });

    const { findByTestId, findByText } = renderWithRouterAndProject(
      <FlowEditorContext.Provider value={contextValue}>
        <ToolkitsSelect
          id="LLM 1"
          versionTools={[{ type: 'github', name: 'github', toolkit_name: 'github', tools: ['create_issue', 'list_issues'] }]}
        />
      </FlowEditorContext.Provider>,
      PROJECT_ID,
    );

    await user.click(await findByTestId('pipeline-llm-toolkits-picker'));
    await user.click(await findByText('github'));

    expect(setYamlJsonObject).toHaveBeenCalledWith(
      expect.objectContaining({
        nodes: [expect.objectContaining({ id: 'LLM 1', tool_names: { github: ['create_issue', 'list_issues'] } })],
      }),
    );
  });

  it('calls the onValueChange callback when provided', async () => {
    const user = userEvent.setup();
    const onValueChange = vi.fn();
    const contextValue = buildFlowEditorContextValue({ yamlJsonObject: { nodes: [{ id: 'LLM 1' }] } });

    const { findByTestId, findByText } = renderWithRouterAndProject(
      <FlowEditorContext.Provider value={contextValue}>
        <ToolkitsSelect
          id="LLM 1"
          versionTools={[{ type: 'github', name: 'github', toolkit_name: 'github', tools: [] }]}
          onValueChange={onValueChange}
        />
      </FlowEditorContext.Provider>,
      PROJECT_ID,
    );

    await user.click(await findByTestId('pipeline-llm-toolkits-picker'));
    await user.click(await findByText('github'));

    expect(onValueChange).toHaveBeenCalledWith(['github']);
  });
});
