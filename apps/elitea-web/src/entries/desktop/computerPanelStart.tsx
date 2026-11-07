/**
 * The computer use session panel's start (ADR-0047 decision 6), imported by
 * `./main.tsx` in the `computer-panel` webview instead of the shell: the
 * panel alone, under the compiled default brand. It calls `computer_session`
 * and listens to `computer://event`, nothing else.
 */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { tauriComputerIpc } from '@/shared/desktop/computerIpc';
import { installContextMenuGuard } from '@/shared/desktop/contextMenu';
import { DesktopThemeLayer } from '@/widgets/desktop-shell';

import { ComputerSessionPanel } from './ComputerSessionPanel';

export function startComputerPanel(): void {
  const container = document.getElementById('root');
  const ipc = tauriComputerIpc();
  if (!container || ipc === undefined) return;
  installContextMenuGuard();
  createRoot(container).render(
    <StrictMode>
      <BrandThemeProvider>
        <DesktopThemeLayer>
          <ComputerSessionPanel ipc={ipc} />
        </DesktopThemeLayer>
      </BrandThemeProvider>
    </StrictMode>,
  );
}
