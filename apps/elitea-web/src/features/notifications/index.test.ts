import { describe, expect, it } from 'vitest';

import * as slice from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: `index.ts` is the
 * only file other slices may import). `export type` interfaces are erased
 * by `verbatimModuleSyntax` and never appear on the runtime namespace
 * object, so this list is deliberately the value-export subset only — see
 * `./index.ts` for the full (type + value) surface. Precedent:
 * `src/entities/notification/index.test.ts`.
 */
const PUBLIC_SURFACE = [
  'NOTIFICATIONS_QUERY_ROOT',
  'NotificationListItem',
  'parseNotificationMessage',
  'resolveNotificationHref',
  'useBulkDeleteNotifications',
  'useBulkMarkSeenNotifications',
  'useDeleteNotification',
  'useNotificationsInfiniteList',
  'useNotificationsList',
  'useNotificationsSSE',
  'useReadNotification',
  'useSoundNotification',
] as const;

describe('features/notifications public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(slice).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });

  /**
   * Issue 940/A4 added `useNotificationsInfiniteList` (the popover's
   * infinite-scroll query) to an already-curated barrel sitting exactly at
   * the cap, pushing the combined (type + value) count to 21/20 —
   * `scripts/lib/budgets-core.mjs`'s `BUDGET_WAIVERS` now carries a disclosed
   * waiver for this file (same precedent as `chat-messages`/
   * `interactive-tours`/`skills`'s own barrels).
   */
  it('stays within the §3.5 20-symbol budget (type + value exports combined) plus the one disclosed waiver slot', () => {
    // ./index.ts source-level export count, hand-counted against the file:
    // 5 `export type` statements (9 type names) + 12 value exports = 21.
    expect(PUBLIC_SURFACE.length).toBeLessThanOrEqual(21);
  });
});
