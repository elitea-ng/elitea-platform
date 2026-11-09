// Android managed configuration (ADR-0052 W1b): the EMM's app restrictions through the
// inline module native-modules/ManagedRestrictions.kt, parsed by the same parser as iOS.

type Restrictions = Record<string, unknown>;

const native = {
  values: {} as Restrictions,
  listeners: [] as ((r: Restrictions) => void)[],
  read: jest.fn((): Restrictions => native.values),
  addListener: jest.fn((_event: string, listener: (r: Restrictions) => void) => {
    native.listeners.push(listener);
    return { remove: jest.fn() };
  }),
  /** What Android does on ACTION_APPLICATION_RESTRICTIONS_CHANGED. */
  push(values: Restrictions): void {
    native.values = values;
    for (const listener of native.listeners) listener(values);
  },
};
let available = true;
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

function loadOnAndroid(): typeof import('@/security/managedConfig') {
  let mod: typeof import('@/security/managedConfig') | undefined;
  // react-native resolves Platform lazily, so the module under test may read either registry's copy.
  jest.replaceProperty(require('react-native').Platform, 'OS', 'android');
  jest.isolateModules(() => {
    jest.replaceProperty(require('react-native').Platform, 'OS', 'android');
    mod = require('@/security/managedConfig');
    mod!.setManagedRestrictionsLoader((name) => (available && name === 'ManagedRestrictions' ? native : null));
  });
  return mod!;
}

beforeEach(() => {
  native.values = {};
  native.listeners = [];
  available = true;
  jest.clearAllMocks();
});
afterEach(() => jest.restoreAllMocks());

describe('Android managed configuration', () => {
  it('reads the restrictions an EMM set, with their Android types', () => {
    native.values = {
      server_url: 'https://ai.example.com',
      lock_server: true,
      require_device_lock: true,
      idle_lock_seconds: 120,
      allow_screenshots: false,
    };
    const m = loadOnAndroid();
    m.startManagedConfig();
    expect(m.getManagedConfig()).toEqual({
      serverUrl: 'https://ai.example.com',
      lockServer: true,
      requireDeviceLock: true,
      idleLockSeconds: 120,
      allowScreenshots: false,
      supportContact: null,
    });
  });

  it('ignores a restriction of the wrong type (an EMM sending a string for a bool)', () => {
    native.values = { server_url: 'https://ai.example.com', require_device_lock: 'true', idle_lock_seconds: 1.5 };
    const m = loadOnAndroid();
    m.startManagedConfig();
    expect(m.getManagedConfig()).toMatchObject({ serverUrl: 'https://ai.example.com', requireDeviceLock: null, idleLockSeconds: null });
  });

  it('follows a change, and drops everything when the EMM removes the configuration', () => {
    const m = loadOnAndroid();
    m.startManagedConfig();
    expect(m.hasManagedConfig(m.getManagedConfig())).toBe(false);
    native.push({ require_device_lock: true });
    expect(m.getManagedConfig().requireDeviceLock).toBe(true);
    native.push({});
    expect(m.getManagedConfig()).toEqual(m.NO_MANAGED_CONFIG);
    // Started twice (a remount): one listener.
    m.startManagedConfig();
    expect(native.addListener).toHaveBeenCalledTimes(1);
  });

  it('is "none" without the native module (Expo Go) or when reading throws', () => {
    available = false;
    const m = loadOnAndroid();
    m.startManagedConfig();
    expect(m.getManagedConfig()).toEqual(m.NO_MANAGED_CONFIG);
    expect(native.addListener).not.toHaveBeenCalled();
    available = true;

    native.read.mockImplementationOnce(() => {
      throw new Error('RestrictionsManager unavailable');
    });
    expect(loadOnAndroid().refreshManagedConfig()).toEqual(m.NO_MANAGED_CONFIG);
  });
});
