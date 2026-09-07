/**
 * `DeepWikiToolkit` is the composition root both `/deepwiki` entry points go
 * through, and NOTHING executed it (issue 77).
 *
 * The file measured 0 of 6 statements and 0 of 1 functions before this test.
 * `DeepWiki.test.tsx` beside it renders the page component DIRECTLY, with a
 * `RepositoryIdentity` handed in as a prop, so it never touched the part that
 * resolves one — which is the only thing this component does. A whole composition
 * root at zero, inside a suite that reads as covering the screen, is the failure
 * class this repository has already shipped seven times (see the note on
 * `src/routes/_shell/toolkits/**` in scripts/merge-coverage.mjs).
 *
 * All three branches are pinned separately, because each one fails on its own
 * and two of them look like "the wiki list is empty" from the screen.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { DeepWikiToolkit } from './DeepWikiToolkit';

const BASE = 'http://elitea.test/api/v2';
const TOOLKIT_ROUTE = `${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`;
const LIST_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket`;
const OBJECT_ROUTE = `${BASE}/artifacts/objects/:projectId/:bucket/*`;

/** One manifest for the repository the toolkit below is configured for. */
const MANIFEST = {
  wiki_id: 'acme--notes-service--main',
  wiki_title: 'notes-service (main)',
  repository: 'acme/notes-service',
  branch: 'main',
  pages: ['wiki_pages/overview/main-page.md'],
};
const MANIFEST_KEY = `${MANIFEST.wiki_id}/wiki_manifest_1.json`;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const PROJECT_ID = '7';
const TOOLKIT_ID = '42';

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <DeepWikiToolkit projectId={PROJECT_ID} toolkitId={TOOLKIT_ID} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('DeepWikiToolkit', () => {
  it('resolves the toolkit and hands its repository to the browser', async () => {
    server.use(
      http.get(TOOLKIT_ROUTE, () =>
        HttpResponse.json({
          id: 42,
          name: 'Notes wiki',
          type: 'wikis',
          settings: { repository: 'acme/notes-service', branch: 'main' },
        }),
      ),
      // The wiki listing the browser makes once it HAS an identity. It is
      // reached only when the resolution succeeded, and it is filtered by the
      // repository this component resolved — so the title below appearing is
      // the whole assertion.
      http.get(LIST_ROUTE, () =>
        HttpResponse.json({ objects: [{ key: MANIFEST_KEY, size_bytes: 1 }] }),
      ),
      http.get(OBJECT_ROUTE, ({ params }) =>
        String(params['0']) === MANIFEST_KEY
          ? HttpResponse.json(MANIFEST)
          : HttpResponse.text('# page'),
      ),
    );

    show();

    expect(await screen.findByText('notes-service (main)')).toBeInTheDocument();
    expect(screen.queryByTestId('deepwiki-toolkit-error')).not.toBeInTheDocument();
  });

  it('shows the pending state instead of an empty browser while the toolkit loads', () => {
    server.use(
      // A request that never settles. `isPending` is not a cosmetic state here:
      // rendering the browser now would say "no wikis" about a repository this
      // screen has not learned the name of yet.
      http.get(TOOLKIT_ROUTE, () => new Promise(() => {})),
    );

    show();

    expect(screen.queryByTestId('deepwiki-toolkit-error')).not.toBeInTheDocument();
    // `RoutePending` is an `<output>`, whose implicit role is `status`.
    expect(screen.getByRole('status')).toBeInTheDocument();
  });

  it('refuses rather than reporting an empty wiki list when the toolkit cannot be read', async () => {
    server.use(http.get(TOOLKIT_ROUTE, () => HttpResponse.json({ error: 'nope' }, { status: 500 })));

    show();

    // THE POINT OF THE BRANCH: a toolkit that cannot be read is not a
    // repository with no wikis, and the two are indistinguishable to a reader
    // unless the refusal is rendered.
    expect(await screen.findByTestId('deepwiki-toolkit-error')).toBeInTheDocument();
  });
});
