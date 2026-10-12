/**
 * Everything the desktop shell can be asked to do, from any of its four
 * doors: a keyboard shortcut, the command palette, a sidebar click, or the
 * host's `app://command` (its menu bar, dock and global shortcuts). One
 * vocabulary, so the four cannot drift apart.
 */
import type { AppCommand, AppCommandId } from '@/shared/desktop/appEvents';

export type ShellAction =
  | { type: 'open_folder' }
  | { type: 'new_thread' }
  | { type: 'settings' }
  | { type: 'command_palette' }
  | { type: 'toggle_sidebar' }
  | { type: 'toggle_changes' }
  | { type: 'toggle_plan' }
  | { type: 'back' }
  | { type: 'forward' }
  /** The folders home: the last folder/thread, else the start screen. */
  | { type: 'folders' }
  /** One of the Elitea web pages (`/agents`, `/settings`, …). */
  | { type: 'go'; to: string }
  | { type: 'open_workspace'; workspaceId: string; conversationId?: string }
  /** The host opened a folder itself (`workspace_opened`): re-read the list and go there. */
  | { type: 'workspaces_changed'; workspaceId: string }
  /** Something to tell the person (a failed Open Folder…, a drop the shell cannot use). */
  | { type: 'notice'; message: string }
  /** Files (not a folder) were dropped on the window. */
  | { type: 'files_dropped' };

const PLAIN_COMMANDS = new Set<string>(['new_thread', 'settings', 'command_palette', 'toggle_sidebar', 'toggle_changes', 'back', 'forward']);

const workspaceIdOf = (args: unknown): string | undefined => {
  const id = (args as { workspace_id?: unknown } | undefined)?.workspace_id;
  return typeof id === 'string' && id !== '' ? id : undefined;
};

/** The commands that carry arguments. */
const WITH_ARGS: Partial<Record<AppCommandId, (args: unknown) => ShellAction | undefined>> = {
  // The host ran its own folder picker (menu, dock, a folder dropped on the window).
  workspace_opened: (args) => {
    const workspaceId = workspaceIdOf(args);
    return workspaceId === undefined ? undefined : { type: 'workspaces_changed', workspaceId };
  },
  workspace_open_failed: (args) => {
    const message = (args as { message?: unknown } | undefined)?.message;
    return { type: 'notice', message: typeof message === 'string' ? message : '' };
  },
  // Files (not folders) dropped on the window: only a folder opens as a workspace.
  files_dropped: () => ({ type: 'files_dropped' }),
  // Reserved by the host (not sent yet); harmless to honour.
  focus_turn: (args) => {
    const workspaceId = workspaceIdOf(args);
    return workspaceId === undefined ? undefined : { type: 'open_workspace', workspaceId };
  },
};

/** The host's command as a shell action; `undefined` for an unknown id or unusable arguments. */
export function actionForAppCommand(command: AppCommand): ShellAction | undefined {
  if (PLAIN_COMMANDS.has(command.id)) return { type: command.id } as ShellAction;
  return WITH_ARGS[command.id]?.(command.args);
}

interface KeyLike {
  key: string;
  code: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

/**
 * The shell's shortcuts: ⌘K palette, ⌘N new thread, ⌘O open folder,
 * ⌘\ sidebar, ⌘⌥\ changes panel, ⌘, settings (Ctrl on Windows/Linux).
 * `code` for the backslash: with ⌥ held, macOS changes the produced `key`.
 */
export function actionForShortcut(event: KeyLike): ShellAction | undefined {
  if (!(event.metaKey || event.ctrlKey) || event.shiftKey) return undefined;
  if (event.code === 'Backslash') return { type: event.altKey ? 'toggle_changes' : 'toggle_sidebar' };
  if (event.altKey) return undefined;
  switch (event.key.toLowerCase()) {
    case 'k':
      return { type: 'command_palette' };
    case 'n':
      return { type: 'new_thread' };
    case 'o':
      return { type: 'open_folder' };
    case ',':
      return { type: 'settings' };
    default:
      return undefined;
  }
}
