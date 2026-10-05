/**
 * The database surface the offline store is written against. The app runs it
 * over expo-sqlite (SQLCipher build, `expoDriver.ts`); jest runs the same code
 * over node's built-in SQLite (`src/test/nodeSqlite.ts`), so migrations, sync
 * and retention are tested against a real SQL engine.
 */

export type SqlValue = string | number | null;

/** One connection's statements. */
export interface SqlExecutor {
  /** One or more statements without parameters (DDL, PRAGMA). */
  exec(sql: string): Promise<void>;
  run(sql: string, params?: readonly SqlValue[]): Promise<{ changes: number }>;
  all<T>(sql: string, params?: readonly SqlValue[]): Promise<T[]>;
  /** The first row, or null. */
  get<T>(sql: string, params?: readonly SqlValue[]): Promise<T | null>;
}

/** A driver's open connection. */
export interface RawConnection extends SqlExecutor {
  close(): Promise<void>;
}

export class DbClosedError extends Error {
  constructor() {
    super('the offline database is closed');
    this.name = 'DbClosedError';
  }
}

/**
 * The store's handle on one open connection.
 *
 * Every call is serialised through one queue: expo-sqlite's async calls on a
 * single connection may otherwise interleave with a transaction another
 * caller has open (its `withTransactionAsync` warns about exactly that), and
 * `withExclusiveTransactionAsync` opens a second connection that would not
 * carry the SQLCipher key. A transaction therefore owns the queue from BEGIN
 * to COMMIT, and its callback must use the `tx` it is given, never `db.*`
 * (that would wait for itself).
 */
export class Db implements SqlExecutor {
  private queue: Promise<unknown> = Promise.resolve();
  private closed = false;

  constructor(private readonly raw: RawConnection) {}

  get isClosed(): boolean {
    return this.closed;
  }

  private enqueue<T>(work: () => Promise<T>): Promise<T> {
    const next = this.queue.then(() => {
      if (this.closed) throw new DbClosedError();
      return work();
    });
    // The queue continues after a failure; the caller still sees it.
    this.queue = next.catch(() => undefined);
    return next;
  }

  exec(sql: string): Promise<void> {
    return this.enqueue(() => this.raw.exec(sql));
  }

  run(sql: string, params: readonly SqlValue[] = []): Promise<{ changes: number }> {
    return this.enqueue(() => this.raw.run(sql, params));
  }

  all<T>(sql: string, params: readonly SqlValue[] = []): Promise<T[]> {
    return this.enqueue(() => this.raw.all<T>(sql, params));
  }

  get<T>(sql: string, params: readonly SqlValue[] = []): Promise<T | null> {
    return this.enqueue(() => this.raw.get<T>(sql, params));
  }

  /** Runs `work` in one transaction: all of it commits, or none of it (ROLLBACK on any throw). */
  transaction<T>(work: (tx: SqlExecutor) => Promise<T>): Promise<T> {
    return this.enqueue(async () => {
      await this.raw.exec('BEGIN IMMEDIATE');
      let result: T;
      try {
        result = await work(this.raw);
      } catch (error) {
        try {
          await this.raw.exec('ROLLBACK');
        } catch {
          // The connection already rolled back (e.g. SQLITE_FULL); the original error matters.
        }
        throw error;
      }
      await this.raw.exec('COMMIT');
      return result;
    });
  }

  /** Waits for queued work, then closes. Later calls reject with `DbClosedError`. */
  close(): Promise<void> {
    const done = this.queue.then(async () => {
      if (this.closed) return;
      this.closed = true;
      await this.raw.close();
    });
    this.queue = done.catch(() => undefined);
    return done;
  }
}
