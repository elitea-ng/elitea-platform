import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

const BASE = 'http://elitea.test/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function show(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>{ui}</ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

import { DeleteWikiButton } from './DeleteWikiButton';

const WIKI = { wiki_id: 'acme--svc--main', wiki_title: 'Service Wiki', pages: ['acme--svc--main/wiki_pages/a.md'] };

function serveBucket(keys: string[], failed: string[] = []) {
  let deleted: string[] | null = null;
  server.use(
    http.get(`${BASE}/artifacts/objects/:projectId/:bucket`, () =>
      HttpResponse.json({ objects: keys.map((key) => ({ key, size_bytes: 1 })), common_prefixes: [] }),
    ),
    http.post(`${BASE}/artifacts/objects/:projectId/:bucket\\:batchDelete`, async ({ request }) => {
      const body = (await request.json()) as { keys: string[] };
      deleted = body.keys;
      return HttpResponse.json({
        deleted: body.keys.filter((k) => !failed.includes(k)),
        // The server's envelope (BatchDeleteObjects): `failed`, not `errors`.
        failed: failed.map((key) => ({ key, code: 'PreconditionFailed', message: 'locked' })),
      });
    }),
  );
  return () => deleted;
}

describe('DeleteWikiButton', () => {
  it('deletes every key under the wiki, and ONLY those, in one batch', async () => {
    const user = userEvent.setup();
    // The key set is read at delete time: the analysis file is not in the
    // manifest's page list, and the other wiki must be left alone.
    const sent = serveBucket([
      'acme--svc--main/wiki_manifest_1.json',
      'acme--svc--main/wiki_pages/a.md',
      'acme--svc--main/analysis/structure.json',
      'other--wiki--main/wiki_manifest_1.json',
    ]);
    const onDeleted = vi.fn();
    show(<DeleteWikiButton projectId="7" wiki={WIKI} onDeleted={onDeleted} />);
    await user.click(screen.getByTestId('wiki-delete'));
    await user.click(await screen.findByRole('button', { name: /confirm|delete/i }));
    await waitFor(() => expect(onDeleted).toHaveBeenCalled());
    expect(sent()).toEqual([
      'acme--svc--main/wiki_manifest_1.json',
      'acme--svc--main/wiki_pages/a.md',
      'acme--svc--main/analysis/structure.json',
    ]);
  });

  it('lists the keys that remain after a partial failure, and does not claim success', async () => {
    const user = userEvent.setup();
    serveBucket(['acme--svc--main/wiki_manifest_1.json', 'acme--svc--main/wiki_pages/a.md'], ['acme--svc--main/wiki_pages/a.md']);
    const onDeleted = vi.fn();
    show(<DeleteWikiButton projectId="7" wiki={WIKI} onDeleted={onDeleted} />);
    await user.click(screen.getByTestId('wiki-delete'));
    await user.click(await screen.findByRole('button', { name: /confirm|delete/i }));
    const partial = await screen.findByTestId('wiki-delete-partial');
    expect(partial).toHaveTextContent('acme--svc--main/wiki_pages/a.md');
    expect(onDeleted).not.toHaveBeenCalled();
  });
});
