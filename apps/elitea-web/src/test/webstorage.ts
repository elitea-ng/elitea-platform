/**
 * Test-environment Web Storage shim (added by unit F4; lives in `src/test/`
 * so it is outside the production tree and outside the coverage denominator,
 * and so the storage fence it necessarily bypasses is not app code).
 *
 * Under vitest 4 + Node 24, Node's own experimental `localStorage` global
 * (undefined unless `--localstorage-file` is passed) shadows jsdom's, so
 * `window.localStorage` is `undefined` in the `node` test project while
 * `sessionStorage` (Node's in-memory one) works. This installs a minimal
 * spec-shaped in-memory `Storage` for whichever area is missing.
 *
 * R-M1 (§6.5) sanctions exactly this: "browser APIs jsdom lacks".
 *
 * NOTE for M1 (owner of `src/test/setup.ts`): calling `installWebStorageShim()`
 * from the shared setup file would let every unit inherit it and let this
 * per-file import disappear. F4 may not edit setup.ts, hence the split.
 */

import { createMemoryStorage } from '@/shared/lib/webstorage.testshim';

// One implementation for both shims, so the faithful `Storage` semantics
// (enumerable stored keys included) cannot drift between them.
/** Installs in-memory storage for any missing area; idempotent. */
export function installWebStorageShim(): void {
  for (const name of ['localStorage', 'sessionStorage'] as const) {
    const holder = globalThis as unknown as Record<string, Storage | undefined>;
    if (holder[name] === undefined) {
      Object.defineProperty(globalThis, name, {
        value: createMemoryStorage(),
        writable: true,
        configurable: true,
      });
    }
  }
}
