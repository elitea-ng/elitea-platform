import { isApiError } from '@/api/errors';

/** The copy of the Chats offline, nothing-saved state (design-system §4.9), shared with its `__DEV__` preview. */
export const OFFLINE_EMPTY = {
  title: "You're offline",
  body: 'Nothing was saved on this device for this project yet. Connect to see your chats.',
} as const;

/** A request that never reached the workspace (no network, or it timed out). */
export function isOfflineError(error: unknown): boolean {
  return isApiError(error) && (error.kind === 'network' || error.kind === 'timeout');
}
