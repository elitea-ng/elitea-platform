import { REGISTERED_KEYS } from '@/security/secureStorage';
import { checkFreshInstall, type InstallMarker } from '@/security/freshInstall';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => ({ File: jest.fn(), Paths: { document: 'file:///doc/' } }));

const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

function memoryMarker(initial: string | null = null): InstallMarker & { contents: string | null } {
  const m = {
    contents: initial,
    exists: () => m.contents !== null,
    write: (c: string) => {
      m.contents = c;
    },
  };
  return m;
}

beforeEach(() => {
  fake.values.clear();
  jest.clearAllMocks();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('checkFreshInstall', () => {
  it('purges every registered key and writes the marker when the marker is missing', async () => {
    for (const key of REGISTERED_KEYS) fake.values.set(key, 'left over from a previous install');
    const marker = memoryMarker();

    const result = await checkFreshInstall({ marker, platform: 'ios', now: () => 42 });

    expect(result).toEqual({ fresh: true, purgeIncomplete: false });
    expect(fake.values.size).toBe(0);
    expect(JSON.parse(marker.contents!)).toEqual({ installedAt: 42 });
  });

  it('purges nothing when the marker is present', async () => {
    for (const key of REGISTERED_KEYS) fake.values.set(key, 'kept');
    const result = await checkFreshInstall({ marker: memoryMarker('{}'), platform: 'ios' });
    expect(result).toEqual({ fresh: false, purgeIncomplete: false });
    expect(fake.values.size).toBe(REGISTERED_KEYS.length);
    expect(fake.deleteItemAsync).not.toHaveBeenCalled();
  });

  it('does not write the marker after an incomplete purge, so the next launch retries', async () => {
    fake.deleteItemAsync.mockImplementationOnce(async () => {
      throw new Error('keychain busy');
    });
    const marker = memoryMarker();
    const result = await checkFreshInstall({ marker, platform: 'ios' });
    expect(result).toEqual({ fresh: true, purgeIncomplete: true });
    expect(marker.contents).toBeNull();
  });

  it('does not purge when the marker cannot be read', async () => {
    fake.values.set(REGISTERED_KEYS[0], 'kept');
    const marker: InstallMarker = {
      exists: () => {
        throw new Error('io');
      },
      write: jest.fn(),
    };
    const result = await checkFreshInstall({ marker, platform: 'android' });
    expect(result.fresh).toBe(false);
    expect(fake.values.size).toBe(1);
  });

  it('is a no-op on web', async () => {
    const marker = memoryMarker();
    const result = await checkFreshInstall({ marker, platform: 'web' });
    expect(result.fresh).toBe(false);
    expect(marker.contents).toBeNull();
  });
});
