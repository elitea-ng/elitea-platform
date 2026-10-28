import { useSyncExternalStore } from 'react';
import { AppState } from 'react-native';

import { offlineStore } from '@/db/offlineStore';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';
import { registerWipeHook } from '@/security/endSession';

/**
 * The text a person typed in a composer and did not send, per conversation
 * (`<project id>:<conversation ref>`, or `<project id>:new` for a conversation
 * not created yet). Leaving the thread, switching chats or projects, or
 * backgrounding the app keeps it; sending clears it.
 *
 * Memory first: every change lands in memory at once, so the next composer
 * opened for the key shows it. The copy that survives a relaunch lives in the
 * encrypted offline store (`drafts`, migration 5), written `WRITE_DELAY_MS`
 * after the last change and at once when the app goes to the background. That
 * store is open only while the workspace policy allows offline storage
 * (src/db/offlineBinding.ts); without it drafts are kept in memory only and a
 * relaunch starts empty, which is the policy's point: nothing typed is
 * written to the device. Opening the store loads its drafts (a change made in
 * memory meanwhile wins).
 *
 * At most `MAX_DRAFTS` are kept (the most recently changed); an empty text is
 * no draft. A draft is personal data: the session wipe (and a workspace
 * switch) forgets the memory copy, and the stored copy goes with the
 * workspace's database file. Retention drops stored drafts older than the
 * policy's window (src/db/retention.ts).
 */

/** How long after the last keystroke a draft is written to the device. */
export const WRITE_DELAY_MS = 300;
/** Drafts kept per workspace; the least recently changed go first. */
export const MAX_DRAFTS = 50;

/** The key of a conversation's draft; `null` is the conversation not created yet. */
export function draftKey(projectId: number, conversationRef: string | null): string {
  return `${projectId}:${conversationRef ?? 'new'}`;
}

interface Draft {
  text: string;
  at: number;
}

export interface DraftStoreDeps {
  /** The open database, or null (memory only). */
  db: () => Db | null;
  /** Told when the database opens or closes. */
  subscribe?: (listener: () => void) => () => void;
  now?: () => number;
  delayMs?: number;
  max?: number;
}

export class DraftStore {
  private readonly drafts = new Map<string, Draft>();
  /** Keys changed in memory and not yet written (a deleted draft is a dirty key with no entry). */
  private readonly dirty = new Set<string>();
  private readonly listeners = new Set<() => void>();
  private timer: ReturnType<typeof setTimeout> | null = null;
  private loadedFrom: Db | null = null;
  private writing: Promise<void> = Promise.resolve();
  private readonly now: () => number;
  private readonly delayMs: number;
  private readonly max: number;
  /** Bumped each time drafts are loaded from the device (a composer opened earlier picks them up). */
  loads = 0;

  constructor(private readonly deps: DraftStoreDeps) {
    this.now = deps.now ?? Date.now;
    this.delayMs = deps.delayMs ?? WRITE_DELAY_MS;
    this.max = deps.max ?? MAX_DRAFTS;
    deps.subscribe?.(() => this.onStoreChange());
    this.onStoreChange();
  }

  get(key: string): string | null {
    return this.drafts.get(key)?.text ?? null;
  }

  /** Keeps `text` as `key`'s draft (an empty one deletes it). */
  set(key: string, text: string): void {
    if (text.trim() === '') {
      if (!this.drafts.delete(key)) return;
    } else {
      if (this.drafts.get(key)?.text === text) return;
      this.drafts.set(key, { text, at: this.now() });
      this.evict();
    }
    this.dirty.add(key);
    this.schedule();
  }

  /** Deletes `key`'s draft when `sent` still holds it (a send was accepted); an edit made since stays. */
  clearIfSent(key: string, holdsOnlySent: (stored: string) => boolean): void {
    const stored = this.get(key);
    if (stored !== null && holdsOnlySent(stored)) this.set(key, '');
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  /** Writes the pending changes now (backgrounding). */
  flush(): Promise<void> {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    this.writing = this.writing.then(() => this.write());
    return this.writing;
  }

  /** Forgets every draft in memory (the stored copy goes with the database file). */
  clear(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.drafts.clear();
    this.dirty.clear();
    this.loadedFrom = null;
    this.emit();
  }

  private emit(): void {
    for (const listener of [...this.listeners]) listener();
  }

  private evict(): void {
    if (this.drafts.size <= this.max) return;
    const oldest = [...this.drafts.entries()].sort((a, b) => a[1].at - b[1].at).slice(0, this.drafts.size - this.max);
    for (const [key] of oldest) {
      this.drafts.delete(key);
      this.dirty.add(key);
    }
  }

  private schedule(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = setTimeout(() => void this.flush(), this.delayMs);
  }

  private async write(): Promise<void> {
    const db = this.deps.db();
    if (db === null || this.dirty.size === 0) return;
    const keys = [...this.dirty];
    this.dirty.clear();
    try {
      await db.transaction(async (tx) => {
        for (const key of keys) {
          const draft = this.drafts.get(key);
          if (draft === undefined) await tx.run('DELETE FROM drafts WHERE key = ?', [key]);
          else await tx.run('INSERT OR REPLACE INTO drafts (key, text, updated_ms) VALUES (?, ?, ?)', [key, draft.text, draft.at]);
        }
        await tx.run('DELETE FROM drafts WHERE key NOT IN (SELECT key FROM drafts ORDER BY updated_ms DESC LIMIT ?)', [this.max]);
      });
    } catch (error) {
      // Kept dirty for the next write (the store may have closed under it).
      for (const key of keys) this.dirty.add(key);
      log.info('drafts not written', { error });
    }
  }

  private onStoreChange(): void {
    const db = this.deps.db();
    if (db === null) {
      this.loadedFrom = null;
      return;
    }
    if (db === this.loadedFrom) return;
    this.loadedFrom = db;
    this.writing = this.writing.then(() => this.load(db));
  }

  private async load(db: Db): Promise<void> {
    try {
      const rows = await db.all<{ key: string; text: string; updated_ms: number }>(
        'SELECT key, text, updated_ms FROM drafts ORDER BY updated_ms DESC LIMIT ?',
        [this.max],
      );
      if (this.loadedFrom !== db) return;
      for (const row of rows) {
        if (!this.dirty.has(row.key)) this.drafts.set(row.key, { text: row.text, at: row.updated_ms });
      }
      this.evict();
      this.loads += 1;
      this.emit();
    } catch (error) {
      log.info('drafts not loaded', { error });
    }
    await this.write();
  }
}

/** The app's drafts, stored in the active workspace's offline store. */
export const composerDrafts = new DraftStore({ db: () => offlineStore.get(), subscribe: offlineStore.subscribe });

/** The count of loads from the device; a change tells a composer to look for its draft again. */
export function useDraftLoads(store: DraftStore = composerDrafts): number {
  return useSyncExternalStore(store.subscribe, () => store.loads, () => store.loads);
}

// Memory only: runs at the end of a session and on a workspace switch (the
// next workspace's projects may share ids with this one's).
registerWipeHook('chat_drafts', () => composerDrafts.clear());
AppState.addEventListener('change', (state) => {
  if (state !== 'active') void composerDrafts.flush();
});
