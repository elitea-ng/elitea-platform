import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';

import {
  getCreateModerationRequestMockHandler,
  getModerationStatusMockHandler,
} from '@/shared/api/generated/admin/admin.msw';
import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getListToolkitsMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { REQUEST_STATUS } from '../../lib/constants';
import { createTestQueryClient } from '../../__tests__/testUtils';

import { ApplicationCatalog } from './ApplicationCatalog';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * `ApplicationCatalog` composes `useApplicationCatalog`/`useModerationRequests`,
 * both of which read the selected project id through the real TanStack
 * Router root context (`api/useSelectedProjectId.ts`) — the same
 * router-harness pattern `__tests__/testUtils.tsx`'s `renderHookWithRouter`
 * uses, built directly here because this is a full component render (with
 * user interaction), not a hook probe.
 */
function renderCatalog(onConfigure = vi.fn()) {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ApplicationCatalog onConfigure={onConfigure} />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return { ...render(<RouterProvider router={router} />), onConfigure, queryClient };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListToolkitsMockHandler({}),
    getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    getModerationStatusMockHandler({ total: 0, rows: [] }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ApplicationCatalog', () => {
  it('renders both catalog cards', async () => {
    renderCatalog();
    expect(await screen.findByText('Wikis')).toBeInTheDocument();
    expect(screen.getByText('Inventory')).toBeInTheDocument();
  });

  it('calls onConfigure with the catalog type when Configure is clicked', async () => {
    server.use(
      getListToolkitsMockHandler({ inventory: { metadata: { application: true, label: 'Inventory' } } }),
      getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    );
    const user = userEvent.setup();
    const { onConfigure } = renderCatalog();

    const configureButtons = await screen.findAllByRole('button', { name: 'Configure' });
    expect(configureButtons).toHaveLength(1);
    await user.click(configureButtons[0]!);
    expect(onConfigure).toHaveBeenCalledWith('inventory');
  });

  it('opens the request-access modal for the clicked card and submits through to the moderation API', async () => {
    server.use(getCreateModerationRequestMockHandler({
        id: 1,
        user_id: 7,
        user_email: 'requester@example.com',
        project_id: 1,
        issue_type: 'Inventory',
        entity_id: 'inventory',
        description: 'I need this',
        status: REQUEST_STATUS.APPROVED,
        rejection_comment: null,
        created_at: '2026-08-01T10:00:00Z',
        updated_at: '2026-08-01T10:00:00Z',
      }));
    const user = userEvent.setup();
    renderCatalog();

    const requestButtons = await screen.findAllByRole('button', { name: 'Request Access' });
    expect(requestButtons).toHaveLength(2);
    await user.click(requestButtons[0]!);

    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    const textbox = screen.getByPlaceholderText('Describe why you need access to this application...');
    await user.type(textbox, 'onboarding');
    await user.click(screen.getByRole('button', { name: 'Send Request' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('closing the modal without submitting leaves both cards untouched', async () => {
    const user = userEvent.setup();
    renderCatalog();

    const requestButtons = await screen.findAllByRole('button', { name: 'Request Access' });
    await user.click(requestButtons[0]!);
    expect(await screen.findByRole('dialog')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getAllByRole('button', { name: 'Request Access' })).toHaveLength(2);
  });
});
