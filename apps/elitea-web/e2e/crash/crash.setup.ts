/** Signs the crash subject in once; the `crash` project reuses the storage state. */
import { test as setup } from '@playwright/test';

import { CRASH_STORAGE_STATE } from '../../playwright.crash.config';
import { performOidcLogin, requireEnv } from './crashControl';

setup('sign in as the crash subject', async ({ page }) => {
  await performOidcLogin(
    page,
    requireEnv('PLAYWRIGHT_BASE_URL'),
    process.env['CRASH_OIDC_SUBJECT'] ?? 'admin@centry.user',
    CRASH_STORAGE_STATE,
  );
});
