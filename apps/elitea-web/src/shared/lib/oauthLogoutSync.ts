/** Credential-scoped logout markers. Tokens remain in tab-local session storage. */
import { createStorage, STORAGE_NAMESPACE } from './storage';

export function getLogoutMarkerStorageKey(storageKey: string | null): string | null {
  return storageKey ? `mcp.logout:${storageKey}` : null;
}

export function getLogoutMarkerEventKey(storageKey: string | null): string | null {
  const key = getLogoutMarkerStorageKey(storageKey);
  return key ? `${STORAGE_NAMESPACE}${key}` : null;
}

export function loadLogoutMarker(storageKey: string): number {
  const key = getLogoutMarkerStorageKey(storageKey);
  if (!key || typeof window === 'undefined') return 0;
  const marker = Number(createStorage('local').get(key));
  return Number.isFinite(marker) && marker > 0 ? marker : 0;
}

export function publishLogout(storageKey: string): void {
  if (typeof window === 'undefined') return;
  const key = getLogoutMarkerStorageKey(storageKey);
  if (!key) return;
  const previous = loadLogoutMarker(storageKey);
  createStorage('local').set(key, String(Math.max(Date.now(), previous + 1)));
}
