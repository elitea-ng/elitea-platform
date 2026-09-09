/**
 * The owner's share dialog.
 *
 * The assertion this file exists for is the one-shot token: the URL appears
 * once, in the create response, and the "links on this conversation" list must
 * never offer a way to copy it again — because there is nothing to copy. A
 * Copy button on a listed row would be a control that cannot work, added by
 * someone porting the reference dialog without noticing that this server stores
 * only a hash.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { ShareLinkDialog } from './ShareLinkDialog';

// The fixtures return the response BODY, unwrapped. `eliteaFetch` builds an
// orval-shaped `{data, status, headers}` envelope around whatever the server
// sent, so a fixture that itself nested the payload under `data` would be
// unwrapped one level short and every consumer would read `undefined` off a
// successful 200 — the #132 shape, reproduced here on the way in.

const LINKS = '*/elitea_core/shared_chat_links/prompt_lib/:projectId/:conversationId';
const LINK = '*/elitea_core/shared_chat_link/prompt_lib/:projectId/:conversationId/:linkId';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function mount(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <CssBaseline />
      <QueryClientProvider client={queryClient}>
        <ShareLinkDialog open projectId="7" conversationId="42" conversationName="Quarterly plan" onClose={() => {}} />
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

const activeLink = {
  id: 1,
  scope: 'all',
  has_password: false,
  created_at: '2026-01-01T00:00:00Z',
  expires_at: '2030-01-01T00:00:00Z',
  access_count: 3,
  active: true,
};

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', '/api/v2');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

describe('ShareLinkDialog', () => {
  it('shows the created URL once, and says it cannot be retrieved again', async () => {
    server.use(
      http.get(LINKS, () => HttpResponse.json([])),
      http.post(LINKS, () => HttpResponse.json({ ...activeLink, token: 'the-only-copy' }, { status: 201 })),
    );
    const writeText = vi.fn(() => Promise.resolve());
    // DEFINED, not spied and not spread. `vi.spyOn(navigator, 'clipboard')`
    // throws when the property is absent, and jsdom only sometimes provides it
    // — the spy passed in isolation and failed in the full-suite worker.
    // Spreading `navigator` instead is worse: it is a class instance, so the
    // replacement loses its prototype and answers `undefined` for every other
    // accessor the render happens to read.
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true, writable: true });

    mount();
    await userEvent.click(screen.getByTestId('share-link-create'));

    const created = await screen.findByTestId('share-link-created');
    expect(created).toHaveTextContent('the-only-copy');
    expect(created.textContent ?? '').toMatch(/shown once|cannot be retrieved/i);
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining('/shared/chat/the-only-copy'));

    /*
     * THE ADDRESS IS ITS OWN ELEMENT, and its text is the address alone.
     *
     * The box around it also holds the "shown once" sentence, and the two are
     * adjacent text nodes: reading the BOX yields
     * `…/shared/chat/the-only-copyCopied to your clipboard…`, i.e. the token
     * with the next sentence's first word welded on. A recipient handed that
     * string gets a token that does not exist — which is what the share
     * journey's reader met, and the one failure this dialog cannot recover
     * from, because the plaintext token is never served twice.
     */
    const url = await screen.findByTestId('share-link-created-url');
    expect(writeText).toHaveBeenCalledWith(url.textContent);
    expect(url.textContent).toMatch(/^https?:\/\/\S+\/shared\/chat\/the-only-copy$/);
    expect(url.textContent).not.toMatch(/copied/i);
    expect(created.textContent ?? '').toContain('the-only-copyCopied');
  });

  it('offers no Copy on a listed link — the token is not recoverable', async () => {
    server.use(http.get(LINKS, () => HttpResponse.json([activeLink])));

    mount();

    await screen.findByTestId('share-link-row');
    expect(screen.queryByRole('button', { name: /copy/i })).toBeNull();
    expect(screen.getByTestId('share-link-revoke')).toBeInTheDocument();
  });

  it('revokes a link by its row id, not by a token it does not hold', async () => {
    let revokedPath = '';
    server.use(
      http.get(LINKS, () => HttpResponse.json([activeLink])),
      http.delete(LINK, ({ request }) => {
        revokedPath = new URL(request.url).pathname;
        return new HttpResponse(null, { status: 204 });
      }),
    );

    mount();
    await userEvent.click(await screen.findByTestId('share-link-revoke'));

    await waitFor(() => {
      expect(revokedPath).toContain('/shared_chat_link/prompt_lib/7/42/1');
    });
  });

  it('offers no "never expires" option', async () => {
    server.use(http.get(LINKS, () => HttpResponse.json([])));

    mount();

    await userEvent.click(screen.getByTestId('share-link-expiry').querySelector('[role="combobox"]') ?? screen.getByTestId('share-link-expiry'));
    const options = await screen.findAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['1 hour', '1 day', '7 days', '30 days']);
  });
});
