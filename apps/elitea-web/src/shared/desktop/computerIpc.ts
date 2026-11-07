/**
 * The typed client for computer use on this device (ADR-0047 decisions 6
 * and 7, #1584; IPC.md "Computer use"): Settings › Computer use
 * (`computer_settings_get` / `_set`, `computer_app_forget`,
 * `computer_open_privacy`) and the session panel (`computer_session` and the
 * `computer://event` channel). Wire shapes are the host's (snake_case);
 * failures reject with a `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** A macOS privacy grant, as the host reads it now. */
export type GrantState = 'granted' | 'denied' | 'unknown' | 'not_required';

/** The System Settings panes the page links to. */
export type PrivacyGrant = 'accessibility' | 'screen_recording' | 'input_monitoring';

interface RememberedApp {
  bundle_id: string;
  /** "Always for this app" to see it. */
  sees: boolean;
  /** "Always for this app" to use it. */
  acts: boolean;
}

export interface ComputerSettings {
  /** The organisation lets computer use run here. */
  policy_allows: boolean;
  /** The person's device switch (off unless turned on). */
  enabled: boolean;
  /** The OS can run it (macOS). */
  supported: boolean;
  accessibility: GrantState;
  screen_recording: GrantState;
  /** The tools that are not offered without Screen Recording. */
  screenshot_tools: string[];
  /** The stop shortcut, for example `ctrl+option+escape`. */
  stop_shortcut: string;
  remembered: RememberedApp[];
}

type SessionPhase = 'idle' | 'active' | 'paused' | 'stopped';

/** The session panel's state (`computer://event`, kind `computer_session`). */
export interface SessionView {
  phase: SessionPhase;
  /** `button`: Pause was pressed; `user_active`: the person used the keyboard or the pointer. */
  paused_by: 'button' | 'user_active' | null;
  /** The app the last call was about. */
  app: string | null;
  /** The last action. */
  action: string | null;
}

export type SessionAction = 'get' | 'pause' | 'resume' | 'stop';

export interface ComputerIpc {
  settings(): Promise<ComputerSettings>;
  /** On is refused without the policy or the Accessibility grant. */
  setEnabled(enabled: boolean): Promise<ComputerSettings>;
  /** Revoke "always for this app", seeing and acting. */
  forgetApp(bundleId: string): Promise<ComputerSettings>;
  openPrivacy(grant: PrivacyGrant): Promise<void>;
  session(action: SessionAction): Promise<SessionView>;
  /** Subscribe to the session's changes; resolves once live, with its unsubscribe. */
  onSession(handler: (view: SessionView) => void): Promise<() => void>;
}

export const COMPUTER_EVENT_CHANNEL = 'computer://event';

export const IDLE_SESSION: SessionView = { phase: 'idle', paused_by: null, app: null, action: null };

export function createComputerIpc(hostInvoke: HostInvoke, listen: ListenFn): ComputerIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    (args === undefined ? hostInvoke<T>(command) : hostInvoke<T>(command, args)).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    settings: () => invoke<ComputerSettings>('computer_settings_get'),
    setEnabled: (enabled) => invoke<ComputerSettings>('computer_settings_set', { enabled }),
    forgetApp: (bundleId) => invoke<ComputerSettings>('computer_app_forget', { bundle_id: bundleId }),
    openPrivacy: (grant) => invoke<void>('computer_open_privacy', { grant }),
    session: (action) => invoke<SessionView>('computer_session', { action }),
    onSession: (handler) =>
      listen(COMPUTER_EVENT_CHANNEL, (payload) => {
        const { kind, ...view } = payload as SessionView & { kind?: string };
        if (kind === 'computer_session') handler(view);
      }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriComputerIpc(): ComputerIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = { invoke: internals.invoke.bind(internals), transformCallback: internals.transformCallback.bind(internals) };
  return createComputerIpc(bound.invoke, tauriListen(bound));
}

/** `ctrl+option+escape` as macOS writes it: ⌃⌥⎋. */
export function shortcutLabel(shortcut: string): string {
  const keys: Record<string, string> = { ctrl: '⌃', control: '⌃', option: '⌥', alt: '⌥', cmd: '⌘', command: '⌘', shift: '⇧', escape: '⎋', esc: '⎋' };
  return shortcut
    .split('+')
    .map((key) => keys[key.trim().toLowerCase()] ?? key.trim().toUpperCase())
    .join('');
}
