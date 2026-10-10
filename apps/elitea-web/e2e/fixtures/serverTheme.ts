/**
 * Keeps a test's theme choice out of the server.
 *
 * The theme is a per-user server preference (`GET`/`PUT
 * /api/v2/social/author/theme`), shared by every page of that user. Suites
 * that run in parallel as one persona — the visual project runs four workers
 * as `member` — would otherwise leak a light-scheme test's click into every
 * other test of that persona: their pages read the stored mode on load and on
 * focus, and the server wins. That is the feature working, which is why the
 * test, not the app, has to stay out of it.
 *
 * The page still drives the real toggle and the real MUI mode (the rule
 * issue #61 sets for the light shots); only the round trip to the stored
 * preference is answered locally: reads say "never chosen", writes succeed
 * and are dropped.
 */
import type { Page } from '@playwright/test';

const THEME_ROUTE = '**/api/v2/social/author/theme';

export async function keepThemeOffTheServer(page: Page): Promise<void> {
  await page.route(THEME_ROUTE, async (route) => {
    const method = route.request().method();
    if (method === 'GET') {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ theme_mode: null }) });
      return;
    }
    if (method === 'PUT') {
      await route.fulfill({ status: 200, contentType: 'application/json', body: route.request().postData() ?? '{}' });
      return;
    }
    await route.fallback();
  });
}
