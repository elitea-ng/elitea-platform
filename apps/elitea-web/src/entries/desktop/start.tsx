/**
 * The desktop shell's start, imported by `./main.tsx` once the saved language
 * is applied.
 *
 * The connect / sign-in screen is rendered before the app exists, under the
 * compiled default brand and the desktop component layer: there is no
 * deployment pack to fetch until one is chosen. Its strings go through
 * `t()`, in the language `./main.tsx` applied.
 */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { tauriAppIpc } from '@/shared/desktop/appEvents';
import { installContextMenuGuard } from '@/shared/desktop/contextMenu';
import { createDoctorIpc } from '@/shared/desktop/doctorIpc';
import { createHostBridge, type HostInvoke } from '@/shared/desktop/hostBridge';
import { DesktopThemeLayer } from '@/widgets/desktop-shell';

import { DesktopShell, type DesktopShellProps } from './DesktopShell';

function renderShell(root: HTMLElement, host: Pick<DesktopShellProps, 'bridge' | 'doctor' | 'appIpc'>): void {
  createRoot(root).render(
    <StrictMode>
      <BrandThemeProvider>
        <DesktopThemeLayer>
          <DesktopShell {...host} />
        </DesktopThemeLayer>
      </BrandThemeProvider>
    </StrictMode>,
  );
}

/** `invoke`: the host's, or `undefined` in a browser tab; `./main.tsx` already installed the diagnostics on it. */
export function start(invoke: HostInvoke | undefined): void {
  const container = document.getElementById('root');
  if (!container) {
    throw new Error('elitea-web desktop: #root container missing from index.html');
  }

  const bridge = invoke === undefined ? undefined : createHostBridge(invoke);
  const doctor = invoke === undefined ? undefined : createDoctorIpc(invoke);
  const appIpc = tauriAppIpc();
  // Inside the host only: the dev harness in a browser keeps its devtools menu.
  if (invoke !== undefined) installContextMenuGuard();

  // DEV ONLY: `/?harness` shows the signed-in shell with a fake host and canned
  // data (`devHarness.tsx`). `import.meta.env.DEV` is false in every build, so
  // the branch and the harness's chunk are dropped from it.
  if (import.meta.env.DEV && bridge === undefined && new URLSearchParams(window.location.search).has('harness')) {
    void import('./devHarness').then(({ mountDesktopHarness }) => mountDesktopHarness(container));
  } else {
    renderShell(container, { bridge, doctor, appIpc });
  }
}
