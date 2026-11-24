import type { ComponentProps } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClientProvider } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getDeleteApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { DeleteApplicationButton } from './DeleteApplicationButton';

/** `DeleteEntityModal` reads `theme.vars.palette.*` — this file drives its own `RouterProvider` (needed for `useSelectedProjectId`'s `useRouteContext`) rather than the shared `renderWithProviders` helper, so the theme has to be wired in here too. */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderButton(props: Partial<ComponentProps<typeof DeleteApplicationButton>> = {}) {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <DeleteApplicationButton
            applicationId="42"
            name="My Agent"
            {...props}
          />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

describe('DeleteApplicationButton', () => {
  it('opens the confirmation modal on click, closed by default', async () => {
    const user = userEvent.setup();
    renderButton();
    expect(screen.queryByText('Delete confirmation')).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'delete entity' }));
    expect(screen.getByText('Delete confirmation')).toBeInTheDocument();
  });

  it('calls onDeleted after a successful confirm (requires typing the exact name first)', async () => {
    server.use(getDeleteApplicationMockHandler());
    const onDeleted = vi.fn();
    const user = userEvent.setup();
    renderButton({ onDeleted });

    await user.click(await screen.findByRole('button', { name: 'delete entity' }));
    await user.type(screen.getByLabelText('Name'), 'My Agent');
    await user.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
  });

  it('calls onError when the delete request fails', async () => {
    server.use(
      http.delete('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'nope' }, { status: 403 }),
      ),
    );
    const onError = vi.fn();
    const user = userEvent.setup();
    renderButton({ onError });

    await user.click(await screen.findByRole('button', { name: 'delete entity' }));
    await user.type(screen.getByLabelText('Name'), 'My Agent');
    await user.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
  });

  it("shows the server's reason for a refused delete inside the dialog", async () => {
    server.use(
      http.delete('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'Unpublish first. Cannot delete application with published versions.' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    renderButton();

    await user.click(await screen.findByRole('button', { name: 'delete entity' }));
    await user.type(screen.getByLabelText('Name'), 'My Agent');
    await user.click(screen.getByRole('button', { name: 'Delete' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Unpublish first. Cannot delete application with published versions.');
    expect(screen.getByText('Delete confirmation')).toBeInTheDocument();
  });
});
