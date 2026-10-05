/**
 * Test-environment Web Storage shim (carry-forward item "action for M1",
 * `elitea-docs/.../ui/decisions-ui-reimplementation-2026-07-26.md`).
 *
 * WHY THIS EXISTS — the failure it fixes, reproduced 2026-08-06:
 *
 *   ExperimentalWarning: localStorage is not available because
 *   --localstorage-file was not provided.
 *   TypeError: Cannot read properties of undefined (reading 'getItem')
 *       ... in <Sidebar> / <AppShell>
 *
 * Under vitest 4 on Node 24, Node ships its OWN experimental `localStorage`
 * global. In the `node` (jsdom) vitest project that global SHADOWS jsdom's
 * implementation, and because the process is started without
 * `--localstorage-file` Node's version resolves to `undefined`. The result is
 * that `window.localStorage` — which jsdom would otherwise provide — is
 * `undefined`, so any component reading it during an effect throws.
 *
 * This is an environment defect, not a product one: the app code is correct
 * and works in a real browser. The shim therefore installs a REAL in-memory
 * `Storage` only when the global is missing or broken, and never replaces a
 * working implementation — so if a future Node/jsdom/vitest combination fixes
 * the shadowing, this becomes inert rather than silently taking over.
 *
 * Deliberately NOT a mock (§6.2: "mocks stop at the network boundary"). It is
 * a faithful `Storage` implementation — same semantics for `length`,
 * `key(n)`, enumerable stored keys (`Object.keys(storage)`), string
 * coercion of keys and values, and `removeItem` on a missing
 * key being a no-op — so tests exercise real storage behaviour.
 */

/**
 * The `Storage` methods live on a prototype, so they are neither own nor
 * enumerable — exactly as on a real `Storage`, whose methods come from
 * `Storage.prototype`.
 */
const storageMethods = {
  get length(): number {
    return entriesOf(this).size;
  },
  key(index: number): string | null {
    return [...entriesOf(this).keys()][index] ?? null;
  },
  getItem(key: string): string | null {
    return entriesOf(this).get(String(key)) ?? null;
  },
  setItem(key: string, value: string): void {
    entriesOf(this).set(String(key), String(value));
  },
  removeItem(key: string): void {
    entriesOf(this).delete(String(key));
  },
  clear(): void {
    entriesOf(this).clear();
  },
};

const backing = new WeakMap<object, Map<string, string>>();

function entriesOf(storage: object): Map<string, string> {
  const entries = backing.get(storage);
  if (entries === undefined) throw new TypeError('Illegal invocation');
  return entries;
}

/**
 * A real `Storage` exposes each stored key as its own enumerable property, so
 * `Object.keys(localStorage)` and `key in localStorage` see the data. The
 * first version of this shim was an object literal of methods, which made
 * `Object.keys` list `length`/`key`/`getItem`/... instead — invisible on Node
 * 24 (jsdom's real Storage wins there, the shim stays inert) and a failing
 * test on Node >= 25, where Node's own `localStorage` getter shadows jsdom's
 * and the shim is what every test actually talks to.
 *
 * The proxy answers enumeration and named-property reads from the backing
 * map. Methods still win over a same-named key (`setItem('getItem', ..)`
 * does not break `getItem`), matching the spec's "override built-ins" rule.
 */
export function createMemoryStorage(): Storage {
  const target = Object.create(storageMethods) as Storage;
  const entries = new Map<string, string>();
  backing.set(target, entries);
  const proxy = new Proxy(target, {
    get(obj, prop, receiver) {
      if (typeof prop === 'string' && !(prop in storageMethods) && entries.has(prop)) {
        return entries.get(prop);
      }
      return Reflect.get(obj, prop, receiver) as unknown;
    },
    has(obj, prop) {
      return (typeof prop === 'string' && entries.has(prop)) || Reflect.has(obj, prop);
    },
    ownKeys() {
      return [...entries.keys()];
    },
    getOwnPropertyDescriptor(_obj, prop) {
      if (typeof prop !== 'string' || !entries.has(prop)) return undefined;
      return { value: entries.get(prop), writable: true, enumerable: true, configurable: true };
    },
  });
  // Methods run with `this` = the proxy (`storage.getItem(..)`), and with
  // `this` = the target when a caller detaches them; both find the map.
  backing.set(proxy, entries);
  return proxy;
}

function isUsable(candidate: unknown): boolean {
  if (candidate === undefined || candidate === null) return false;
  try {
    const storage = candidate as Storage;
    const probe = '__elitea_probe__';
    storage.setItem(probe, '1');
    storage.removeItem(probe);
    return true;
  } catch {
    return false;
  }
}

/**
 * Install an in-memory `localStorage`/`sessionStorage` on `window` and
 * `globalThis` wherever the environment does not already provide a working
 * one. Idempotent. Returns the names it had to shim, so a caller (or a test)
 * can assert on what the environment was missing.
 */
export function installWebStorageShim(): readonly string[] {
  const shimmed: string[] = [];
  const targets: readonly (typeof globalThis)[] =
    typeof window === 'undefined' || (window as unknown) === globalThis
      ? [globalThis]
      : [globalThis, window];

  for (const name of ['localStorage', 'sessionStorage'] as const) {
    if (isUsable((globalThis as Record<string, unknown>)[name])) continue;
    const storage = createMemoryStorage();
    for (const target of targets) {
      Object.defineProperty(target, name, {
        value: storage,
        configurable: true,
        writable: true,
      });
    }
    shimmed.push(name);
  }
  return shimmed;
}
