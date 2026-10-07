import {
  NO_MANAGED_CONFIG,
  getManagedConfig,
  hasManagedConfig,
  parseManagedConfig,
  refreshManagedConfig,
  subscribeManagedConfig,
} from '@/security/managedConfig';
import { STRICT_DEFAULTS, applyManagedFloor, getPolicyState, resetPolicy, setPolicy } from '@/security/policy';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// No native SettingsManager under jest. The stub keeps React Native's own JS
// semantics (Libraries/Settings/Settings.ios.js): get() reads a cache filled at
// start, and a settingsUpdated body updates and notifies ONLY the keys it carries.
jest.mock('react-native/Libraries/Settings/Settings', () => {
  const emitter = jest.requireActual('react-native/Libraries/EventEmitter/RCTDeviceEventEmitter').default;
  const subs: { keys: string[]; callback: () => void }[] = [];
  const Settings = {
    _settings: {} as Record<string, unknown>,
    get: (key: string) => Settings._settings[key],
    set: jest.fn(),
    watchKeys: (keys: string | string[], callback: () => void) => subs.push({ keys: typeof keys === 'string' ? [keys] : keys, callback }),
    clearWatch: jest.fn(),
  };
  emitter.addListener('settingsUpdated', (body: Record<string, unknown>) => {
    for (const key of Object.keys(body)) {
      const changed = Settings._settings[key] !== body[key];
      Settings._settings[key] = body[key];
      if (changed) for (const sub of subs) if (sub.keys.includes(key)) sub.callback();
    }
  });
  return { __esModule: true, default: Settings };
});

// The dictionary an MDM (Jamf, Intune, Kandji) delivers as the app's managed AppConfig.
const appConfig = {
  server_url: 'https://ai.example.com',
  require_device_lock: true,
  idle_lock_seconds: 60,
  allow_screenshots: false,
};

afterEach(() => {
  refreshManagedConfig(() => null);
  resetPolicy();
});

describe('parseManagedConfig', () => {
  it('reads the documented keys', () => {
    expect(parseManagedConfig(appConfig)).toEqual({
      serverUrl: 'https://ai.example.com',
      requireDeviceLock: true,
      idleLockSeconds: 60,
      allowScreenshots: false,
    });
  });

  it('ignores a key of the wrong type, and anything that is not a dictionary', () => {
    expect(parseManagedConfig({ server_url: 42, require_device_lock: 'yes', idle_lock_seconds: -5, other: 1 })).toEqual(NO_MANAGED_CONFIG);
    expect(parseManagedConfig({ server_url: '  https://x.example.com ', idle_lock_seconds: 90_000 })).toMatchObject({
      serverUrl: 'https://x.example.com',
      idleLockSeconds: null,
    });
    for (const raw of [null, undefined, 'https://a', [appConfig], 7]) expect(parseManagedConfig(raw)).toEqual(NO_MANAGED_CONFIG);
    expect(hasManagedConfig(NO_MANAGED_CONFIG)).toBe(false);
  });

  it('reads booleans as React Native Settings delivers them on iOS: 1 and 0', () => {
    // Settings.get() hands a plist <true/> to JS as the number 1 (seen on the simulator with
    // `defaults write … -dict require_device_lock -bool true`), so a 1/0 must count as the boolean.
    expect(parseManagedConfig({ require_device_lock: 1, idle_lock_seconds: 60, allow_screenshots: 0 })).toEqual({
      serverUrl: null,
      requireDeviceLock: true,
      idleLockSeconds: 60,
      allowScreenshots: false,
    });
    expect(parseManagedConfig({ require_device_lock: 2, allow_screenshots: -1 })).toEqual(NO_MANAGED_CONFIG);
  });

  it('a 1/0 managed dictionary tightens the workspace policy', () => {
    refreshManagedConfig(() => ({ require_device_lock: 1, idle_lock_seconds: 60, allow_screenshots: 0 }));
    setPolicy({ require_device_lock: false, allow_screenshots: true, idle_lock_seconds: 0 }, 'server');
    expect(getPolicyState()).toMatchObject({
      policy: { require_device_lock: true, idle_lock_seconds: 60, allow_screenshots: false },
      managed: true,
    });
  });

  it('notifies on a change only', () => {
    const listener = jest.fn();
    const unsubscribe = subscribeManagedConfig(listener);
    refreshManagedConfig(() => appConfig);
    refreshManagedConfig(() => ({ ...appConfig }));
    expect(listener).toHaveBeenCalledTimes(1);
    expect(getManagedConfig().serverUrl).toBe('https://ai.example.com');
    unsubscribe();
  });
});

describe('the managed lock floor', () => {
  const loose = { ...STRICT_DEFAULTS, require_device_lock: false, idle_lock_seconds: 0, allow_screenshots: true };

  it('only tightens: a lock, a shorter non-zero idle time, screenshots off', () => {
    expect(applyManagedFloor(loose, parseManagedConfig(appConfig))).toEqual({
      policy: { ...loose, require_device_lock: true, idle_lock_seconds: 60, allow_screenshots: false },
      tightened: true,
    });
    const strict = { ...STRICT_DEFAULTS, idle_lock_seconds: 30 };
    expect(applyManagedFloor(strict, parseManagedConfig({ require_device_lock: false, idle_lock_seconds: 600, allow_screenshots: true }))).toEqual({
      policy: strict,
      tightened: false,
    });
    // 0 ("never relock on resume") is the loosest idle value, never a tightening.
    expect(applyManagedFloor(strict, parseManagedConfig({ idle_lock_seconds: 0 })).policy.idle_lock_seconds).toBe(30);
  });

  it('applies to the workspace policy, and again at once when MDM pushes a change', () => {
    setPolicy({ require_device_lock: false, allow_screenshots: true }, 'server');
    expect(getPolicyState()).toMatchObject({ policy: { require_device_lock: false, allow_screenshots: true } });
    expect(getPolicyState().managed).toBeUndefined();
    refreshManagedConfig(() => appConfig);
    expect(getPolicyState()).toMatchObject({
      source: 'server',
      managed: true,
      policy: { require_device_lock: true, idle_lock_seconds: 60, allow_screenshots: false },
    });
    refreshManagedConfig(() => null);
    expect(getPolicyState().policy.require_device_lock).toBe(false);
  });
});

describe('following the MDM while the app runs', () => {
  // F4: React Native's Settings.watchKeys only fires for keys PRESENT in the
  // settingsUpdated body, and Settings.get reads a JS cache that keeps a
  // removed key. Removing the AppConfig (or unenrolling) must still drop the floor.
  it('drops the floor when the MDM removes the managed dictionary (the key is missing from the update)', () => {
    const { DeviceEventEmitter, Settings } = require('react-native') as typeof import('react-native');
    const { startManagedConfig } = require('@/security/managedConfig') as typeof import('@/security/managedConfig');
    (Settings as unknown as { _settings: Record<string, unknown> })._settings = { 'com.apple.configuration.managed': appConfig };
    startManagedConfig();
    expect(getManagedConfig().requireDeviceLock).toBe(true);

    // NSUserDefaultsDidChange: RCTSettingsManager sends the WHOLE defaults dictionary.
    DeviceEventEmitter.emit('settingsUpdated', { 'com.apple.configuration.managed': { ...appConfig, idle_lock_seconds: 30 }, AppleLocale: 'en_GB' });
    expect(getManagedConfig().idleLockSeconds).toBe(30);

    DeviceEventEmitter.emit('settingsUpdated', { AppleLocale: 'en_GB' });
    expect(getManagedConfig()).toEqual(NO_MANAGED_CONFIG);
    // And a later re-read does not bring the stale cached copy back.
    refreshManagedConfig();
    expect(getManagedConfig()).toEqual(NO_MANAGED_CONFIG);
  });
});
