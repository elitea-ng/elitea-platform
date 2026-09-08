import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { configure, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../__tests__/testUtils';

import { TestToolPane } from './TestToolPane';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const RUN_PATH = '/api/v2/elitea_core/test_tool/prompt_lib/:projectId/:toolkitId';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** The type catalogue the picker's static tier and the argument form both read. */
const GITHUB_SCHEMA = {
  github: {
    properties: {
      selected_tools: {
        args_schemas: {
          list_branches_in_repo: {
            type: 'object',
            properties: { repository: { type: 'string', title: 'Repository' } },
            required: ['repository'],
          },
        },
      },
    },
  },
};

function renderWithHarness(ui: ReactElement) {
  function RootComponent() {
    return (
      <QueryClientProvider client={createTestQueryClient()}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <SocketClientContext.Provider value={createTestSocketClient()}>{ui}</SocketClientContext.Provider>
        </ThemeProvider>
      </QueryClientProvider>
    );
  }
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => '7' } },
  });
  return render(<RouterProvider router={router} />);
}

function renderPane() {
  return renderWithHarness(
    <TestToolPane
      projectId="7"
      toolkitId="tk-1"
      values={{ type: 'github', settings: { selected_tools: ['list_branches_in_repo'] } }}
    />,
  );
}

/** Opens the Tool picker and chooses the one tool the fixture offers. */
async function pickTheTool(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  const picker = await screen.findByRole('combobox');
  await user.click(picker);
  await user.click(await screen.findByRole('option', { name: /list branches in repo/i }));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json(GITHUB_SCHEMA)));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('TestToolPane', () => {
  it('keeps the testid the toolkit editor’s configuration tab is waited on by', async () => {
    renderPane();
    expect(await screen.findByTestId('edit-toolkit-test-pane-slot')).toBeInTheDocument();
  });

  it('offers the toolkit’s own tools, draws the picked tool’s argument form, and runs it', async () => {
    let seen: unknown;
    server.use(
      http.post(RUN_PATH, async ({ request, params }) => {
        seen = { body: await request.json(), params };
        return HttpResponse.json({ ok: true, result: { branches: ['main'] }, truncated: false });
      }),
    );

    const user = userEvent.setup();
    renderPane();
    await pickTheTool(user);

    // The argument form is built from the TOOL's own schema, not the toolkit's.
    const repository = await screen.findByLabelText(/repository/i);
    await user.type(repository, 'octo/repo');

    await user.click(screen.getByRole('button', { name: /run tool/i }));

    await waitFor(() => {
      expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'ok');
    });
    expect(seen).toEqual({
      body: { tool_name: 'list_branches_in_repo', tool_params: { repository: 'octo/repo' } },
      params: { projectId: '7', toolkitId: 'tk-1' },
    });
    expect(screen.getByTestId('test-tool-result-payload')).toHaveTextContent('main');
  });

  it('does not offer Run until the tool’s required arguments are filled', async () => {
    const user = userEvent.setup();
    renderPane();
    await pickTheTool(user);

    // `repository` is required and empty: pressing Run here would send a
    // request the tool must refuse, and the panel would report the tool's
    // complaint as though the platform had caused it.
    expect(await screen.findByRole('button', { name: /run tool/i })).toBeDisabled();
  });

  it('shows the server’s own refusal rather than a generic failure', async () => {
    server.use(
      http.post(RUN_PATH, () =>
        HttpResponse.json({ ok: false, reason: 'unsupported_toolkit', error: 'this deployment cannot build a github toolkit' }, { status: 422 }),
      ),
    );

    const user = userEvent.setup();
    renderPane();
    await pickTheTool(user);
    await user.type(await screen.findByLabelText(/repository/i), 'octo/repo');
    await user.click(screen.getByRole('button', { name: /run tool/i }));

    await waitFor(() => {
      expect(screen.getByTestId('test-tool-result')).toHaveAttribute('data-status', 'unsupportedToolkit');
    });
    expect(screen.getByTestId('test-tool-result')).toHaveTextContent('this deployment cannot build a github toolkit');
  });

  it('clears the previous tool’s result when another tool is picked', async () => {
    server.use(http.post(RUN_PATH, () => HttpResponse.json({ ok: true, result: 'first answer', truncated: false })));

    const user = userEvent.setup();
    renderPane();
    await pickTheTool(user);
    await user.type(await screen.findByLabelText(/repository/i), 'octo/repo');
    await user.click(screen.getByRole('button', { name: /run tool/i }));
    await waitFor(() => {
      expect(screen.getByTestId('test-tool-result')).toBeInTheDocument();
    });

    // Re-opening the picker and choosing again re-selects the same tool, which
    // is the same state transition a different tool makes: the outcome the
    // previous tool produced must not stay under the new tool's arguments.
    await pickTheTool(user);

    await waitFor(() => {
      expect(screen.queryByTestId('test-tool-result')).not.toBeInTheDocument();
    });
  });
});
