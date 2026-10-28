import { afterEach, describe, expect, it, vi } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import {
  compactionSettingsError,
  createCompactionIpc,
  customIdleCache,
  DEFAULT_COMPACTION_SETTINGS,
  installCompactionIpc,
  migrateIdleSwitch,
  readCompactionSettings,
  writeCompactionSettings,
  type CompactionSettings,
} from './compactionSettings';
import { createFakeCompactionIpc } from './compactionSettings.fake';

const OLD_KEY = 'desktop.idleCompaction.off';
const settings = (over: Partial<CompactionSettings> = {}): CompactionSettings => ({ ...DEFAULT_COMPACTION_SETTINGS, ...over });

describe('compactionSettingsError', () => {
  it('accepts the defaults, a threshold from 40 to 99 and custom minutes from 1 to 600', () => {
    expect(compactionSettingsError(DEFAULT_COMPACTION_SETTINGS)).toBeNull();
    expect(compactionSettingsError(settings({ threshold_percent: 40 }))).toBeNull();
    expect(compactionSettingsError(settings({ threshold_percent: 99 }))).toBeNull();
    expect(compactionSettingsError(settings({ idle: 'custom', idle_minutes: 1 }))).toBeNull();
    expect(compactionSettingsError(settings({ idle: 'custom', idle_minutes: 600 }))).toBeNull();
  });

  it('refuses what the host refuses', () => {
    expect(compactionSettingsError(settings({ threshold_percent: 39 }))).toMatch(/40 to 99/);
    expect(compactionSettingsError(settings({ threshold_percent: 100 }))).toMatch(/40 to 99/);
    expect(compactionSettingsError(settings({ threshold_percent: 50.5 }))).toMatch(/whole/);
    expect(compactionSettingsError(settings({ idle: 'custom', idle_minutes: null }))).toMatch(/1 to 600/);
    expect(compactionSettingsError(settings({ idle: 'custom', idle_minutes: 0 }))).toMatch(/1 to 600/);
    expect(compactionSettingsError(settings({ idle: 'custom', idle_minutes: 601 }))).toMatch(/1 to 600/);
    expect(compactionSettingsError(settings({ idle: 'cache_1h', idle_minutes: 30 }))).toMatch(/custom/);
  });
});

describe('customIdleCache', () => {
  it('uses the 5-minute cache up to 4 minutes, the 1-hour one to 55, and none from 56', () => {
    expect(customIdleCache(1)).toBe('cache_5m');
    expect(customIdleCache(4)).toBe('cache_5m');
    expect(customIdleCache(5)).toBe('cache_1h');
    expect(customIdleCache(55)).toBe('cache_1h');
    expect(customIdleCache(56)).toBe('past');
  });
});

describe('createCompactionIpc', () => {
  it('names the host commands and events', async () => {
    const invoke = vi.fn(() => Promise.resolve(DEFAULT_COMPACTION_SETTINGS));
    const listen = vi.fn(() => Promise.resolve(() => undefined));
    const ipc = createCompactionIpc(invoke as never, listen);
    await ipc.get();
    await ipc.set(settings({ idle: 'off' }));
    await ipc.hold('w1', 'c1', true);
    await ipc.onSettingsChanged(vi.fn());
    await ipc.onCompacted(vi.fn());
    expect(invoke.mock.calls).toEqual([
      ['compaction_settings_get'],
      ['compaction_settings_set', { settings: settings({ idle: 'off' }) }],
      ['compaction_idle_hold', { workspace_id: 'w1', conversation_id: 'c1', hold: true }],
    ]);
    expect(listen.mock.calls.map((call: unknown[]) => call[0])).toEqual(['compaction-settings-changed', 'workspace-context-compacted']);
  });
});

describe('writeCompactionSettings', () => {
  it('drops the minutes unless custom, and refuses invalid settings before the host', async () => {
    const ipc = createFakeCompactionIpc();
    await writeCompactionSettings(ipc, settings({ idle: 'cache_1h', idle_minutes: 30 }));
    expect(ipc.current()).toEqual(settings({ idle: 'cache_1h', idle_minutes: null }));
    await expect(writeCompactionSettings(ipc, settings({ threshold_percent: 10 }))).rejects.toThrow(/40 to 99/);
    expect(ipc.calls.sets).toHaveLength(1);
  });
});

describe('migrateIdleSwitch', () => {
  afterEach(() => {
    installCompactionIpc(undefined);
    createStorage('local').remove(OLD_KEY);
  });

  it('turns idle compaction off on the host once when the old switch was off, and drops the key', async () => {
    installCompactionIpc(undefined);
    createStorage('local').set(OLD_KEY, '1');
    const ipc = createFakeCompactionIpc(settings({ auto: false, threshold_percent: 80 }));
    expect(await readCompactionSettings(ipc)).toEqual(settings({ auto: true, threshold_percent: 80, idle: 'off' }));
    expect(createStorage('local').get(OLD_KEY)).toBeNull();
    await migrateIdleSwitch(ipc);
    expect(ipc.calls.sets).toHaveLength(1);
  });

  it('writes nothing without the old key, and drops a key that was not off', async () => {
    installCompactionIpc(undefined);
    const ipc = createFakeCompactionIpc();
    await migrateIdleSwitch(ipc);
    expect(ipc.calls.sets).toHaveLength(0);

    installCompactionIpc(undefined);
    createStorage('local').set(OLD_KEY, '0');
    await migrateIdleSwitch(ipc);
    expect(ipc.calls.sets).toHaveLength(0);
    expect(createStorage('local').get(OLD_KEY)).toBeNull();
  });

  it('keeps the key for the next try when the host refuses', async () => {
    installCompactionIpc(undefined);
    createStorage('local').set(OLD_KEY, '1');
    const ipc = createFakeCompactionIpc();
    vi.spyOn(ipc, 'set').mockRejectedValueOnce(new Error('host down'));
    await migrateIdleSwitch(ipc);
    expect(createStorage('local').get(OLD_KEY)).toBe('1');
    await migrateIdleSwitch(ipc);
    expect(createStorage('local').get(OLD_KEY)).toBeNull();
    expect(ipc.current().idle).toBe('off');
  });
});
