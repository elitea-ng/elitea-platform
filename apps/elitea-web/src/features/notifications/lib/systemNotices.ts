/**
 * The desktop's "Show system notifications" switch (Settings › Notifications),
 * on by default and remembered per viewer (`el.desktop.systemNotifications`),
 * and the text a native notice shows for a notification: its kind as the
 * title, its message as the body.
 *
 * The settings page and the desktop's notification feed read the one stored
 * value; a change reaches every reader at once.
 */
import { useSyncExternalStore } from 'react';

import { createStorage } from '@/shared/lib/storage';

import type { NormalizedNotification } from '../api/normalize';
import { parseNotificationMessage } from './routes';
import { runFinishedText } from './runFinished';

const KEY = 'desktop.systemNotifications';

/** Whether a new notification shows a native notice (the desktop, while its window is in the background). */
export function systemNoticesEnabled(): boolean {
  try {
    return (
      createStorage('local').getJSON(KEY, (raw) => {
        if (typeof raw !== 'boolean') throw new Error('invalid');
        return raw;
      }) ?? true
    );
  } catch {
    return true;
  }
}

const listeners = new Set<() => void>();

export function setSystemNoticesEnabled(enabled: boolean): void {
  try {
    createStorage('local').setJSON(KEY, enabled);
  } catch {
    // Not remembered.
  }
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => void listeners.delete(listener);
}

export function useSystemNoticesEnabled(): boolean {
  return useSyncExternalStore(subscribe, systemNoticesEnabled, () => true);
}

/** `workspace_reclaim_requested` → "Workspace reclaim requested". */
function kindTitle(eventType: string): string {
  const words = eventType.replaceAll('_', ' ').trim();
  return words === '' ? 'Notification' : `${words.charAt(0).toUpperCase()}${words.slice(1)}`;
}

/**
 * A native notice's title (the kind) and body (the message, links as their
 * text). A `run_finished` row has no message (it is content-free): its text
 * is the fixed one for its outcome.
 */
export function systemNoticeText(notification: NormalizedNotification): { readonly title: string; readonly body: string } {
  if (notification.eventType === 'run_finished') return runFinishedText(notification.meta?.outcome);
  const body = parseNotificationMessage(notification.meta?.message)
    .map((segment) => segment.text)
    .join('');
  return { title: kindTitle(String(notification.eventType)), body };
}
