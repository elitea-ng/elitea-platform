/**
 * The typed client for the desktop host's native shell (IPC.md, "App and
 * native shell"): `app_platform`, `reveal_path`, `open_path`, and the
 * `app://command` channel that carries menu items, shortcuts and OS actions
 * (Open Folder…, folder/file drops) to the UI.
 *
 * Wire shapes are the host's (snake_case), not renamed here, like
 * `workspaceIpc.ts`. Failures reject with a `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** `app_platform`: what the web shell needs to lay out its title area. */
export interface AppPlatform {
  os: 'macos' | 'linux' | 'windows';
  /** The content runs under the title bar: draw a `data-tauri-drag-region` and leave room for the window controls. */
  titlebar_overlay: boolean;
  /** Room to leave on the left of the top bar (0 without the overlay). */
  traffic_light_inset_px: number;
  /** The window is transparent over the system sidebar material: a transparent sidebar shows it. */
  vibrancy: boolean;
}

/** One `app://command` payload. `open_folder` is never sent: Open Folder… runs host-side and sends `workspace_opened`. */
export type AppCommand =
  | { id: 'new_thread' | 'settings' | 'command_palette' | 'toggle_sidebar' | 'toggle_changes' | 'back' | 'forward'; args?: undefined }
  | { id: 'workspace_opened'; args: { workspace_id: string } }
  | { id: 'workspace_open_failed'; args: { message: string } }
  /** Absolute paths of files (not folders) dropped on the window. */
  | { id: 'files_dropped'; args: { paths: string[] } }
  /** Reserved: not sent yet (no notification click callback on desktop). */
  | { id: 'focus_turn'; args: { workspace_id: string; turn_id: string } };

export type AppCommandId = AppCommand['id'];
export type AppCommandHandler = (command: AppCommand) => void;

export interface AppIpc {
  platform(): Promise<AppPlatform>;
  /** Show a workspace file or folder in Finder / the file manager; `path` is workspace-relative (`''` is the folder). */
  revealPath(workspaceId: string, path: string): Promise<void>;
  /** Open it with its default app; rejects `open_refused` for anything the OS would run. */
  openPath(workspaceId: string, path: string): Promise<void>;
  /** Subscribe to `app://command`; resolves once live, with its unsubscribe. */
  onCommand(handler: AppCommandHandler): Promise<() => void>;
  /**
   * The native window's appearance (and so its sidebar material): `null`
   * follows the OS. Tauri's own `plugin:window|set_theme`, granted alone
   * (`core:window:allow-set-theme`).
   */
  setWindowTheme(theme: NativeWindowTheme): Promise<void>;
}

export type NativeWindowTheme = 'light' | 'dark' | null;

export const APP_COMMAND_CHANNEL = 'app://command';

/** What a plain browser tab (no host) reports. */
export const BROWSER_PLATFORM: AppPlatform = { os: 'linux', titlebar_overlay: false, traffic_light_inset_px: 0, vibrancy: false };

export function createAppIpc(hostInvoke: HostInvoke, listen: ListenFn): AppIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    (args === undefined ? hostInvoke<T>(command) : hostInvoke<T>(command, args)).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    platform: () => invoke<AppPlatform>('app_platform'),
    revealPath: (workspaceId, path) => invoke<void>('reveal_path', { workspace_id: workspaceId, path }),
    openPath: (workspaceId, path) => invoke<void>('open_path', { workspace_id: workspaceId, path }),
    onCommand: (handler) => listen(APP_COMMAND_CHANNEL, (payload) => handler(payload as AppCommand)),
    setWindowTheme: (theme) => invoke<void>('plugin:window|set_theme', { label: 'main', value: theme }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriAppIpc(): AppIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createAppIpc(bound.invoke, tauriListen(bound));
}
