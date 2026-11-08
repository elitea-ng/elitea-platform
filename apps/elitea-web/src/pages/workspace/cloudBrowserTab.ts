/**
 * Which browser a Local work thread's Browser tab shows (ADR-0048 decision
 * 7, #1592). On this computer, the desktop's own pane. Away from it (its
 * executor is the cloud, ADR-0034), the cloud run's browser streamed: the
 * session of the newest answer whose run used the browser tools, read from
 * the cloud copy's transcript as the web chat reads its own.
 */
import { cloudBrowserSessionOf } from '@/pages/chat/cloudBrowserSession';
import { browserViewAvailable } from '@/shared/api/browserView/client';

/**
 * `undefined`: the local pane (a thread on this computer). A session name:
 * the streamed view. `null`: no Browser tab (no run used the browser yet,
 * or this build cannot open the view).
 */
export function cloudBrowserTabSession(offLaptop: boolean, history: readonly unknown[]): string | null | undefined {
  if (!offLaptop) return undefined;
  if (!browserViewAvailable()) return null;
  return cloudBrowserSessionOf(history) ?? null;
}
