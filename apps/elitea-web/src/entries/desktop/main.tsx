/**
 * Desktop entry (ADR-0029 decision 9): the page the Tauri host loads from its
 * BUNDLED assets. It is never pointed at remote content — the privileged
 * webview only ever runs this build, and talks to the deployment through the
 * host's network layer.
 *
 * This module applies the person's saved language and only then imports the
 * shell (`./start.tsx`): modules evaluate their top-level `t()` calls when
 * they are imported, so the catalogues must be in place first. In English
 * (the default) nothing loads and the shell follows at once.
 */
import { applyLocale, storedLocale } from '@/shared/i18n';

import { registerDesktopCatalogue } from './i18n/registerDesktopCatalogue';

// Before the first render: the shell and the workspace screens read desktop-only keys.
registerDesktopCatalogue();

applyLocale(storedLocale())
  // A language that fails to load leaves English; the shell starts either way.
  .catch((error: unknown) => console.error('[desktop] language not applied', error))
  .then(() => import('./start'))
  .then(({ start }) => start())
  .catch((error: unknown) => console.error('[desktop] shell failed to start', error));
