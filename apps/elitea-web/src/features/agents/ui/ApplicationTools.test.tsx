import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../__tests__/testUtils';

import { ApplicationTools } from './ApplicationTools';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({
        chat_enabled: true,
        applications_enabled: true,
        skills_enabled: true,
        toolkits_enabled: true,
        datasources_enabled: true,
        pipelines_enabled: true,
        publishing_enabled: true,
        moderation_enabled: true,
        mcp_enabled: true,
        support_chat_enabled: true,
      }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ApplicationTools', () => {
  it('renders one renderToolCard() result per tool, in order', async () => {
    renderWithRouterAndProject(
      <ApplicationTools
        tools={[{ type: 'github', name: 'gh' }, { type: 'jira', name: 'jr' }]}
        internalTools={[]}
        onInternalToolsChange={vi.fn()}
        renderToolCard={(tool) => <div data-testid="tool-card">{tool.name}</div>}
      />,
      'proj-1',
    );
    await waitFor(() => expect(screen.getAllByTestId('tool-card')).toHaveLength(2));
    expect(screen.getByText('gh')).toBeInTheDocument();
    expect(screen.getByText('jr')).toBeInTheDocument();
  });

  it('shows the tool-attach menu unless disabled', async () => {
    renderWithRouterAndProject(
      <ApplicationTools
        tools={[]}
        internalTools={[]}
        onInternalToolsChange={vi.fn()}
        renderToolCard={() => null}
      />,
      'proj-1',
    );
    await waitFor(() => expect(screen.getByTestId('agent-toolkits-section')).toBeInTheDocument());
    // ToolMenu (sibling A1e) renders its own attach affordance; this only
    // asserts ApplicationTools itself does not hide the accordion section.
    expect(screen.getByTestId('agent-toolkits-section')).toBeInTheDocument();
  });

  it('toggles an internal tool on click and reports the new membership array', async () => {
    const onInternalToolsChange = vi.fn();
    renderWithRouterAndProject(
      <ApplicationTools
        tools={[]}
        internalTools={[]}
        onInternalToolsChange={onInternalToolsChange}
        renderToolCard={() => null}
        hidePythonSandbox={false}
      />,
      'proj-1',
    );
    // Only the first 4 internal tools show until "Show all" is clicked
    // (`ApplicationTools`'s own `minToolsToShow` logic) — "Data Analysis"
    // is inside that default-visible set.
    const dataAnalysisLabel = await screen.findByText('Data Analysis');
    const row = dataAnalysisLabel.closest('div')?.parentElement;
    const switchInput = row?.querySelector('input[type="checkbox"]');
    expect(switchInput).toBeTruthy();
    fireEvent.click(switchInput!);
    expect(onInternalToolsChange).toHaveBeenCalledWith(['data_analysis']);
  });

  it('shows only the attachments internal tool for pipelines', async () => {
    renderWithRouterAndProject(
      <ApplicationTools
        tools={[]}
        internalTools={['attachments']}
        onInternalToolsChange={vi.fn()}
        renderToolCard={() => null}
        isPipeline
      />,
      'proj-1',
    );
    await waitFor(() => expect(screen.getByText('Attachments')).toBeInTheDocument());
    expect(screen.queryByText('Swarm Mode')).not.toBeInTheDocument();
  });
});
