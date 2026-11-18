import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PipelineReadOnlyNotice } from './PipelineReadOnlyNotice';

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

function renderNotice(disabled: boolean, isPublished = false) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => <QueryClientProvider client={client}><PipelineReadOnlyNotice disabled={disabled} isPublished={isPublished} /></QueryClientProvider>,
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'p1' } },
  });
  return render(<RouterProvider router={router} />);
}

describe('PipelineReadOnlyNotice', () => {
  it('says why a disabled editor is read-only once permissions are known', async () => {
    server.use(http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([])));
    renderNotice(true);
    expect(await screen.findByTestId('pipeline-read-only-notice')).toHaveAttribute('data-cause', 'permission');
  });

  it('names a published pipeline without waiting for the permission list', async () => {
    server.use(http.get('*/auth/permissions/prompt_lib/:projectId', () => new Promise(() => undefined)));
    renderNotice(true, true);
    expect(await screen.findByTestId('pipeline-read-only-notice')).toHaveAttribute('data-cause', 'published');
  });

  it('stays silent for an editable pipeline', async () => {
    server.use(http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([])));
    renderNotice(false);
    await waitFor(() => expect(screen.queryByTestId('pipeline-read-only-notice')).not.toBeInTheDocument());
  });
});
