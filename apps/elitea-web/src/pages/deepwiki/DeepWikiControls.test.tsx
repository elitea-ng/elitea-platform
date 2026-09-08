/**
 * The DeepWiki page's HEADER and the three panels it opens (issue 77).
 *
 * `DeepWiki.test.tsx` beside this one renders the page WITHOUT `settings` and
 * WITHOUT `toolkit`, because it is about which wiki version is remembered. That
 * leaves `hasToolkit` false on every render it makes, so the settings toggle,
 * the generation panel, the "Ask about this repository" button, the page editor
 * and the delete-and-forget callback were reached by NOTHING: eight of
 * `DeepWiki.tsx`'s sixteen functions measured zero.
 *
 * A page rendered only in its degraded shape is the same class of blind spot as
 * a composition root nobody mounts — the file reads as covered and the branch
 * an operator actually sees does not run. These tests render it in the shape a
 * real toolkit produces.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { RepositoryIdentity, ToolkitSettings, WikiManifest } from '@/entities/wiki';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { createWikiVersionStorage } from '@/widgets/deepwiki';
import { server } from '@/test/setup';

import { DeepWiki } from './DeepWiki';

const BASE = 'http://elitea.test/api/v2';
const LIST_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket`;
const OBJECT_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket/*`;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const PROJECT_ID = '7';
const TOOLKIT_ID = '42';
const IDENTITY: RepositoryIdentity = { repository: 'acme/notes-service', branch: null };
const SETTINGS: ToolkitSettings = { repository: 'acme/notes-service', branch: 'main' };
const TOOLKIT = { id: 42, name: 'Notes wiki', type: 'wikis', settings: SETTINGS };

const MAIN: WikiManifest = {
  wiki_id: 'acme--notes-service--main',
  wiki_title: 'notes-service (main)',
  repository: 'acme/notes-service',
  branch: 'main',
  pages: ['wiki_pages/overview/main-page.md'],
};
const MANIFEST_KEY = `${MAIN.wiki_id ?? ''}/wiki_manifest_1.json`;

function serveBucket(): void {
  server.use(
    http.get(LIST_ROUTE, () =>
      HttpResponse.json({ objects: [{ key: MANIFEST_KEY, size_bytes: 1 }] }),
    ),
    http.get(OBJECT_ROUTE, ({ params }) =>
      String(params['0']) === MANIFEST_KEY
        ? HttpResponse.json(MAIN)
        : HttpResponse.text('# The overview page'),
    ),
  );
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <DeepWiki
          projectId={PROJECT_ID}
          identity={IDENTITY}
          toolkitId={TOOLKIT_ID}
          settings={SETTINGS}
          toolkit={TOOLKIT}
        />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  configureGeneratedClient({ baseUrl: BASE });
  serveBucket();
});
afterEach(() => {
  resetGeneratedClient();
});

describe('DeepWiki header controls', () => {
  it('shows the toolkit-owned controls only when a toolkit is known', async () => {
    show();

    // Both are gated on `hasToolkit`, which needs BOTH a toolkit id and its
    // settings. The version-selection suite passes only the id, so neither of
    // these had ever rendered.
    expect(await screen.findByTestId('wiki-settings-toggle')).toBeInTheDocument();
    expect(screen.getByText('Ask about this repository')).toBeInTheDocument();
  });

  it('opens and closes the settings panel from the header', async () => {
    const user = userEvent.setup();
    show();

    const toggle = await screen.findByTestId('wiki-settings-toggle');
    expect(toggle).toHaveTextContent('Settings');

    await user.click(toggle);
    // The label is the state: a toggle that never changes its own text is how a
    // panel that failed to open reads as one that opened.
    await waitFor(() => {
      expect(screen.getByTestId('wiki-settings-toggle')).toHaveTextContent('Hide settings');
    });

    await user.click(screen.getByTestId('wiki-settings-toggle'));
    await waitFor(() => {
      expect(screen.getByTestId('wiki-settings-toggle')).toHaveTextContent('Settings');
    });
  });

  it('opens the chat drawer for the open wiki', async () => {
    const user = userEvent.setup();
    show();

    await user.click(await screen.findByText('Ask about this repository'));

    // The drawer is rendered by the page only when `chatTargetFor` produced a
    // target, which needs the toolkit id AND the settings — the same pair the
    // header button is gated on, resolved separately.
    expect(await screen.findByTestId('wiki-chat-drawer')).toBeInTheDocument();
  });

  it('forgets the stored version when the open wiki is deleted', async () => {
    const storage = createWikiVersionStorage(PROJECT_ID, TOOLKIT_ID);
    storage.save(MAIN.wiki_id ?? '');
    server.use(
      http.post(`${BASE}/artifacts/objects/:projectId/:bucket/batch-delete`, () =>
        HttpResponse.json({ deleted: [MANIFEST_KEY], errors: [] }),
      ),
    );
    const user = userEvent.setup();
    show();

    await screen.findByText('notes-service (main)');
    const remove = await screen.findByRole('button', { name: /delete/i });
    await user.click(remove);
    // The confirmation the delete button asks for.
    const confirm = await screen.findByRole('button', { name: /^delete$/i });
    await user.click(confirm);

    // THE POINT: the stored choice is CLEARED, not merely dropped from state.
    // A deleted wiki left in storage is looked up on every later visit and
    // re-selected the moment a wiki with the same id is generated again.
    await waitFor(() => {
      expect(storage.load()).toBeNull();
    });
  });
});
