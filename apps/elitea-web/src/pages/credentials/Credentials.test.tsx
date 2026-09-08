import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { Credentials } from './Credentials';

const BASE = '/api/v2';

afterEach(() => {
  resetGeneratedClient();
});

describe('Credentials (ROUTE-022 target)', () => {
  it('renders the page heading (COPY-466) and the credentials list', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <Credentials
          tab="all"
          projectId="7"
          onSelectCredential={vi.fn()}
          onCreateNew={vi.fn()}
        />
      </QueryClientProvider>,
    );
    expect(screen.getByText('Credentials')).toBeInTheDocument();
    expect(await screen.findByText('No credentials yet')).toBeInTheDocument();
  });

  /** COMPOSITION ROOT — see `pages/skills/Skills.test.tsx` (issue 841). */
  it('renders its heading inside the shared page header', () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <Credentials
          tab="all"
          projectId="7"
          onSelectCredential={vi.fn()}
          onCreateNew={vi.fn()}
        />
      </QueryClientProvider>,
    );

    const header = screen.getByTestId('page-header');
    expect(header).toContainElement(screen.getByRole('heading', { name: 'Credentials', level: 1 }));
  });
});
