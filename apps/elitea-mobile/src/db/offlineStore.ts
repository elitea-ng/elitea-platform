import { createDbKey, deleteDbKey, readDbKey } from '@/db/key';
import { FutureSchemaError, migrate } from '@/db/migrations';
import { DbOpenError, keyConnection } from '@/db/open';
import { enforceRetention } from '@/db/retention';
import { Db, type RawConnection } from '@/db/sql';
import { log } from '@/lib/log';
import { registerWipeHook } from '@/security/endSession';
import { getPolicy, type ClientPolicy } from '@/security/policy';

/**
 * The encrypted offline store (ADR-0025 WP6, plan-app B4.6): one SQLCipher
 * file per device, belonging to the connected workspace origin.
 *
 * Lifecycle (driven by src/db/offlineBinding.ts):
 * - `open(origin)`: only while signed in, unlocked and allowed by policy. The
 *   key is read (or created) here and nowhere else; a missing key means any
 *   old file is unreadable, so the file is deleted first. A file that does not
 *   decrypt, a schema from a newer app, or a file of another origin is
 *   deleted and recreated with a new key: it is a cache, the server has the
 *   data. A binary without SQLCipher is refused for the rest of the process
 *   (nothing is ever written in plaintext).
 * - `close()`: on lock and sign-out; the connection and the key it holds go.
 * - `wipe()`: endSession (logout, device_revoked, server switch, …) and a
 *   policy that turns offline storage off. Close first, then the file with its
 *   -wal/-shm sidecars, then the key.
 *
 * All three are serialised, so a wipe never races an open.
 */

export const DB_NAME = 'az-offline.db';

export interface StoreDriver {
  open(name: string): Promise<RawConnection>;
  /** Deletes the file and its sidecars; a missing file is not an error. */
  remove(name: string): Promise<void>;
  /** False only in tests (node's SQLite has no SQLCipher). */
  requireCipher: boolean;
}

export interface StoreKeys {
  read(): Promise<string | null>;
  create(): Promise<string>;
  remove(): Promise<void>;
}

export interface OfflineStoreDeps {
  driver: StoreDriver;
  keys?: StoreKeys;
  getPolicy?: () => Readonly<ClientPolicy>;
  now?: () => number;
  /**
   * The device-lock gate: true only while the app is unlocked. Checked again
   * right before the key is read (the binding already checks it).
   */
  keyGate?: () => boolean;
}

export type StoreStatus = 'closed' | 'open' | 'unavailable';

const defaultKeys: StoreKeys = { read: readDbKey, create: () => createDbKey(), remove: deleteDbKey };

export class OfflineStore {
  private db: Db | null = null;
  private origin: string | null = null;
  private unavailable = false;
  private chain: Promise<unknown> = Promise.resolve();
  private readonly listeners = new Set<() => void>();
  private readonly keys: StoreKeys;
  private readonly policy: () => Readonly<ClientPolicy>;
  private readonly now: () => number;

  private keyGate: () => boolean;

  constructor(private readonly deps: OfflineStoreDeps) {
    this.keys = deps.keys ?? defaultKeys;
    this.policy = deps.getPolicy ?? getPolicy;
    this.now = deps.now ?? Date.now;
    this.keyGate = deps.keyGate ?? (() => false);
  }

  /** Installs the device-lock gate (the binding does, at startup). Until then no key is released. */
  setKeyGate(gate: () => boolean): void {
    this.keyGate = gate;
  }

  /** The open database, or null. */
  get(): Db | null {
    return this.db && !this.db.isClosed ? this.db : null;
  }

  get status(): StoreStatus {
    if (this.unavailable) return 'unavailable';
    return this.get() ? 'open' : 'closed';
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  private emit(): void {
    for (const listener of [...this.listeners]) listener();
  }

  private serial<T>(work: () => Promise<T>): Promise<T> {
    const next = this.chain.then(work);
    this.chain = next.catch(() => undefined);
    return next;
  }

  open(origin: string): Promise<Db | null> {
    return this.serial(async () => {
      if (this.db && !this.db.isClosed && this.origin === origin) return this.db;
      if (this.db) await this.closeNow();
      if (this.unavailable) return null;
      if (!this.keyGate()) return null;
      let db: Db | null = null;
      for (let attempt = 0; attempt < 2 && db === null; attempt++) {
        db = await this.tryOpen(origin, attempt > 0);
        if (this.unavailable) return null;
      }
      if (db === null) return null;
      this.db = db;
      this.origin = origin;
      try {
        await enforceRetention(db, this.policy(), this.now());
      } catch (error) {
        log.warn('offline retention failed', { error });
      }
      this.emit();
      return db;
    });
  }

  /** One open attempt; `fresh` starts over with a new key and no file. */
  private async tryOpen(origin: string, fresh: boolean): Promise<Db | null> {
    let key = fresh ? null : await this.keys.read();
    if (key === null) {
      // No usable key: any file left behind cannot be decrypted.
      await this.deps.driver.remove(DB_NAME);
      key = await this.keys.create();
    }
    let raw: RawConnection | null = null;
    try {
      raw = await this.deps.driver.open(DB_NAME);
      await keyConnection(raw, key, { requireCipher: this.deps.driver.requireCipher });
      const db = new Db(raw);
      await migrate(db);
      const stored = await db.get<{ value: string }>("SELECT value FROM meta WHERE key = 'origin'");
      if (stored !== null && stored.value !== origin) {
        await db.close();
        raw = null;
        await this.deps.driver.remove(DB_NAME);
        log.info('offline store belonged to another workspace; recreated');
        return null;
      }
      if (stored === null) await db.run("INSERT INTO meta (key, value) VALUES ('origin', ?)", [origin]);
      return db;
    } catch (error) {
      if (raw) {
        try {
          await raw.close();
        } catch {
          // already closed or never usable
        }
      }
      if (error instanceof DbOpenError && error.reason === 'no_cipher') {
        log.warn('this build has no SQLCipher; offline storage stays off');
        this.unavailable = true;
        await this.deps.driver.remove(DB_NAME);
        this.emit();
        return null;
      }
      const reason = error instanceof DbOpenError ? error.reason : error instanceof FutureSchemaError ? 'future_schema' : 'error';
      log.warn('offline store could not be opened; starting over', { reason });
      await this.deps.driver.remove(DB_NAME);
      return null;
    }
  }

  private async closeNow(): Promise<void> {
    const db = this.db;
    this.db = null;
    this.origin = null;
    if (db) {
      try {
        await db.close();
      } finally {
        this.emit();
      }
    }
  }

  /** Closes the connection (lock, sign-out). The file and key stay. */
  close(): Promise<void> {
    return this.serial(() => this.closeNow());
  }

  /** Closes, then deletes the file (with sidecars) and the key. Throws if a step failed. */
  wipe(): Promise<void> {
    return this.serial(async () => {
      let failure: unknown = null;
      try {
        await this.closeNow();
      } catch (error) {
        failure = error;
      }
      try {
        await this.deps.driver.remove(DB_NAME);
      } catch (error) {
        failure ??= error;
      }
      try {
        await this.keys.remove();
      } catch (error) {
        failure ??= error;
      }
      if (failure !== null) throw failure;
    });
  }

  /** Re-applies retention (policy change, resume). */
  retain(): Promise<void> {
    return this.serial(async () => {
      const db = this.get();
      if (!db) return;
      try {
        await enforceRetention(db, this.policy(), this.now());
      } catch (error) {
        log.warn('offline retention failed', { error });
      }
    });
  }
}

/**
 * expo-sqlite is loaded on first use, so modules that only import the store
 * (queries, sync) never load the native module in jest or on web.
 */
function expoDriver(): typeof import('@/db/expoDriver') {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  return require('@/db/expoDriver');
}

/** The app's store, over expo-sqlite with SQLCipher. */
export const offlineStore = new OfflineStore({
  driver: {
    requireCipher: true,
    open: (name) => expoDriver().openExpoConnection(name),
    remove: (name) => expoDriver().deleteExpoDatabase(name),
  },
});

// Every end of a session (logout, device_revoked, server switch, reset) deletes the store.
registerWipeHook('offline_store', () => offlineStore.wipe());
