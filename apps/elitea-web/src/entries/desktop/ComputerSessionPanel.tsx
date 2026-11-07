/**
 * The computer use session panel (ADR-0047 decision 6, #1584): the page of
 * the host's always-on-top `computer-panel` window (`index.html#computer-panel`).
 * It shows the app the agent uses and its last action, with **Pause** and
 * **Stop**; paused, it says why and offers **Resume**. The host shows the
 * window while a session is active or paused and hides it when it ends; the
 * menu-bar item mirrors the same state.
 *
 * The window's capability grants `computer_session` and event listening only.
 */
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { IDLE_SESSION, type ComputerIpc, type SessionAction, type SessionView } from '@/shared/desktop/computerIpc';
import { t } from '@/shared/i18n';

/** What the panel says, by state. */
export function panelText(view: SessionView): { title: string; detail: string | null } {
  if (view.phase === 'paused') {
    return {
      title:
        view.paused_by === 'user_active'
          ? t('desktop.computerPanel.pausedUserActive', 'Paused, you are using the computer')
          : t('desktop.computerPanel.paused', 'Paused'),
      detail: t('desktop.computerPanel.resumeHint', 'The agent waits until you press Resume.'),
    };
  }
  if (view.phase === 'stopped') return { title: t('desktop.computerPanel.stopped', 'Stopped'), detail: null };
  if (view.phase === 'idle') return { title: t('desktop.computerPanel.idle', 'Not using the computer'), detail: null };
  return {
    title: view.app === null ? t('desktop.computerPanel.active', 'Using the computer') : t('desktop.computerPanel.using', 'Using {{app}}', { app: view.app }),
    detail: view.action,
  };
}

export function ComputerSessionPanel({ ipc }: { readonly ipc: ComputerIpc }): React.JSX.Element {
  const [view, setView] = useState<SessionView>(IDLE_SESSION);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let live = true;
    let unlisten: (() => void) | undefined;
    void ipc
      .onSession((next) => {
        if (live) setView(next);
      })
      .then((stop) => {
        if (live) unlisten = stop;
        else stop();
      });
    void ipc
      .session('get')
      .then((current) => {
        if (live) setView(current);
      })
      .catch(() => undefined);
    return () => {
      live = false;
      unlisten?.();
    };
  }, [ipc]);

  const press = (action: SessionAction): void => {
    setBusy(true);
    ipc
      .session(action)
      .then(setView)
      .catch(() => undefined)
      .finally(() => setBusy(false));
  };

  const { title, detail } = panelText(view);
  const live = view.phase === 'active' || view.phase === 'paused';
  return (
    <Box component="output" data-testid="computer-panel" data-phase={view.phase} aria-live="polite" sx={{ display: 'flex', alignItems: 'center', gap: 1.5, paddingX: 2, paddingY: 1.5, height: '100vh', boxSizing: 'border-box' }}>
      <Box sx={{ minWidth: 0, flexGrow: 1 }}>
        <Typography variant="labelMedium" noWrap component="p" data-testid="computer-panel-title">
          {title}
        </Typography>
        {detail !== null ? (
          <Typography variant="bodySmall" color="text.secondary" noWrap component="p" data-testid="computer-panel-detail">
            {detail}
          </Typography>
        ) : null}
      </Box>
      {view.phase === 'active' ? (
        <Button size="small" disabled={busy} onClick={() => press('pause')} data-testid="computer-panel-pause">
          {t('desktop.computerPanel.pause', 'Pause')}
        </Button>
      ) : null}
      {view.phase === 'paused' ? (
        <Button size="small" variant="contained" disabled={busy} onClick={() => press('resume')} data-testid="computer-panel-resume">
          {t('desktop.computerPanel.resume', 'Resume')}
        </Button>
      ) : null}
      {live ? (
        <Button size="small" color="error" disabled={busy} onClick={() => press('stop')} data-testid="computer-panel-stop">
          {t('desktop.computerPanel.stop', 'Stop')}
        </Button>
      ) : null}
    </Box>
  );
}
