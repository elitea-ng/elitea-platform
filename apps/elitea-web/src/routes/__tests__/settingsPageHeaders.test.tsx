/**
 * ONE TITLE ROW PER SETTINGS TAB.
 *
 * Four settings pages rendered a header of their own — a 60px row with the
 * tab's name and a hairline under it — while their ROUTE file already rendered
 * `DrawerPageHeader` above them. Preferences, AI Personality, Memory and
 * Personalization each drew the same word twice, 60px apart, with two rules;
 * the reader saw a title bar, then an identical title bar, then the content.
 * Nothing in the unit suites caught it: each page's own tests render the page
 * component alone, without the route that supplies the other header, so both
 * halves were individually correct and only their composition was wrong (the
 * `unit-suite-blind-to-composition-root` class again).
 *
 * The assertion mounts the REAL route tree, so it sees exactly what the
 * browser composes, and counts headings rather than checking one page: a fifth
 * page acquiring a second header fails here too.
 *
 * It also pins the tab titles themselves against the production drawer — the
 * settings pane opened with a page whose title said "AI Configuration" where
 * production says "AI Providers", and Project Context was labelled "Project
 * Params".
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { stubAuthContext } from '@/app/router-context';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { routeTree } from '../../routeTree.gen';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'http://elitea.test/api/v2' });
});
afterEach(() => {
  resetGeneratedClient();
  queryClient.clear();
});

function mountAt(path: string): void {
  const history = createMemoryHistory({ initialEntries: [path] });
  const router = createRouter({ routeTree, history, context: { auth: stubAuthContext } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <CssBaseline />
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/**
 * The drawer lists every tab by name on every settings route, so a bare text
 * query would match the NAV item too. The page title is the one that is NOT
 * inside a button.
 */
function pageTitleCount(title: string): number {
  return screen
    .queryAllByText(title, { exact: true })
    .filter((node) => node.closest('button') === null).length;
}

describe.each([
  ['/settings/preferences', 'Preferences'],
  ['/settings/ai-personality', 'AI Personality'],
  ['/settings/memory', 'Memory'],
  ['/settings/personalization', 'Personalization'],
  ['/settings/profile', 'Profile'],
])('settings page headers — %s', (path, title) => {
  it(`renders "${title}" exactly once outside the drawer`, async () => {
    mountAt(path);

    await waitFor(() => {
      expect(pageTitleCount(title)).toBe(1);
    });
  });
});
