/**
 * Everything the desktop webview keeps locally, cleared in one call.
 *
 * `clearNamespace()` (the logout sweep) removes the `el.*` keys; this goes
 * further because a wipe after `device_revoked` must leave nothing of the
 * previous session: raw keys outside the namespace, and IndexedDB. The host
 * additionally clears the webview's browsing data (cookies, caches) and is the
 * authority; this runs first so the page does not rewrite state meanwhile.
 */
import { clearNamespace } from '@/shared/lib/storage';

export async function clearAllLocalData(): Promise<void> {
  try {
    clearNamespace();
  } catch {
    // Storage may be unavailable; the host-side clear still runs.
  }
  // oxlint-disable-next-line elitea/no-raw-webstorage -- a wipe must also remove keys outside the `el.` namespace, which the wrapper cannot see.
  for (const area of [globalThis.localStorage, globalThis.sessionStorage]) {
    try {
      area.clear();
    } catch {
      // See above.
    }
  }
  await deleteDatabases();
}

async function deleteDatabases(): Promise<void> {
  const factory = (globalThis as { indexedDB?: IDBFactory }).indexedDB;
  if (factory === undefined || typeof factory.databases !== 'function') return;
  const databases = await factory.databases().catch(() => []);
  await Promise.all(
    databases.map(
      ({ name }) =>
        new Promise<void>((resolve) => {
          if (name === undefined) {
            resolve();
            return;
          }
          const request = factory.deleteDatabase(name);
          request.onsuccess = () => resolve();
          request.onerror = () => resolve();
          request.onblocked = () => resolve();
        }),
    ),
  );
}
