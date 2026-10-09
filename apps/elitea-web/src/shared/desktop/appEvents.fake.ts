/**
 * An in-memory `AppIpc` for tests and Storybook. `emit` plays the host's
 * side of `app://command` (a menu item, a drop), so a test can drive the
 * shell exactly.
 */
import type { AppCommand, AppCommandHandler, AppIpc, AppPlatform, NativeWindowTheme } from './appEvents';
import { BROWSER_PLATFORM } from './appEvents';
import { WorkspaceIpcError } from './workspaceIpc';

export interface FakeAppIpc extends AppIpc {
  emit(command: AppCommand): void;
  readonly calls: {
    revealed: { workspaceId: string; path: string }[];
    opened: { workspaceId: string; path: string }[];
    themes: NativeWindowTheme[];
  };
  /** Make the next `revealPath` / `openPath` reject the way the host does. */
  failNext(command: 'revealPath' | 'openPath', code: string, message: string): void;
  subscriberCount(): number;
}

export const MACOS_PLATFORM: AppPlatform = { os: 'macos', titlebar_overlay: true, traffic_light_inset_px: 90, vibrancy: true };

export function createFakeAppIpc(platform: AppPlatform = BROWSER_PLATFORM): FakeAppIpc {
  const handlers = new Set<AppCommandHandler>();
  const calls: FakeAppIpc['calls'] = { revealed: [], opened: [], themes: [] };
  const failures = new Map<'revealPath' | 'openPath', WorkspaceIpcError>();
  const settle = (command: 'revealPath' | 'openPath'): Promise<void> => {
    const error = failures.get(command);
    if (error === undefined) return Promise.resolve();
    failures.delete(command);
    return Promise.reject(error);
  };
  return {
    calls,
    platform: () => Promise.resolve({ ...platform }),
    revealPath(workspaceId, path) {
      calls.revealed.push({ workspaceId, path });
      return settle('revealPath');
    },
    openPath(workspaceId, path) {
      calls.opened.push({ workspaceId, path });
      return settle('openPath');
    },
    setWindowTheme(theme) {
      calls.themes.push(theme);
      return Promise.resolve();
    },
    onCommand(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
    emit(command) {
      Array.from(handlers).forEach((handler) => handler(command));
    },
    failNext(command, code, message) {
      failures.set(command, new WorkspaceIpcError(code, message));
    },
    subscriberCount: () => handlers.size,
  };
}
