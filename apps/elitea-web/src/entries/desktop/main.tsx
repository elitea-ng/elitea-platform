/**
 * Desktop entry (ADR-0029 decision 9): the page the Tauri host loads from its
 * BUNDLED assets. It is never pointed at remote content — the privileged
 * webview only ever runs this build, and talks to the deployment through the
 * host's network layer.
 *
 * The connect / sign-in screen is rendered before the app exists, with a bare
 * MUI theme: there is no brand pack to fetch until a deployment is chosen.
 * Its copy is English-only for that reason (no i18n catalogue is loaded yet).
 */
import { CssBaseline, ThemeProvider, createTheme } from '@mui/material';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { createHostBridge, tauriInvoke } from '@/shared/desktop/hostBridge';

import { DesktopShell } from './DesktopShell';

const container = document.getElementById('root');
if (!container) {
  throw new Error('elitea-web desktop: #root container missing from index.html');
}

const invoke = tauriInvoke();
const bridge = invoke === undefined ? undefined : createHostBridge(invoke);

createRoot(container).render(
  <StrictMode>
    <ThemeProvider theme={createTheme()}>
      <CssBaseline />
      <DesktopShell bridge={bridge} />
    </ThemeProvider>
  </StrictMode>,
);
