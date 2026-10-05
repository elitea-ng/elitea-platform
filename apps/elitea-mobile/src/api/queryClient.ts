import { QueryClient, focusManager } from '@tanstack/react-query';
import { Platform, type AppStateStatus } from 'react-native';

import { isApiError } from '@/api/errors';
import { appActivity } from '@/lib/appActivity';
import { registerWipeHook } from '@/security/endSession';
import { getSession, subscribeSession } from '@/state/session';

/**
 * The server-data cache (react-query). Memory only: what must survive offline
 * lives in the encrypted offline store (src/db/), which the list, thread and
 * notification queries fall back to. `endSession` cancels and clears it.
 */

/** Most retries a query makes after its first failure. */
export const MAX_QUERY_RETRIES = 2;

/**
 * Whether a failed query should be retried. Only transient failures are:
 * no answer (network, timeout) or a 5xx / 429. A 4xx is the server's final
 * word, and auth, upgrade, origin and cancellation failures must never repeat.
 */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (failureCount >= MAX_QUERY_RETRIES) return false;
  if (!isApiError(error)) return false;
  switch (error.kind) {
    case 'network':
    case 'timeout':
      return true;
    case 'http':
      return error.status === 429 || (error.status !== undefined && error.status >= 500);
    default:
      return false;
  }
}

/** Retry delay: honours `Retry-After`, else 1 s, 2 s, 4 s … capped at 15 s. */
export function retryDelay(failureCount: number, error: unknown): number {
  if (isApiError(error) && error.retryAfterMs !== undefined) return Math.min(error.retryAfterMs, 30_000);
  return Math.min(1000 * 2 ** failureCount, 15_000);
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Run the query function even when the device looks offline: it falls
        // back to the encrypted offline store itself (src/sync/offlineReads.ts).
        networkMode: 'always',
        retry: shouldRetry,
        retryDelay,
        staleTime: 30_000,
        gcTime: 5 * 60_000,
      },
      mutations: { retry: false },
    },
  });
}

export const queryClient = createQueryClient();

registerWipeHook('query_cache', async () => {
  await queryClient.cancelQueries();
  queryClient.clear();
});

let focusWired = false;

/**
 * Refetch-on-focus follows the app's effective activity on native: the OS
 * foreground state, with a locked app counting as unfocused. Idempotent.
 */
export function wireFocusManager(): void {
  if (focusWired || Platform.OS === 'web') return;
  focusWired = true;
  focusManager.setEventListener((setFocused) => {
    const subscription = appActivity.addEventListener('change', (status: AppStateStatus) => {
      setFocused(status === 'active');
    });
    return () => subscription.remove();
  });
}

/** Whether a query last failed because the workspace refused this app version (426). */
function failedOnVersion(error: unknown): boolean {
  return isApiError(error) && error.kind === 'upgrade_required';
}

/**
 * When the blocking update screen clears (the workspace accepts this version
 * again, DEF-R4), refetch every query a 426 failed. The screen is an overlay,
 * so the screen underneath stays mounted; without this it would still show the
 * 426 as "Couldn't open … Check your connection" until the user taps Retry.
 * Queries that failed for any other reason are left alone. Returns the
 * unsubscribe function.
 */
export function wireUpgradeRecovery(client: QueryClient = queryClient): () => void {
  let blocked = getSession().upgradeRequired !== null;
  return subscribeSession(() => {
    const nowBlocked = getSession().upgradeRequired !== null;
    const cleared = blocked && !nowBlocked;
    blocked = nowBlocked;
    if (!cleared) return;
    void client.invalidateQueries({
      predicate: (query) => query.state.status === 'error' && failedOnVersion(query.state.error),
    });
  });
}
