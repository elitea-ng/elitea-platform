import { SCHEMA_VERSION } from '@/db/migrations';
import { DB_NAME, OfflineStore, type StoreKeys } from '@/db/offlineStore';
import { STRICT_DEFAULTS } from '@/security/policy';
import { nodeSqliteDriver, tempDir } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';

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

function setup(options: { cipher?: boolean; keys?: ReturnType<typeof memoryKeys>; gate?: () => boolean } = {}) {
  const driver = nodeSqliteDriver(tempDir(), { cipher: options.cipher ?? true });
  const keys = options.keys ?? memoryKeys();
  const store = new OfflineStore({
    driver,
    keys,
    keyGate: options.gate ?? (() => true),
    getPolicy: () => ({ ...STRICT_DEFAULTS, offline_retention_days: 30, offline_max_mb: 0 }),
  });
  return { driver, keys, store };
}

beforeEach(() => {
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('OfflineStore.open', () => {
  it('creates a key, keys the file, migrates and records the origin', async () => {
    const { store, keys, driver } = setup();
    const db = await store.open(ORIGIN);
    expect(db).not.toBeNull();
    expect(keys.create).toHaveBeenCalledTimes(1);
    expect(driver.opened[0].statements[0]).toBe(`PRAGMA key = "x'${keys.stored()}'";`);
    expect(await db!.get('PRAGMA user_version;')).toEqual({ user_version: SCHEMA_VERSION });
    expect(await db!.get("SELECT value FROM meta WHERE key = 'origin'")).toEqual({ value: ORIGIN });
    expect(store.status).toBe('open');
    expect(await store.open(ORIGIN)).toBe(db);
  });

  it('reopens an existing file with its stored key', async () => {
    const { store, keys, driver } = setup();
    const db = await store.open(ORIGIN);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'kept')");
    await store.close();
    expect(store.get()).toBeNull();
    const again = await store.open(ORIGIN);
    expect(keys.create).toHaveBeenCalledTimes(1);
    expect(await again!.get("SELECT value FROM meta WHERE key = 'probe'")).toEqual({ value: 'kept' });
    expect(driver.remove).toHaveBeenCalledTimes(1); // only the first open (no key yet)
  });

  it('deletes a file it has no key for before creating a new one', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN);
    await store.close();
    await keys.remove(); // e.g. the Keychain item was lost
    const db = await store.open(ORIGIN);
    expect(db).not.toBeNull();
    expect(driver.remove).toHaveBeenLastCalledWith(DB_NAME);
    expect(keys.create).toHaveBeenCalledTimes(2);
  });

  it('starts over with a new key and an empty file when the key is wrong', async () => {
    const keys = memoryKeys();
    const { store, driver } = setup({ keys });
    const db = await store.open(ORIGIN);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'old')");
    await store.close();
    // The stored key no longer matches the file.
    await keys.create();
    const fresh = await store.open(ORIGIN);
    expect(fresh).not.toBeNull();
    expect(await fresh!.get("SELECT value FROM meta WHERE key = 'probe'")).toBeNull();
    expect(driver.fileKeys.get(DB_NAME)).toBe(keys.stored());
  });

  it('never runs without SQLCipher: the file is deleted and the store stays unavailable', async () => {
    const driver = nodeSqliteDriver(tempDir(), { cipher: false });
    const keys = memoryKeys();
    const store = new OfflineStore({ driver: { ...driver, requireCipher: true }, keys, keyGate: () => true });
    expect(await store.open(ORIGIN)).toBeNull();
    expect(store.status).toBe('unavailable');
    expect(driver.exists(DB_NAME)).toBe(false);
    (keys.read as jest.Mock).mockClear();
    expect(await store.open(ORIGIN)).toBeNull();
    expect(keys.read).not.toHaveBeenCalled();
  });

  it('recreates a file that belongs to another workspace', async () => {
    const { store } = setup();
    const db = await store.open(ORIGIN);
    await db!.run("INSERT INTO meta (key, value) VALUES ('probe', 'other workspace')");
    await store.close();
    const other = await store.open('https://other.example.com');
    expect(await other!.get("SELECT value FROM meta WHERE key = 'probe'")).toBeNull();
    expect(await other!.get("SELECT value FROM meta WHERE key = 'origin'")).toEqual({ value: 'https://other.example.com' });
  });

  it('discards a file written by a newer app version', async () => {
    const { store } = setup();
    const db = await store.open(ORIGIN);
    await db!.exec(`PRAGMA user_version = ${SCHEMA_VERSION + 1};`);
    await store.close();
    const again = await store.open(ORIGIN);
    expect(await again!.get('PRAGMA user_version;')).toEqual({ user_version: SCHEMA_VERSION });
  });

  it('releases no key while the device-lock gate is closed', async () => {
    let unlocked = false;
    const { store, keys } = setup({ gate: () => unlocked });
    expect(await store.open(ORIGIN)).toBeNull();
    expect(keys.read).not.toHaveBeenCalled();
    expect(keys.create).not.toHaveBeenCalled();
    unlocked = true;
    expect(await store.open(ORIGIN)).not.toBeNull();
  });
});

describe('OfflineStore.wipe', () => {
  it('closes first, then deletes the file and the key', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN);
    expect(driver.exists(DB_NAME)).toBe(true);
    await store.wipe();
    expect(store.get()).toBeNull();
    expect(driver.exists(DB_NAME)).toBe(false);
    expect(keys.stored()).toBeNull();
  });

  it('is serialised behind an open in progress', async () => {
    const { store, driver } = setup();
    const opening = store.open(ORIGIN);
    const wiping = store.wipe();
    await Promise.all([opening, wiping]);
    expect(store.get()).toBeNull();
    expect(driver.exists(DB_NAME)).toBe(false);
  });

  it('still deletes the key when the file delete fails, and reports the failure', async () => {
    const { store, driver, keys } = setup();
    await store.open(ORIGIN);
    driver.remove.mockRejectedValueOnce(new Error('io'));
    await expect(store.wipe()).rejects.toThrow('io');
    expect(keys.remove).toHaveBeenCalled();
  });
});
