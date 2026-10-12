/**
 * The shell's wiring to the window it lives in: the chrome the host drew
 * (`app_platform`), the host's `app://command` events, the keyboard
 * shortcuts and the persisted pane layout.
 *
 * Shortcuts have ONE owner. When the host answers `app_platform`, its menu
 * bar owns ⌘K/⌘N/⌘O/⌘,/⌘\/⌘⌥\/⌘[/⌘] and sends them as `app://command`; the
 * page then binds none of them (both would fire). Without a host (a browser
 * tab, an older app) the page binds them itself.
 */
import { useEffect } from 'react';

import { BROWSER_PLATFORM, type AppCommand, type AppIpc } from '@/shared/desktop/appEvents';

import { actionForAppCommand, actionForShortcut, type ShellAction } from '../lib/shellActions';
import { useDesktopLayout } from './desktopLayout.store';
import { readPersistedLayout, writePersistedLayout } from './desktopLayoutPersistence';

export function useHostIntegration(run: (action: ShellAction) => void, appIpc: AppIpc | undefined): void {
  const hostMenu = useDesktopLayout((state) => state.hostMenu);

  // The window chrome; an answer also means the host's menu owns the shortcuts.
  useEffect(() => {
    let live = true;
    if (appIpc === undefined) {
      useDesktopLayout.getState().setPlatform(BROWSER_PLATFORM, false);
      return undefined;
    }
    appIpc.platform().then(
      (platform) => {
        if (live) useDesktopLayout.getState().setPlatform(platform, true);
      },
      () => {
        if (live) useDesktopLayout.getState().setPlatform(BROWSER_PLATFORM, false);
      },
    );
    return () => {
      live = false;
    };
  }, [appIpc]);

  // The persisted layout: read once, then written on every change.
  useEffect(() => {
    const stored = readPersistedLayout();
    if (stored !== null) useDesktopLayout.setState(stored);
    return useDesktopLayout.subscribe((state) =>
      writePersistedLayout({ sidebarOpen: state.sidebarOpen, changesOpen: state.changesOpen, changesWidth: state.changesWidth }),
    );
  }, []);

  // The host's menu, dock and window drops.
  useEffect(() => {
    if (appIpc === undefined) return undefined;
    let off: (() => void) | undefined;
    let disposed = false;
    const handle = (command: AppCommand): void => {
      const action = actionForAppCommand(command);
      if (action !== undefined) run(action);
    };
    appIpc
      .onCommand(handle)
      .then((unsubscribe) => {
        if (disposed) {
          unsubscribe();
          return undefined;
        }
        off = unsubscribe;
        // Live now: the host hands over what it sent before (a dock drop at launch).
        return appIpc.ready().then((pending) => {
          if (!disposed) pending.forEach(handle);
        });
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      off?.();
    };
  }, [appIpc, run]);

  // In-page shortcuts, only while no host menu owns them.
  useEffect(() => {
    if (hostMenu) return undefined;
    const onKey = (event: KeyboardEvent): void => {
      const action = actionForShortcut(event);
      if (action === undefined) return;
      event.preventDefault();
      run(action);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [run, hostMenu]);
}
