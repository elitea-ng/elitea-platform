import { createDbKey, deleteDbKey, readDbKey } from '@/db/key';
import { FutureSchemaError, migrate } from '@/db/migrations';
import { DbOpenError, keyConnection } from '@/db/open';
import { enforceRetention } from '@/db/retention';
import { Db, type RawConnection } from '@/db/sql';
import { log } from '@/lib/log';
import { isEndingSession, registerStoredWipeHook, registerWipeHook } from '@/security/endSession';
import { getPolicy, type ClientPolicy } from '@/security/policy';
import { boundWorkspace } from '@/security/secureStorage';
import {
  LEGACY_DB_NAME,
  readWorkspaceUsedAt,
  recordWorkspaceUse,
  registerLegacyDatabaseRemover,
  workspaceDbName,
} from '@/security/workspaceRegistry';

/**
 * The encrypted offline store (ADR-0025 WP6, plan-app B4.6): one SQLCipher
 * file and one key PER WORKSPACE (the slot's meta names the file:
 * `az-offline-<id>.db`, or `az-offline.db` for the slot migrated from the
 * single-workspace layout). Only the active workspace's file is ever open;
 * the others stay closed with their cache, outbox and sync cursors intact.
 *
 * Lifecycle (driven by src/db/offlineBinding.ts):
 * - `open(origin, workspaceId)`: only while signed in, unlocked, allowed by
 *   policy, and while that workspace is the bound one. The
 *   key is read (or created) here and nowhere else; a missing key means any
 *   old file is unreadable, so the file is deleted first. A file that does not
 *   decrypt, a schema from a newer app, or a file of another origin is
 *   deleted and recreated with a new key: it is a cache, the server has the
 *   data. A binary without SQLCipher is refused for the rest of the process
 *   (nothing is ever written in plaintext).
 * - `close()`: on lock, sign-out and a workspace switch; the connection and
 *   the key it holds go.
 * - `wipe(workspaceId)`: endSession (logout, device_revoked, …), removing a
 *   workspace that is not active, and a policy that turns offline storage
 *   off. Close first when it is the open one, then the file with its -wal/-shm
 *   sidecars, then the key.
 * - `expireIfUnused(workspaceId, days)`: retention for a workspace put aside
 *   (its key is behind the device lock, so `enforceRetention` cannot run on
 *   it). The last time a file was open is stamped in the slot's meta at open,
 *   on retain and at close; a file not open for longer than the retention
 *   window holds nothing retention would keep, and is deleted with its key.
 *
 * All three are serialised, so a wipe never races an open.
 */

/** The file of the single-workspace layout (kept by the migrated slot). */
export const DB_NAME = LEGACY_DB_NAME;

export interface StoreDriver {
  open(name: string): Promise<RawConnection>;
  /** Deletes the file and its sidecars; a missing file is not an error. */
  remove(name: string): Promise<void>;
  /** False only in tests (node's SQLite has no SQLCipher). */
  requireCipher: boolean;
}

export interface StoreKeys {
  read(workspaceId: string): Promise<string | null>;
  create(workspaceId: string): Promise<string>;
  remove(workspaceId: string): Promise<void>;
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
  /** The database file of a workspace (default: from its slot's meta). */
  dbName?: (workspaceId: string) => Promise<string>;
  /**
   * True while `workspaceId` is the bound workspace (default: secure
   * storage's binding). Checked right before the key is read, so an open
   * queued before a switch never opens a file for the wrong workspace.
   */
  isActive?: (workspaceId: string) => boolean;
  /** Last use of a workspace's file (default: its slot's meta). */
  usage?: StoreUsage;
}

export interface StoreUsage {
  read(workspaceId: string): Promise<number | null>;
  record(workspaceId: string, at: number): Promise<void>;
}

const DAY_MS = 86_400_000;

export type StoreStatus = 'closed' | 'open' | 'unavailable';

const defaultKeys: StoreKeys = { read: readDbKey, create: (id) => createDbKey(id), remove: deleteDbKey };
const defaultUsage: StoreUsage = { read: readWorkspaceUsedAt, record: recordWorkspaceUse };

export class OfflineStore {
  private db: Db | null = null;
  private origin: string | null = null;
  private workspaceId: string | null = null;
  private unavailable = false;
  private chain: Promise<unknown> = Promise.resolve();
  private readonly listeners = new Set<() => void>();
  private readonly keys: StoreKeys;
  private readonly policy: () => Readonly<ClientPolicy>;
  private readonly now: () => number;
  private readonly usage: StoreUsage;

  private keyGate: () => boolean;
  private readonly dbName: (workspaceId: string) => Promise<string>;
  private readonly isActive: (workspaceId: string) => boolean;

  constructor(private readonly deps: OfflineStoreDeps) {
    this.keys = deps.keys ?? defaultKeys;
    this.dbName = deps.dbName ?? workspaceDbName;
    this.isActive = deps.isActive ?? ((id) => boundWorkspace() === id);
    this.policy = deps.getPolicy ?? getPolicy;
    this.now = deps.now ?? Date.now;
    this.keyGate = deps.keyGate ?? (() => false);
    this.usage = deps.usage ?? defaultUsage;
  }

  /**
   * Stamps the last use of `workspaceId`'s file. Not while a session ends:
   * its slot is being wiped, and a write then could leave a meta item behind.
   * Never throws (a keychain error only makes a later expiry earlier).
   */
  private async recordUse(workspaceId: string): Promise<void> {
    if (isEndingSession()) return;
    try {
      await this.usage.record(workspaceId, this.now());
    } catch (error) {
      log.info('offline store use not recorded', { error });
    }
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

  /** The workspace whose file is open, or null. */
  get openWorkspace(): string | null {
    return this.get() ? this.workspaceId : null;
  }

  open(origin: string, workspaceId: string): Promise<Db | null> {
    return this.serial(async () => {
      if (this.db && !this.db.isClosed && this.origin === origin && this.workspaceId === workspaceId && this.isActive(workspaceId)) {
        return this.db;
      }
      if (this.db) await this.closeNow();
      if (this.unavailable) return null;
      if (!this.keyGate() || !this.isActive(workspaceId)) return null;
      const name = await this.dbName(workspaceId);
      let db: Db | null = null;
      for (let attempt = 0; attempt < 2 && db === null; attempt++) {
        db = await this.tryOpen(origin, workspaceId, name, attempt > 0);
        if (this.unavailable) return null;
      }
      if (db === null) return null;
      if (!this.isActive(workspaceId)) {
        // Switched away while opening: never leave another workspace's file open.
        await db.close();
        return null;
      }
      this.db = db;
      this.origin = origin;
      this.workspaceId = workspaceId;
      try {
        await enforceRetention(db, this.policy(), this.now());
      } catch (error) {
        log.warn('offline retention failed', { error });
      }
      await this.recordUse(workspaceId);
      this.emit();
      return db;
    });
  }

  /** One open attempt; `fresh` starts over with a new key and no file. */
  private async tryOpen(origin: string, workspaceId: string, name: string, fresh: boolean): Promise<Db | null> {
    let key = fresh ? null : await this.keys.read(workspaceId);
    if (key === null) {
      // No usable key: any file left behind cannot be decrypted.
      await this.deps.driver.remove(name);
      key = await this.keys.create(workspaceId);
    }
    let raw: RawConnection | null = null;
    try {
      raw = await this.deps.driver.open(name);
      await keyConnection(raw, key, { requireCipher: this.deps.driver.requireCipher });
      const db = new Db(raw);
      await migrate(db);
      const stored = await db.get<{ value: string }>("SELECT value FROM meta WHERE key = 'origin'");
      if (stored !== null && stored.value !== origin) {
        await db.close();
        raw = null;
        await this.deps.driver.remove(name);
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
        await this.deps.driver.remove(name);
        this.emit();
        return null;
      }
      const reason = error instanceof DbOpenError ? error.reason : error instanceof FutureSchemaError ? 'future_schema' : 'error';
      log.warn('offline store could not be opened; starting over', { reason });
      await this.deps.driver.remove(name);
      return null;
    }
  }

  private async closeNow(): Promise<void> {
    const db = this.db;
    this.db = null;
    this.origin = null;
    this.workspaceId = null;
    if (db) {
      try {
        await db.close();
      } finally {
        this.emit();
      }
    }
  }

  /** Closes the connection (lock, sign-out, a switch). The file and key stay. */
  close(): Promise<void> {
    return this.serial(async () => {
      const open = this.get() ? this.workspaceId : null;
      await this.closeNow();
      if (open !== null) await this.recordUse(open);
    });
  }

  /**
   * Deletes `workspaceId`'s file (with sidecars) and key, closing it first
   * when it is the open one. Other workspaces' files are not touched. Throws
   * if a step failed.
   */
  wipe(workspaceId: string): Promise<void> {
    return this.serial(async () => {
      let failure: unknown = null;
      if (this.workspaceId === workspaceId) {
        try {
          await this.closeNow();
        } catch (error) {
          failure = error;
        }
      }
      try {
        await this.deps.driver.remove(await this.dbName(workspaceId));
      } catch (error) {
        failure ??= error;
      }
      try {
        await this.keys.remove(workspaceId);
      } catch (error) {
        failure ??= error;
      }
      if (failure !== null) throw failure;
    });
  }

  /**
   * Deletes a database file no workspace owns (the old layout's file after a
   * discarded migration), with its sidecars. Never the open one.
   */
  removeOrphan(name: string): Promise<void> {
    return this.serial(async () => {
      if (this.db !== null && this.workspaceId !== null && name === (await this.dbName(this.workspaceId))) return;
      await this.deps.driver.remove(name);
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
      if (this.workspaceId !== null) await this.recordUse(this.workspaceId);
    });
  }

  /**
   * Retention for a workspace that is put aside (#45). `enforceRetention`
   * drops whatever had no server activity for `retentionDays`, and the file
   * only receives data while it is open, so a file last open more than
   * `retentionDays` ago holds nothing it would keep (sync cursors and unsent
   * outbox entries included: they are as old). Deletes the file (with its
   * sidecars) and the key; the next open starts an empty one.
   *
   * Never the open file or the bound workspace's, and nothing when the last
   * use is unknown (a slot written by an older app version: retention runs
   * when it is opened, as before). Serialised with open, so a switch to the
   * workspace in the meantime wins. True when the file was deleted; throws if
   * a delete failed.
   */
  expireIfUnused(workspaceId: string, retentionDays: number): Promise<boolean> {
    return this.serial(async () => {
      if (!(retentionDays > 0)) return false;
      if (this.get() !== null && this.workspaceId === workspaceId) return false;
      if (this.isActive(workspaceId)) return false;
      const usedAt = await this.usage.read(workspaceId);
      if (usedAt === null || this.now() - usedAt <= retentionDays * DAY_MS) return false;
      if (this.isActive(workspaceId)) return false;
      await this.deps.driver.remove(await this.dbName(workspaceId));
      await this.keys.remove(workspaceId);
      return true;
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

// Every end of a session closes the connection; the workspace's file and key
// are deleted by the stored-wipe hook, which also serves workspaces that are
// not active (removed from the switcher, a wipe resumed at launch).
registerWipeHook('offline_store', () => offlineStore.close());
registerStoredWipeHook('offline_store', (workspaceId) => offlineStore.wipe(workspaceId));
// A discarded single-workspace migration (an unfinished old-layout wipe): its file.
registerLegacyDatabaseRemover(() => offlineStore.removeOrphan(LEGACY_DB_NAME));
