/**
 * The DeepWiki page's ONE piece of remembered state: which wiki version is
 * open.
 *
 * A repository analysed twice has two manifests, and the browser opens one of
 * them. The legacy app remembered which (`wikiVersionStorageKey`,
 * DeepWikiApp.jsx:733/1284/1388); the port kept the choice in component state,
 * so reopening the screen dropped the reader back onto the newest manifest
 * without saying so.
 *
 * The tests below pin the three halves of that rule separately — persist,
 * restore, fall back — because each fails on its own and each looks like the
 * others from the screen.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { RepositoryIdentity, WikiManifest } from '@/entities/wiki';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { STORAGE_NAMESPACE, clearNamespace } from '@/shared/lib/storage';
import { createWikiVersionStorage } from '@/widgets/deepwiki';
import { server } from '@/test/setup';

import { DeepWiki } from './DeepWiki';

const BASE = 'http://elitea.test/api/v2';
const LIST_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket`;
const OBJECT_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket/*`;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const PROJECT_ID = '7';
const TOOLKIT_ID = '42';

/**
 * Two versions of ONE repository. The toolkit names no branch, and an
 * unbranched identity sees every branch's wiki (`manifestMatchesRepo`), so
 * both are listed and either can be the open one.
 */
const IDENTITY: RepositoryIdentity = { repository: 'acme/notes-service', branch: null };

const MAIN: WikiManifest = {
  wiki_id: 'acme--notes-service--main',
  wiki_title: 'notes-service (main)',
  repository: 'acme/notes-service',
  branch: 'main',
  pages: ['wiki_pages/overview/main-page.md'],
};

const DEV: WikiManifest = {
  wiki_id: 'acme--notes-service--dev',
  wiki_title: 'notes-service (dev)',
  repository: 'acme/notes-service',
  branch: 'dev',
  pages: ['wiki_pages/overview/dev-page.md'],
};

const MANIFESTS = [MAIN, DEV];

function manifestKeyOf(wiki: WikiManifest): string {
  return `${wiki.wiki_id ?? ''}/wiki_manifest_1.json`;
}

function serveBucket(): void {
  server.use(
    http.get(LIST_ROUTE, () =>
      HttpResponse.json({
        objects: MANIFESTS.map((wiki) => ({ key: manifestKeyOf(wiki), size_bytes: 1 })),
      }),
    ),
    http.get(OBJECT_ROUTE, ({ params }) => {
      const key = String(params['0']);
      const manifest = MANIFESTS.find((wiki) => manifestKeyOf(wiki) === key);
      // A manifest comes back as JSON; anything else under a wiki id is one of
      // its pages, and pages are TEXT.
      return manifest ? HttpResponse.json(manifest) : HttpResponse.text(`# ${key}`);
    }),
  );
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <DeepWiki projectId={PROJECT_ID} identity={IDENTITY} toolkitId={TOOLKIT_ID} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** Which wiki the READER opened — the manifest's own page is what says so. */
async function openedPage(): Promise<string> {
  const dev = screen.queryByText('wiki_pages/overview/dev-page.md');
  if (dev) return 'dev';
  await screen.findByText('wiki_pages/overview/main-page.md');
  return 'main';
}

beforeEach(() => {
  window.localStorage.clear();
  configureGeneratedClient({ baseUrl: BASE });
  serveBucket();
});
afterEach(() => {
  resetGeneratedClient();
});

describe('DeepWiki version selection', () => {
  it('opens the first wiki when nothing has been chosen', async () => {
    show();
    await screen.findByText('notes-service (dev)');
    expect(await openedPage()).toBe('main');
  });

  it('reopens the wiki chosen last time, across a remount', async () => {
    const user = userEvent.setup();
    const first = show();
    await user.click(await screen.findByText('notes-service (dev)'));
    await waitFor(async () => {
      expect(await openedPage()).toBe('dev');
    });

    // The remount is the whole point: component state does not survive it, and
    // before this the reader silently went back to `main`.
    first.unmount();
    show();
    await screen.findByText('notes-service (dev)');
    expect(await openedPage()).toBe('dev');
  });

  it('falls back to the first wiki when the stored choice is no longer listed', async () => {
    // A version that was deleted, or a toolkit whose repository moved. Landing
    // the reader on nothing is the failure this rules out.
    createWikiVersionStorage(PROJECT_ID, TOOLKIT_ID).save('acme--notes-service--deleted');
    show();
    await screen.findByText('notes-service (dev)');
    expect(await openedPage()).toBe('main');
  });

  it('keeps the choice inside the namespace the logout sweep clears', async () => {
    const user = userEvent.setup();
    const first = show();
    await user.click(await screen.findByText('notes-service (dev)'));

    // Asserted through the helper, not by reading a raw key: what matters is
    // that the value is REACHABLE where the app looks for it and GONE after a
    // sweep. A raw `deepwiki.selected_manifest.…` key would satisfy neither
    // (issue #22).
    const storage = createWikiVersionStorage(PROJECT_ID, TOOLKIT_ID);
    await waitFor(() => {
      expect(storage.load()).toBe(DEV.wiki_id);
    });
    for (let index = 0; index < window.localStorage.length; index += 1) {
      expect(window.localStorage.key(index)?.startsWith(STORAGE_NAMESPACE)).toBe(true);
    }

    clearNamespace();
    expect(storage.load()).toBeNull();
    first.unmount();
    show();
    await screen.findByText('notes-service (dev)');
    expect(await openedPage()).toBe('main');
  });
});
