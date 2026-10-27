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
import { createHostLogger, hostLog, installDiagnostics } from '@/shared/desktop/diagnostics';
import { tauriInvoke } from '@/shared/desktop/hostBridge';
import { applyLocale, storedLocale } from '@/shared/i18n';

import { registerDesktopCatalogue } from './i18n/registerDesktopCatalogue';

// First, so an error anywhere after this — a language that fails to load, a
// shell that fails to start — reaches the host's log file (README, "Logs").
const invoke = tauriInvoke();
if (invoke !== undefined) {
  installDiagnostics(createHostLogger(invoke));
  hostLog('info', `webview started (${import.meta.env.MODE})`, 'boot');
}

// Before the first render: the shell and the workspace screens read desktop-only keys.
registerDesktopCatalogue();

applyLocale(storedLocale())
  // A language that fails to load leaves English; the shell starts either way.
  .catch((error: unknown) => hostLog('error', `language not applied: ${String(error)}`, 'boot'))
  .then(() => import('./start'))
  .then(({ start }) => start(invoke))
  .catch((error: unknown) => hostLog('error', `shell failed to start: ${String(error)}`, 'boot'));
