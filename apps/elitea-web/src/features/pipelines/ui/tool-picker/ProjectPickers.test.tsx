import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import { AgentVersionSelect } from './AgentVersionSelect';
import { ProjectAgentPicker } from './ProjectAgentPicker';
import { ProjectToolkitPicker } from './ProjectToolkitPicker';
import { ProjectToolkitsMultiPicker } from './ProjectToolkitsMultiPicker';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
      HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Project pickers and AgentVersionSelect', () => {
  it('ProjectAgentPicker loads grouped agents & pipelines and disables Swarm agents (EL-7085)', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();

    server.use(
      http.get(`${BASE}/elitea_core/applications/prompt_lib/${PROJECT_ID}`, ({ request }) => {
        const url = new URL(request.url);
        const type = url.searchParams.get('agents_type');
        if (type === 'pipeline') {
          return HttpResponse.json({
            total: 1,
            rows: [{ id: 20, name: 'DeployPipe', agent_type: 'pipeline' }],
          });
        }
        return HttpResponse.json({
          total: 2,
          rows: [
            { id: 10, name: 'HelperAgent', agent_type: 'classic' },
            { id: 11, name: 'SwarmAgent', agent_type: 'classic', has_swarm: true },
          ],
        });
      }),
    );

    const { findByTestId, findByText, getAllByTestId } = renderWithRouterAndProject(
      <ProjectAgentPicker
        onSelect={onSelect}
        pickerContext={{ projectId: PROJECT_ID }}
        data-testid="agent-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('agent-picker'));
    expect(await findByText('HelperAgent')).toBeInTheDocument();
    expect(await findByText('DeployPipe')).toBeInTheDocument();

    const options = getAllByTestId('agent-picker-option');
    const swarmOption = options.find((el) => el.textContent?.includes('SwarmAgent'));
    expect(swarmOption).toHaveAttribute('aria-disabled', 'true');

    await user.click(await findByText('HelperAgent'));
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ name: 'HelperAgent' }));
  });

  it('ProjectToolkitPicker loads project toolkits and selects an item', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();

    server.use(
      http.get(`${BASE}/elitea_core/tools/prompt_lib/${PROJECT_ID}`, () =>
        HttpResponse.json({
          total: 1,
          rows: [{ id: 5, type: 'github', name: 'GitHub Main', toolkit_name: 'gh_main' }],
        }),
      ),
    );

    const { findByTestId, findByText } = renderWithRouterAndProject(
      <ProjectToolkitPicker
        onSelect={onSelect}
        pickerContext={{ projectId: PROJECT_ID }}
        data-testid="tk-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('tk-picker'));
    await user.click(await findByText('GitHub Main'));
    expect(onSelect).toHaveBeenCalledWith(
      expect.objectContaining({ id: 5, name: 'GitHub Main' }),
      'gh_main',
    );
  });

  it('ProjectToolkitsMultiPicker toggles toolkits and MCPs while keeping menu open', async () => {
    const user = userEvent.setup();
    const onAdd = vi.fn();
    const onRemove = vi.fn();

    server.use(
      http.get(`${BASE}/elitea_core/tools/prompt_lib/${PROJECT_ID}`, ({ request }) => {
        const url = new URL(request.url);
        const isMcp = url.searchParams.get('mcp') === 'true';
        return HttpResponse.json({
          total: 1,
          rows: isMcp
            ? [{ id: 8, type: 'mcp', name: 'DocsMCP', toolkit_name: 'docs_mcp', meta: { mcp: true } }]
            : [{ id: 5, type: 'github', name: 'GitHub', toolkit_name: 'gh' }],
        });
      }),
    );

    const { findByTestId, findByText } = renderWithRouterAndProject(
      <ProjectToolkitsMultiPicker
        value={['gh']}
        onAdd={onAdd}
        onRemove={onRemove}
        pickerContext={{ projectId: PROJECT_ID }}
        data-testid="multi-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('multi-picker'));
    await user.click(await findByText('DocsMCP'));
    expect(onAdd).toHaveBeenCalledWith(expect.objectContaining({ id: 8 }), 'docs_mcp', true);

    await user.click(await findByText('GitHub'));
    expect(onRemove).toHaveBeenCalledWith('gh');
  });

  it('AgentVersionSelect loads versions and reports missing version error', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/application/prompt_lib/${PROJECT_ID}/42`, () =>
        HttpResponse.json({
          id: 42,
          name: 'Bot',
          versions: [
            { id: 1, name: 'v1', created_at: '2026-01-10T00:00:00Z' },
            { id: 2, name: 'latest', created_at: '2026-02-01T00:00:00Z' },
          ],
        }),
      ),
    );

    const { findByText } = renderWithRouterAndProject(
      <AgentVersionSelect
        projectId={PROJECT_ID}
        tool={{
          type: 'application',
          name: 'Bot',
          settings: { application_id: 42, application_version_id: 999 },
        }}
        data-testid="version-select"
      />,
      PROJECT_ID,
    );

    expect(
      await findByText('This version no longer exists. Select another one.'),
    ).toBeInTheDocument();
  });

  it('ProjectAgentPicker attachedOnly handles attachment & wire shapes without self-disabling or duplicating and filters by search', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();

    const { findByTestId, findByPlaceholderText, getAllByTestId, queryByText } = renderWithRouterAndProject(
      <ProjectAgentPicker
        attachedOnly
        onSelect={onSelect}
        pickerContext={{
          projectId: PROJECT_ID,
          yamlJsonObject: {
            nodes: [{ id: 'n1', type: 'agent', agent_name: 'WireAgent' }],
          },
          attachedTools: [
            {
              id: undefined,
              type: 'application',
              name: 'AttachedApp',
              settings: { application_id: 42 },
            },
            {
              id: '123',
              type: 'application',
              name: 'WireAgent',
            },
            {
              id: '456',
              type: 'application',
              name: 'SubPipe',
              agent_type: 'pipeline',
            },
          ],
        }}
        data-testid="agent-attached-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await findByTestId('agent-attached-picker'));

    const options = getAllByTestId('agent-attached-picker-option');
    expect(options).toHaveLength(3);
    for (const option of options) {
      expect(option).not.toHaveAttribute('aria-disabled', 'true');
    }

    const searchInput = await findByPlaceholderText('Search agents and pipelines...');
    await user.type(searchInput, 'Attached');

    expect(getAllByTestId('agent-attached-picker-option')).toHaveLength(1);
    expect(queryByText('WireAgent')).not.toBeInTheDocument();
    expect(queryByText('SubPipe')).not.toBeInTheDocument();

    await user.click(getAllByTestId('agent-attached-picker-option')[0]!);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ name: 'AttachedApp', id: 42, isAttached: true }));
  });

  it('ProjectToolkitPicker and ProjectToolkitsMultiPicker attachedOnly filter unused attached items by search', async () => {
    const user = userEvent.setup();

    const single = renderWithRouterAndProject(
      <ProjectToolkitPicker
        attachedOnly
        pickerContext={{
          projectId: PROJECT_ID,
          attachedTools: [
            { id: 1, type: 'github', name: 'GitHub Alpha', toolkit_name: 'gh_alpha' },
            { id: 2, type: 'jira', name: 'Jira Beta', toolkit_name: 'jira_beta' },
          ],
        }}
        data-testid="tk-attached-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await single.findByTestId('tk-attached-picker'));
    expect(single.getAllByTestId('tk-attached-picker-option')).toHaveLength(2);
    await user.type(await single.findByPlaceholderText('Search toolkits...'), 'Jira');
    expect(single.getAllByTestId('tk-attached-picker-option')).toHaveLength(1);
    expect(single.queryByText('GitHub Alpha')).not.toBeInTheDocument();
    single.unmount();

    const multi = renderWithRouterAndProject(
      <ProjectToolkitsMultiPicker
        attachedOnly
        pickerContext={{
          projectId: PROJECT_ID,
          attachedTools: [
            { id: 1, type: 'github', name: 'GitHub Alpha', toolkit_name: 'gh_alpha' },
            { id: 2, type: 'mcp', name: 'Docs MCP', toolkit_name: 'docs_mcp', meta: { mcp: true } },
          ],
        }}
        data-testid="multi-attached-picker"
      />,
      PROJECT_ID,
    );

    await user.click(await multi.findByTestId('multi-attached-picker'));
    expect(multi.getAllByTestId('multi-attached-picker-option')).toHaveLength(2);
    await user.type(await multi.findByPlaceholderText('Search toolkits and MCPs...'), 'Docs');
    expect(multi.getAllByTestId('multi-attached-picker-option')).toHaveLength(1);
    expect(multi.queryByText('GitHub Alpha')).not.toBeInTheDocument();
  });
});
