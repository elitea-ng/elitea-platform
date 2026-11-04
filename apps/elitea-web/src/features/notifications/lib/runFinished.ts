/**
 * `run_finished` (ADR-0045 decision 5, U3 contract §1/§6): a chain of runs
 * ended. The row is content-free — an outcome and ids, never a prompt, an
 * answer or a title — so its text is fixed per outcome. The same words go
 * into the bell row, the desktop's native notice and (in English, without
 * the catalogue) the web push service worker (`entries/push-sw/pushWorker.ts`).
 */
import { t } from '@/shared/i18n';

import type { FullNotificationMeta } from '../api/normalize';

export interface RunFinishedText {
  readonly title: string;
  readonly body: string;
}

/** The fixed text for a `run_finished` outcome. An unknown outcome reads as a plain finish. */
export function runFinishedText(outcome: string | undefined): RunFinishedText {
  switch (outcome) {
    case 'failed':
      return { title: t('notifications.runFinished.failed.title', 'Run failed'), body: t('notifications.runFinished.failed.body', 'A run stopped with an error.') };
    case 'waiting':
      return { title: t('notifications.runFinished.waiting.title', 'Run needs you'), body: t('notifications.runFinished.waiting.body', 'A run waits for your answer.') };
    case 'limit_reached':
      return {
        title: t('notifications.runFinished.limitReached.title', 'Run stopped at its limit'),
        body: t('notifications.runFinished.limitReached.body', 'A run reached its chain limit.'),
      };
    case undefined:
    default:
      return { title: t('notifications.runFinished.completed.title', 'Run finished'), body: t('notifications.runFinished.completed.body', 'A run is done.') };
  }
}

/** Where a click on a `run_finished` row goes. */
export type RunFinishedTarget =
  | { readonly kind: 'conversation'; readonly projectId: string; readonly conversationUuid: string }
  | { readonly kind: 'thread'; readonly threadId: string };

function present(value: string | number | undefined): string | undefined {
  if (value === undefined) return undefined;
  const text = String(value);
  return text === '' ? undefined : text;
}

/**
 * A desktop chain opens its thread, but only inside the desktop app (only
 * that computer has the thread). Every other row with a conversation opens
 * the conversation; a desktop-only thread seen in a browser has no target.
 * The project is the row's meta project, else the row's own.
 */
export function runFinishedTarget(meta: FullNotificationMeta | undefined, rowProjectId: string | undefined, inDesktopApp: boolean): RunFinishedTarget | null {
  const threadId = present(meta?.threadId);
  if (inDesktopApp && meta?.origin === 'desktop' && threadId !== undefined) return { kind: 'thread', threadId };
  const conversationUuid = present(meta?.conversationUuid);
  const projectId = present(meta?.projectId) ?? present(rowProjectId);
  if (conversationUuid === undefined || projectId === undefined) return null;
  return { kind: 'conversation', projectId, conversationUuid };
}
