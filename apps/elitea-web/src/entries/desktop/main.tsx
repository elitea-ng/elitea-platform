/**
 * Desktop entry (ADR-0029 decision 9): the page the Tauri host loads from its
 * BUNDLED assets. It is never pointed at remote content — the privileged
 * webview only ever runs this build, and talks to the deployment through the
 * host's network layer.
 *
 * The connect / sign-in screen is rendered before the app exists, under the
 * compiled default brand and the desktop component layer: there is no
 * deployment pack to fetch until one is chosen.
 * Its copy is English-only for that reason; strings added since go through
 * `t()`, which resolves against the bundled `en` catalogue.
 */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { tauriAppIpc } from '@/shared/desktop/appEvents';
import { installContextMenuGuard } from '@/shared/desktop/contextMenu';
import { createHostLogger, hostLog, installDiagnostics } from '@/shared/desktop/diagnostics';
import { createDoctorIpc } from '@/shared/desktop/doctorIpc';
import { createHostBridge, tauriInvoke } from '@/shared/desktop/hostBridge';
import { DesktopThemeLayer } from '@/widgets/desktop-shell';

import { DesktopShell } from './DesktopShell';
import { registerDesktopCatalogue } from './i18n/registerDesktopCatalogue';

const container = document.getElementById('root');
if (!container) {
  throw new Error('elitea-web desktop: #root container missing from index.html');
}

// Before the first render: the shell and the workspace screens read desktop-only keys.
registerDesktopCatalogue();

const invoke = tauriInvoke();
const bridge = invoke === undefined ? undefined : createHostBridge(invoke);
const doctor = invoke === undefined ? undefined : createDoctorIpc(invoke);
const appIpc = tauriAppIpc();
// First, so an error anywhere after this reaches the host's log file (README, "Logs").
if (invoke !== undefined) {
  installDiagnostics(createHostLogger(invoke));
  hostLog('info', `webview started (${import.meta.env.MODE})`, 'boot');
  // Inside the host only: the dev harness in a browser keeps its devtools menu.
  installContextMenuGuard();
}

function renderShell(root: HTMLElement): void {
  createRoot(root).render(
    <StrictMode>
      <BrandThemeProvider>
        <DesktopThemeLayer>
          <DesktopShell bridge={bridge} doctor={doctor} appIpc={appIpc} />
        </DesktopThemeLayer>
      </BrandThemeProvider>
    </StrictMode>,
  );
}

// DEV ONLY: `/?harness` shows the signed-in shell with a fake host and canned
// data (`devHarness.tsx`). `import.meta.env.DEV` is false in every build, so
// the branch and the harness's chunk are dropped from it.
if (import.meta.env.DEV && bridge === undefined && new URLSearchParams(window.location.search).has('harness')) {
  void import('./devHarness').then(({ mountDesktopHarness }) => mountDesktopHarness(container));
} else {
  renderShell(container);
}
