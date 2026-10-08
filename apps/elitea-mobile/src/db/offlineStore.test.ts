import { SCHEMA_VERSION } from '@/db/migrations';
import { OfflineStore, type StoreKeys, type StoreUsage } from '@/db/offlineStore';
import { dbNameFor } from '@/security/workspaceRegistry';
import { STRICT_DEFAULTS } from '@/security/policy';
import { nodeSqliteDriver, tempDir } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';
const WS = 'aaaaaaaaaaaa';
const DB_NAME = dbNameFor(WS);

function memoryKeys(initial: string | null = null) {
  let stored = initial;
  let n = 0;
  const keys: StoreKeys & { stored(): string | null } = {
    read: jest.fn(async () => stored),
    create: jest.fn(async () => {
      n += 1;
      stored = n.toString(16).padStart(64, '0');
      return stored;
    }),
    remove: jest.fn(async () => {
      stored = null;
    }),
    stored: () => stored,
  };
  return keys;
}

function setup(
  options: {
    cipher?: boolean;
    keys?: ReturnType<typeof memoryKeys>;
    gate?: () => boolean;
    isActive?: () => boolean;
    now?: () => number;
    usage?: StoreUsage;
  } = {},
) {
  const driver = nodeSqliteDriver(tempDir(), { cipher: options.cipher ?? true });
  const keys = options.keys ?? memoryKeys();
  const store = new OfflineStore({
    driver,
    keys,
    keyGate: options.gate ?? (() => true),
    isActive: options.isActive ?? (() => true),
    getPolicy: () => ({ ...STRICT_DEFAULTS, offline_retention_days: 30, offline_max_mb: 0 }),
    now: options.now,
    usage: options.usage,
  });
  return { driver, keys, store };
}

function memoryUsage() {
  const at = new Map<string, number>();
  const usage: StoreUsage & { at: Map<string, number> } = {
    read: jest.fn(async (id: string) => at.get(id) ?? null),
    record: jest.fn(async (id: string, when: number) => {
      at.set(id, when);
    }),
    at,
  };
  return usage;
}

beforeEach(() => {
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('OfflineStore.open', () => {
  it('creates a key, keys the file, migrates and records the origin', async () => {
    const { store, keys, driver } = setup();
    const db = await store.open(ORIGIN, WS);
    expect(db).not.toBeNull();
    expect(keys.create).toHaveBeenCalledTimes(1);
    expect(driver.opened[0].statements[0]).toBe(`PRAGMA key = "x'${keys.stored()}'";`);
    expect(await db!.get('PRAGMA user_version;')).toEqual({ user_version: SCHEMA_VERSION });
    expect(await db!.get("SELECT value FROM meta WHERE key = 'origin'")).toEqual({ value: ORIGIN });
    expect(store.status).toBe('open');
    expect(await store.open(ORIGIN, WS)).toBe(db);
  });

  it('reopens an existing file with its stored key', async () => {
    const { store, keys, driver } = setup();
    const db = await store.open(ORIGIN, WS);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'kept')");
    await store.close();
    expect(store.get()).toBeNull();
    const again = await store.open(ORIGIN, WS);
    expect(keys.create).toHaveBeenCalledTimes(1);
    expect(await again!.get("SELECT value FROM meta WHERE key = 'probe'")).toEqual({ value: 'kept' });
    expect(driver.remove).toHaveBeenCalledTimes(1); // only the first open (no key yet)
  });

  it('deletes a file it has no key for before creating a new one', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN, WS);
    await store.close();
    await keys.remove(WS); // e.g. the Keychain item was lost
    const db = await store.open(ORIGIN, WS);
    expect(db).not.toBeNull();
    expect(driver.remove).toHaveBeenLastCalledWith(DB_NAME);
    expect(keys.create).toHaveBeenCalledTimes(2);
  });

  it('starts over with a new key and an empty file when the key is wrong', async () => {
    const keys = memoryKeys();
    const { store, driver } = setup({ keys });
    const db = await store.open(ORIGIN, WS);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'old')");
    await store.close();
    // The stored key no longer matches the file.
    await keys.create(WS);
    const fresh = await store.open(ORIGIN, WS);
    expect(fresh).not.toBeNull();
    expect(await fresh!.get("SELECT value FROM meta WHERE key = 'probe'")).toBeNull();
    expect(driver.fileKeys.get(DB_NAME)).toBe(keys.stored());
  });

  it('never runs without SQLCipher: the file is deleted and the store stays unavailable', async () => {
    const driver = nodeSqliteDriver(tempDir(), { cipher: false });
    const keys = memoryKeys();
    const store = new OfflineStore({ driver: { ...driver, requireCipher: true }, keys, keyGate: () => true, isActive: () => true });
    expect(await store.open(ORIGIN, WS)).toBeNull();
    expect(store.status).toBe('unavailable');
    expect(driver.exists(DB_NAME)).toBe(false);
    (keys.read as jest.Mock).mockClear();
    expect(await store.open(ORIGIN, WS)).toBeNull();
    expect(keys.read).not.toHaveBeenCalled();
  });

  it('recreates a file that belongs to another workspace', async () => {
    const { store } = setup();
    const db = await store.open(ORIGIN, WS);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'other workspace')");
    await store.close();
    const other = await store.open('https://other.example.com', WS);
    expect(await other!.get("SELECT value FROM meta WHERE key = 'probe'")).toBeNull();
    expect(await other!.get("SELECT value FROM meta WHERE key = 'origin'")).toEqual({ value: 'https://other.example.com' });
  });

  it('discards a file written by a newer app version', async () => {
    const { store } = setup();
    const db = await store.open(ORIGIN, WS);
    await db!.exec(`PRAGMA user_version = ${SCHEMA_VERSION + 1};`);
    await store.close();
    const again = await store.open(ORIGIN, WS);
    expect(await again!.get('PRAGMA user_version;')).toEqual({ user_version: SCHEMA_VERSION });
  });

  it('releases no key while the device-lock gate is closed', async () => {
    let unlocked = false;
    const { store, keys } = setup({ gate: () => unlocked });
    expect(await store.open(ORIGIN, WS)).toBeNull();
    expect(keys.read).not.toHaveBeenCalled();
    expect(keys.create).not.toHaveBeenCalled();
    unlocked = true;
    expect(await store.open(ORIGIN, WS)).not.toBeNull();
  });
});

describe('OfflineStore.wipe', () => {
  it('closes first, then deletes the file and the key', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN, WS);
    expect(driver.exists(DB_NAME)).toBe(true);
    await store.wipe(WS);
    expect(store.get()).toBeNull();
    expect(driver.exists(DB_NAME)).toBe(false);
    expect(keys.stored()).toBeNull();
  });

  it('is serialised behind an open in progress', async () => {
    const { store, driver } = setup();
    const opening = store.open(ORIGIN, WS);
    const wiping = store.wipe(WS);
    await Promise.all([opening, wiping]);
    expect(store.get()).toBeNull();
    expect(driver.exists(DB_NAME)).toBe(false);
  });

  it('still deletes the key when the file delete fails, and reports the failure', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN, WS);
    driver.remove.mockRejectedValueOnce(new Error('io'));
    await expect(store.wipe(WS)).rejects.toThrow('io');
    expect(keys.remove).toHaveBeenCalled();
  });
});

// #45: a workspace put aside cannot be opened without its key (behind the
// device lock), so enforceRetention cannot run on it. Retention drops what
// had no server activity for `offline_retention_days`, and a workspace's file
// only receives data while it is open: one not used for longer than that
// holds nothing retention would keep.
describe('OfflineStore.expireIfUnused', () => {
  const DAY = 86_400_000;
  function putAside() {
    let clock = 1_000 * DAY;
    let active = true;
    const usage = memoryUsage();
    const ctx = setup({ isActive: () => active, now: () => clock, usage });
    return {
      ...ctx,
      usage,
      setActive: (value: boolean) => {
        active = value;
      },
      advance: (ms: number) => {
        clock += ms;
      },
      now: () => clock,
    };
  }

  it('records when the workspace was last used: at open, on retain and at close', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    expect(t.usage.at.get(WS)).toBe(t.now());
    t.advance(DAY);
    await t.store.retain();
    expect(t.usage.at.get(WS)).toBe(t.now());
    t.advance(DAY);
    await t.store.close();
    expect(t.usage.at.get(WS)).toBe(t.now());
  });

  it('deletes the file and key of a workspace put aside and not used for longer than its retention', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    await t.store.close();
    t.setActive(false);
    t.advance(30 * DAY + 1);
    expect(await t.store.expireIfUnused(WS, 30)).toBe(true);
    expect(t.driver.exists(DB_NAME)).toBe(false);
    expect(t.keys.stored()).toBeNull();
  });

  it('keeps it while it was used within its retention', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    await t.store.close();
    t.setActive(false);
    t.advance(30 * DAY);
    expect(await t.store.expireIfUnused(WS, 30)).toBe(false);
    expect(t.driver.exists(DB_NAME)).toBe(true);
    expect(t.keys.stored()).not.toBeNull();
  });

  it('never deletes the file of the bound workspace, or the open one', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    t.advance(90 * DAY);
    expect(await t.store.expireIfUnused(WS, 30)).toBe(false);
    // Bound to another workspace while this file is still open (a switch under way).
    t.setActive(false);
    expect(await t.store.expireIfUnused(WS, 30)).toBe(false);
    expect(t.store.status).toBe('open');
    expect(t.driver.exists(DB_NAME)).toBe(true);
  });

  it('keeps a file whose last use is unknown (written by an older app version) until it is opened', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    await t.store.close();
    t.usage.at.clear();
    t.setActive(false);
    t.advance(90 * DAY);
    expect(await t.store.expireIfUnused(WS, 30)).toBe(false);
    expect(t.driver.exists(DB_NAME)).toBe(true);
  });

  it('does nothing for a retention of 0 (offline storage off is a wipe, not an expiry)', async () => {
    const t = putAside();
    await t.store.open(ORIGIN, WS);
    await t.store.close();
    t.setActive(false);
    t.advance(90 * DAY);
    expect(await t.store.expireIfUnused(WS, 0)).toBe(false);
  });
});
