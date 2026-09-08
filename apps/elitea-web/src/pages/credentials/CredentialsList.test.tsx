import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialsList } from './CredentialsList';

const BASE = '/api/v2';

function renderList(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

afterEach(() => {
  resetGeneratedClient();
});

describe('CredentialsList', () => {
  it('shows the empty-state message when there are no credentials', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    server.use(http.post(`${BASE}/configurations/check_connections/7`, () => HttpResponse.json([])));
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    expect(await screen.findByText('No credentials yet')).toBeInTheDocument();
  });

  // A FAILED LIST IS NOT AN EMPTY LIST. Before this branch existed, a 403 fell
  // through to "No credentials yet". The screen reported an empty
  // project when the request never returned a list. That is what a live
  // deployment showed while every GET of the list answered 403.
  it('reports a forbidden list as an error, not as an empty project', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    expect(
      await screen.findByText('You do not have permission to read the credentials of this project.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('No credentials yet')).not.toBeInTheDocument();
  });

  it('reports any other list failure as an error, not as an empty project', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({ error: 'list failed' }, { status: 500 }),
      ),
    );
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    expect(await screen.findByText('The credentials could not be loaded.')).toBeInTheDocument();
    expect(screen.queryByText('No credentials yet')).not.toBeInTheDocument();
  });

  it('shows the "nothing found" message when a search yields no rows', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText('Search credentials'), { target: { value: 'nope' } });
    expect(await screen.findByText('Nothing found.')).toBeInTheDocument();
  });

  it('renders loaded credentials, sorted pinned-first, with a type/scope subtitle, and fires ACT-039 batch validation on load', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({
          items: [
            { uid: 'a', type: 'openai', elitea_title: 'A cred', is_pinned: false },
            { uid: 'b', type: 'azure', elitea_title: 'B cred', is_pinned: true },
          ],
          total: 2,
          limit: 20,
          offset: 0,
          shared: { items: [], total: 0 },
        }),
      ),
    );
    let batchCalled = false;
    server.use(
      http.post(`${BASE}/configurations/check_connections/7`, () => {
        batchCalled = true;
        return HttpResponse.json([]);
      }),
    );
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    expect(await screen.findByText('B cred')).toBeInTheDocument();
    const rows = screen.getAllByRole('button').map((el) => el.textContent ?? '');
    const bIndex = rows.findIndex((text) => text.includes('B cred'));
    const aIndex = rows.findIndex((text) => text.includes('A cred'));
    expect(bIndex).toBeLessThan(aIndex);
    await waitFor(() => expect(batchCalled).toBe(true));
  });

  it('calls onSelectCredential when a row is clicked', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({ items: [{ uid: 'a', type: 'openai', elitea_title: 'A cred' }], total: 1, limit: 20, offset: 0 }),
      ),
    );
    server.use(http.post(`${BASE}/configurations/check_connections/7`, () => HttpResponse.json([])));
    const onSelectCredential = vi.fn();
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={onSelectCredential}
        onCreateNew={vi.fn()}
      />,
    );
    fireEvent.click(await screen.findByText('A cred'));
    expect(onSelectCredential).toHaveBeenCalledWith('a');
  });

  it('calls onCreateNew from the New credential button', () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })));
    const onCreateNew = vi.fn();
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={onCreateNew}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'New credential' }));
    expect(onCreateNew).toHaveBeenCalledTimes(1);
  });

  it('shows "Load more" while more rows exist, and advances the page when clicked', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const offsets: string[] = [];
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, ({ request }) => {
        offsets.push(new URL(request.url).searchParams.get('offset') ?? '');
        return HttpResponse.json({
          items: [{ uid: 'a', type: 'openai', elitea_title: 'A cred' }],
          total: 40,
          limit: 20,
          offset: 0,
        });
      }),
    );
    server.use(http.post(`${BASE}/configurations/check_connections/7`, () => HttpResponse.json([])));
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(offsets).toContain('20'));
  });

  it('toggling a type filter re-fetches with the type param and resets to page 0', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const urls: string[] = [];
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, ({ request }) => {
        urls.push(request.url);
        return HttpResponse.json({
          items: [
            { uid: 'a', type: 'openai', elitea_title: 'A cred' },
            { uid: 'b', type: 'azure', elitea_title: 'B cred' },
          ],
          total: 2,
          limit: 20,
          offset: 0,
        });
      }),
    );
    server.use(http.post(`${BASE}/configurations/check_connections/7`, () => HttpResponse.json([])));
    renderList(
      <CredentialsList
        projectId="7"
        onSelectCredential={vi.fn()}
        onCreateNew={vi.fn()}
      />,
    );
    await screen.findByText('A cred');
    fireEvent.click(screen.getByRole('button', { name: 'Openai' }));
    await waitFor(() => expect(urls.some((u) => u.includes('type=openai'))).toBe(true));
  });
});
