/**
 * The desktop host's platform notices (`src-tauri/src/notices.rs`): a native
 * OS notification for a new platform notification, and the number of unread
 * ones, which the Dock badge counts with the approvals waiting.
 *
 * The host shows a notice only while the window is not focused (a focused
 * window has the bell), and only with the OS's permission; it cleans the
 * text (no control characters, the body cut to 120 characters) and refuses a
 * title over 120 or a body over 1000 characters. The texts sent here are
 * already within those limits, so a refusal is a bug worth a log line, not a
 * message for the person.
 *
 * Outside the desktop webview (a plain browser tab) there is no host:
 * `platformNotice()` is `undefined`.
 */
import { tauriInvoke, type HostInvoke } from './hostBridge';

const NOTICE_TITLE_MAX = 120;
export const NOTICE_BODY_MAX = 120;

export interface PlatformNotice {
  /** Shows a native notification while the window is not focused. */
  show(title: string, body: string): Promise<void>;
  /** The unread platform notifications, for the Dock badge. */
  setUnread(unread: number): Promise<void>;
}

function clip(text: string, max: number): string {
  const flat = text.replace(/\s+/g, ' ').trim();
  return flat.length <= max ? flat : `${flat.slice(0, max - 1)}…`;
}

export function createPlatformNotice(invoke: HostInvoke): PlatformNotice {
  return {
    show: (title, body) => invoke<void>('platform_notice_show', { title: clip(title, NOTICE_TITLE_MAX), body: clip(body, NOTICE_BODY_MAX) }),
    setUnread: (unread) => invoke<void>('platform_notice_badge', { unread: Math.max(0, Math.floor(unread)) }),
  };
}

/** The host's notices, or `undefined` outside the desktop webview. */
export function platformNotice(): PlatformNotice | undefined {
  const invoke = tauriInvoke();
  return invoke === undefined ? undefined : createPlatformNotice(invoke);
}
