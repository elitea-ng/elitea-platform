/**
 * Desktop entry (ADR-0029 decision 9): the page the Tauri host loads from its
 * BUNDLED assets. It is never pointed at remote content — the privileged
 * webview only ever runs this build, and talks to the deployment through the
 * host's network layer.
 *
 * This module applies the language (the person's choice, else the system's)
 * and only then imports the shell (`./start.tsx`): modules evaluate their
 * top-level `t()` calls when they are imported, so the catalogues must be in
 * place first. A right-to-left language also loads its style cache here, and
 * the host's own menus get their labels in the same language.
 *
 * The computer use session panel's webview (`#computer-panel`) boots with
 * less (`./bootPlan.ts`): no host log, no menu labels, and the panel alone
 * (`./computerPanelStart.tsx`), not the shell. Its capability grants nothing
 * those would call.
 */
import { installRtlStyleCache } from '@/app/providers/BrandThemeProvider';
import { createHostLogger, hostLog, installDiagnostics } from '@/shared/desktop/diagnostics';
import { tauriInvoke } from '@/shared/desktop/hostBridge';
import { applyLocale, isRtl, startLocale } from '@/shared/i18n';

import { boot } from './bootPlan';
import { nativeMenuLabels } from './i18n/nativeMenuLabels';
import { registerDesktopCatalogue } from './i18n/registerDesktopCatalogue';

// Before the first render: the shell and the workspace screens read desktop-only keys.
registerDesktopCatalogue();

void boot(window.location.hash, {
  invoke: tauriInvoke(),
  installHostLog: (invoke) => installDiagnostics(createHostLogger(invoke)),
  log: (level, message) => hostLog(level, message, 'boot'),
  applyLanguage: async () => {
    const locale = startLocale();
    await applyLocale(locale);
    if (isRtl(locale)) installRtlStyleCache((await import('./rtlStyleCache')).createRtlStyleCache());
  },
  menuLabels: nativeMenuLabels,
  startShell: async (invoke) => (await import('./start')).start(invoke),
  startPanel: async () => (await import('./computerPanelStart')).startComputerPanel(),
});
