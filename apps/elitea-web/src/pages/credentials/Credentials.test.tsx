import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { Credentials } from './Credentials';
import { renderCredentialsRoute } from './__tests__/testRouter';

const BASE = '/api/v2';

afterEach(() => {
  resetGeneratedClient();
});

describe('Credentials (ROUTE-022 target)', () => {
  it('renders the credentials list under its toolbar', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderCredentialsRoute(
      <QueryClientProvider client={client}>
        <Credentials
          tab="all"
          projectId="7"
          onSelectCredential={vi.fn()}
          onCreateNew={vi.fn()}
        />
      </QueryClientProvider>,
      '/credentials/all',
    );
    expect(await screen.findByText('No credentials yet')).toBeInTheDocument();
    expect(screen.getByTestId('page-toolbar-summary')).toHaveTextContent('0 credentials');
  });

  /** COMPOSITION ROOT — the shell's breadcrumb names the page; the page draws only its toolbar. */
  it('renders its search and create action in the page toolbar, and no heading of its own', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderCredentialsRoute(
      <QueryClientProvider client={client}>
        <Credentials
          tab="all"
          projectId="7"
          onSelectCredential={vi.fn()}
          onCreateNew={vi.fn()}
        />
      </QueryClientProvider>,
      '/credentials/all',
    );

    const toolbar = await screen.findByTestId('page-toolbar');
    expect(toolbar).toContainElement(screen.getByTestId('credentials-search-input'));
    expect(toolbar).toContainElement(screen.getByRole('button', { name: 'New credential' }));
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });
});
