/**
 * The `test` every visual spec uses: the stock Playwright one, plus a pin of
 * the persona's stored theme to dark before each test (#2315).
 *
 * The theme is a per-user server preference and the server wins over the
 * browser's `el-mode` on every page load (`app/ThemePreferenceSync`). A spec
 * of any project that writes `light` for the shared persona without the
 * `keepThemeOffTheServer` stub turned every dark shot that ran after it light
 * (five shell shots failed together, 100% of pixels). Rather than hunt a
 * writer per run, each visual test now starts from an explicit stored `dark`
 * (the compiled default, `DEFAULT_COLOR_SCHEME`), through the context's own
 * request (no page interception, so the dark shots still rasterise exactly as
 * before). The light shots keep their `keepThemeOffTheServer` stub, which
 * answers their page locally and so never writes `light` back.
 */
import { test as base } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

export const THEME_URL = `${BASE_URL}/api/v2/social/author/theme`;

export const test = base.extend({
  page: async ({ page }, provide) => {
    const response = await page.request.put(THEME_URL, { data: { theme_mode: 'dark' } });
    if (!response.ok()) {
      // Best effort: a refused pin must not hide the shot's own verdict.
      console.warn(`visual: could not pin the stored theme to dark (HTTP ${response.status()})`);
    }
    await provide(page);
  },
});
