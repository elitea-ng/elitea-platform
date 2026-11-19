/**
 * Namespaced client-storage wrapper — the ONLY file that touches
 * `localStorage` / `sessionStorage` (spec §5.4; fenced by the F2 oxlint
 * `no-restricted-globals` override that allows the raw globals here only).
 *
 * Every key is prefixed `el.` so a complete logout is a mechanical sweep
 * (`clearNamespace()`), fixing the old app's leak: its logout cleared 2
 * sessionStorage keys and left `elitea_ui.project.id`/`.name`,
 * `elitea_mcp_tokens_v1` and the tour keys behind
 * (apps/elitea-ui/src/slices/user.js:24-27).
 *
 * §5.4 enforcement mechanism: in dev/test a write tracker enumerates every
 * write made through this wrapper, so the logout test can assert
 * (write-set − cleared-set) = ∅ instead of trusting a hand-maintained list.
 */

export const STORAGE_NAMESPACE = 'el.';

export type StorageAreaName = 'local' | 'session';

export interface NamespacedStorage {
  readonly area: StorageAreaName;
  /** Raw string read; `null` when absent. `key` is the logical (unprefixed) key. */
  get(key: string): string | null;
  set(key: string, value: string): void;
  remove(key: string): void;
  /**
   * JSON read. Returns `null` for absent keys, malformed JSON, or values the
   * optional `validate` function rejects (by throwing or returning
   * `undefined`) — corrupt persisted state is treated as absent, never thrown.
   */
  getJSON<T>(key: string, validate?: (raw: unknown) => T): T | null;
  setJSON(key: string, value: unknown): void;
  /** Logical (unprefixed) keys currently present in this area's namespace. */
  keys(): string[];
}

function areaOf(area: StorageAreaName): Storage {
  return area === 'local' ? window.localStorage : window.sessionStorage;
}

function namespacedKeys(store: Storage): string[] {
  const found: string[] = [];
  for (let i = 0; i < store.length; i++) {
    const key = store.key(i);
    if (key !== null && key.startsWith(STORAGE_NAMESPACE)) found.push(key);
  }
  return found;
}

/* ── §5.4 write enumeration (dev/test only) ──────────────────────────────── */

/** Entries are `${area}:${underlyingKey}`, e.g. `local:el.project.id`. */
let writeTracker: Set<string> | null = null;

/**
 * Starts enumerating every write made through this wrapper. Dev/test only —
 * the §5.4 logout-completeness test is its sole intended consumer.
 */
export function enableStorageWriteTracking(): void {
  if (!import.meta.env.DEV) {
    throw new Error('storage: write tracking is a dev/test mechanism (§5.4) and is disabled in production builds');
  }
  writeTracker ??= new Set();
}

export function disableStorageWriteTracking(): void {
  writeTracker = null;
}

/** Snapshot of tracked writes; empty when tracking was never enabled. */
export function trackedStorageWrites(): ReadonlySet<string> {
  return new Set(writeTracker ?? []);
}

/* ── factory ─────────────────────────────────────────────────────────────── */

export function createStorage(area: StorageAreaName): NamespacedStorage {
  const store = areaOf(area);
  const prefixed = (key: string): string => STORAGE_NAMESPACE + key;

  const set = (key: string, value: string): void => {
    const underlying = prefixed(key);
    store.setItem(underlying, value);
    writeTracker?.add(`${area}:${underlying}`);
  };

  const get = (key: string): string | null => store.getItem(prefixed(key));

  return {
    area,
    get,
    set,
    remove(key) {
      store.removeItem(prefixed(key));
    },
    getJSON<T>(key: string, validate?: (raw: unknown) => T): T | null {
      const raw = get(key);
      if (raw === null) return null;
      try {
        const parsed: unknown = JSON.parse(raw);
        if (validate === undefined) return parsed as T;
        const validated = validate(parsed);
        return validated === undefined ? null : validated;
      } catch {
        // Handled (§3.6): corrupt or schema-invalid persisted state is not a
        // programmer error — treat it as absent rather than crashing the app.
        return null;
      }
    },
    setJSON(key, value) {
      set(key, JSON.stringify(value));
    },
    keys() {
      return namespacedKeys(store).map((k) => k.slice(STORAGE_NAMESPACE.length));
    },
  };
}

const clearListeners = new Set<() => void>();

/**
 * Runs `listener` whenever {@link clearNamespace} is about to sweep: a module
 * that keeps a parsed copy of a stored value drops it here, so the sweep is
 * not answered from memory afterwards. Returns the unsubscribe.
 */
export function onNamespaceCleared(listener: () => void): () => void {
  clearListeners.add(listener);
  return () => {
    clearListeners.delete(listener);
  };
}

/**
 * §5.4 complete logout: removes EVERY key under the `el.` namespace from BOTH
 * `localStorage` and `sessionStorage`. Keys outside the namespace (other apps
 * sharing the origin) are untouched.
 */
export function clearNamespace(): void {
  for (const area of ['local', 'session'] as const) {
    const store = areaOf(area);
    for (const key of namespacedKeys(store)) {
      store.removeItem(key);
    }
  }
  // After the sweep, and each on its own: a listener that throws must neither
  // stop the logout nor leave the others holding pre-sweep data.
  for (const listener of clearListeners) {
    try {
      listener();
    } catch {
      // A cache that cannot drop itself is not worth failing a sign-out for.
    }
  }
}
