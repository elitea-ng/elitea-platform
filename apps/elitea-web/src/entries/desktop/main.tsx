/**
 * Desktop entry (ADR-0029 decision 9): the page the Tauri host loads from its
 * BUNDLED assets. It is never pointed at remote content — the privileged
 * webview only ever runs this build, and talks to the deployment through the
 * host's network layer.
 *
 * The connect / sign-in screen is rendered before the app exists, with a bare
 * MUI theme: there is no brand pack to fetch until a deployment is chosen.
 * Its copy is English-only for that reason; strings added since go through
 * `t()`, which resolves against the bundled `en` catalogue.
 */
import { CssBaseline, ThemeProvider, createTheme } from '@mui/material';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { createHostBridge, tauriInvoke } from '@/shared/desktop/hostBridge';

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

function renderShell(root: HTMLElement): void {
  createRoot(root).render(
    <StrictMode>
      <ThemeProvider theme={createTheme()}>
        <CssBaseline />
        <DesktopShell bridge={bridge} />
      </ThemeProvider>
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
