/**
 * The app's auto-compaction settings (IPC.md "Context window"): when the
 * host summarizes a local thread on its own, for every folder. Near the
 * window's limit (or at a share of it), and in the background shortly
 * before the provider's prompt cache of an idle thread expires, so the
 * summary is read from the cache. `compaction_settings_get` / `_set` and the
 * `compaction-settings-changed` event; `compaction_idle_hold` keeps a thread
 * whose composer has unsent text from being compacted under the person; the
 * `workspace-context-compacted` event says a thread was summarized.
 *
 * The idle switch the app kept in local storage before (`desktop.idleCompaction.off`)
 * moves to the host once: `migrateIdleSwitch`.
 */
import { createStorage } from '@/shared/lib/storage';

import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** When an idle thread is compacted: before the 5-minute or the 1-hour cache expires, after a number of minutes, or never. */
export type IdleCompaction = 'cache_5m' | 'cache_1h' | 'custom' | 'off';

export interface CompactionSettings {
  auto: boolean;
  /** The share of the window that compacts, 40 to 99; `null`: near the limit (the host's buffer). */
  threshold_percent: number | null;
  idle: IdleCompaction;
  /** Only for `custom`: 1 to 600; else `null`. */
  idle_minutes: number | null;
}

export const DEFAULT_COMPACTION_SETTINGS: CompactionSettings = { auto: true, threshold_percent: null, idle: 'cache_5m', idle_minutes: null };

export const THRESHOLD_PERCENTS = [40, 50, 60, 70, 80, 90, 95, 99] as const;
const MIN_THRESHOLD = 40;
const MAX_THRESHOLD = 99;
export const MIN_IDLE_MINUTES = 1;
export const MAX_IDLE_MINUTES = 600;
/** Custom minutes from here on use the 1-hour cache (its writes cost 2×). */
const HOUR_CACHE_FROM_MINUTES = 5;
/** Custom minutes from here on are later than even the 1-hour cache lasts: the host skips idle compaction. */
const PAST_CACHE_FROM_MINUTES = 56;

/** What a custom number of minutes costs: the 5-minute cache, the 1-hour one, or later than any cache lasts. */
export function customIdleCache(minutes: number): 'cache_5m' | 'cache_1h' | 'past' {
  if (minutes >= PAST_CACHE_FROM_MINUTES) return 'past';
  return minutes >= HOUR_CACHE_FROM_MINUTES ? 'cache_1h' : 'cache_5m';
}

const inRange = (value: number, min: number, max: number): boolean => Number.isInteger(value) && value >= min && value <= max;

/** Why the host would refuse `settings` (its own checks), else `null`. */
export function compactionSettingsError(settings: CompactionSettings): string | null {
  if (settings.threshold_percent !== null && !inRange(settings.threshold_percent, MIN_THRESHOLD, MAX_THRESHOLD)) {
    return `The threshold must be a whole percent from ${String(MIN_THRESHOLD)} to ${String(MAX_THRESHOLD)}.`;
  }
  if (settings.idle === 'custom') {
    if (settings.idle_minutes === null || !inRange(settings.idle_minutes, MIN_IDLE_MINUTES, MAX_IDLE_MINUTES)) {
      return `The idle time must be a whole number of minutes from ${String(MIN_IDLE_MINUTES)} to ${String(MAX_IDLE_MINUTES)}.`;
    }
  } else if (settings.idle_minutes !== null) {
    return 'Minutes are set only for a custom idle time.';
  }
  return null;
}

/** `workspace-context-compacted`: the host summarized a thread. */
export interface ContextCompactedEvent {
  workspace_id: string;
  conversation_id: string;
  /** `idle`: the background compaction before the cache expired. */
  reason: string;
  before_tokens: number;
  after_tokens: number;
}

export interface CompactionIpc {
  get(): Promise<CompactionSettings>;
  /** Resolves the saved settings; rejects with the host's message when they are invalid. */
  set(settings: CompactionSettings): Promise<CompactionSettings>;
  /** `hold`: the thread's composer has unsent text, so it is not compacted while idle. */
  hold(workspaceId: string, conversationId: string, hold: boolean): Promise<void>;
  onSettingsChanged(handler: (settings: CompactionSettings) => void): Promise<() => void>;
  onCompacted(handler: (event: ContextCompactedEvent) => void): Promise<() => void>;
}

const SETTINGS_CHANGED_EVENT = 'compaction-settings-changed';
const CONTEXT_COMPACTED_EVENT = 'workspace-context-compacted';

export function createCompactionIpc(hostInvoke: HostInvoke, listen: ListenFn): CompactionIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    (args === undefined ? hostInvoke<T>(command) : hostInvoke<T>(command, args)).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    get: () => invoke<CompactionSettings>('compaction_settings_get'),
    set: (settings) => invoke<CompactionSettings>('compaction_settings_set', { settings }),
    hold: (workspaceId, conversationId, hold) => invoke<void>('compaction_idle_hold', { workspace_id: workspaceId, conversation_id: conversationId, hold }),
    onSettingsChanged: (handler) => listen(SETTINGS_CHANGED_EVENT, (payload) => handler(payload as CompactionSettings)),
    onCompacted: (handler) => listen(CONTEXT_COMPACTED_EVENT, (payload) => handler(payload as ContextCompactedEvent)),
  };
}

let installed: CompactionIpc | undefined;

/** Use `ipc` instead of the host's (the dev harness and tests); `undefined` goes back to the host. */
export function installCompactionIpc(ipc: CompactionIpc | undefined): void {
  installed = ipc;
  migrated = null;
}

/** The installed client, else the host's, else `undefined` outside the Tauri webview. */
export function compactionIpc(): CompactionIpc | undefined {
  if (installed !== undefined) return installed;
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = { invoke: internals.invoke.bind(internals), transformCallback: internals.transformCallback.bind(internals) };
  return createCompactionIpc(bound.invoke, tauriListen(bound));
}

const OLD_IDLE_OFF_KEY = 'desktop.idleCompaction.off';

function oldIdleSwitch(): string | null {
  try {
    return createStorage('local').get(OLD_IDLE_OFF_KEY);
  } catch {
    return null;
  }
}

function dropOldIdleSwitch(): void {
  try {
    createStorage('local').remove(OLD_IDLE_OFF_KEY);
  } catch {
    // Storage may be unavailable: nothing to drop.
  }
}

let migrated: Promise<void> | null = null;

/**
 * Once per app run: the idle switch turned off in an older version (`'1'` under the old key) turns idle compaction
 * off on the host, and the key goes. A failed write keeps the key for the next run.
 */
export function migrateIdleSwitch(ipc: CompactionIpc): Promise<void> {
  migrated ??= (async () => {
    const old = oldIdleSwitch();
    if (old === null) return;
    if (old === '1') {
      const current = await ipc.get();
      await ipc.set({ ...current, auto: true, idle: 'off', idle_minutes: null });
    }
    dropOldIdleSwitch();
  })().catch(() => {
    migrated = null;
  });
  return migrated;
}

/** The host's settings, after the old switch moved there. */
export async function readCompactionSettings(ipc: CompactionIpc): Promise<CompactionSettings> {
  await migrateIdleSwitch(ipc);
  return ipc.get();
}

/** Store `settings` (minutes dropped unless custom); rejects with the reason before asking the host when invalid. */
export async function writeCompactionSettings(ipc: CompactionIpc, settings: CompactionSettings): Promise<CompactionSettings> {
  const sent = settings.idle === 'custom' ? settings : { ...settings, idle_minutes: null };
  const error = compactionSettingsError(sent);
  if (error !== null) throw new Error(error);
  return ipc.set(sent);
}
