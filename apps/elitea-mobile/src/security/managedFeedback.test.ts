// MDM feedback (ADR-0052 W6.1): what the app reports to the device management, and that it
// is never user content.
import { Platform, Settings } from 'react-native';

import { managedDelivery, NO_MANAGED_DELIVERY, parseManagedConfig } from '@/security/managedConfig';
import {
  FEEDBACK_CODES,
  FEEDBACK_KEY,
  FEEDBACK_KEYS,
  __resetManagedFeedbackForTests,
  feedbackDictionary,
  feedbackStates,
  reportFeedback,
  setKeyedAppStatesLoader,
  type FeedbackState,
} from '@/security/managedFeedback';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// The default export is what react-native's index hands out as `Settings`.
jest.mock('react-native/Libraries/Settings/Settings', () => {
  const Settings = { get: jest.fn(), set: jest.fn(), watchKeys: jest.fn(), clearWatch: jest.fn() };
  return { __esModule: true, default: Settings, ...Settings };
});

const ORIGIN = 'https://acme.example.com';
const signedIn = { serverOrigin: ORIGIN, credential: { kind: 'oauth' as const }, endReason: null, upgradeRequired: null };
const signedOut = { serverOrigin: null, credential: null, endReason: null, upgradeRequired: null };

function managed(raw: Record<string, unknown>) {
  const config = parseManagedConfig(raw);
  return { config, delivery: managedDelivery(raw, config) };
}

const byKey = (states: FeedbackState[]) => Object.fromEntries(states.map((s) => [s.key, s]));

beforeEach(() => {
  __resetManagedFeedbackForTests();
  jest.clearAllMocks();
});

afterEach(() => {
  jest.restoreAllMocks();
});

test('dormant: no managed configuration, no state', () => {
  expect(feedbackStates(signedIn, parseManagedConfig(null), NO_MANAGED_DELIVERY)).toEqual([]);
  // An empty dictionary is no configuration either.
  const { config, delivery } = managed({});
  expect(feedbackStates(signedIn, config, delivery, false)).toEqual([]);
});

test('signed in to the managed workspace: every state is info', () => {
  const { config, delivery } = managed({ server_url: ORIGIN, require_device_lock: true });
  const states = byKey(feedbackStates(signedIn, config, delivery, false));
  expect(states).toEqual({
    managed_config: { key: 'managed_config', severity: 'info', message: 'applied' },
    connection: { key: 'connection', severity: 'info', message: 'managed_server' },
    sign_in: { key: 'sign_in', severity: 'info', message: 'signed_in' },
    policy: { key: 'policy', severity: 'info', message: 'ok' },
  });
});

test('errors an administrator should see', () => {
  const bad = managed({ server_url: 'not a url', idle_lock_seconds: 'soon', allow_screenshots: false });
  const states = byKey(feedbackStates(signedOut, bad.config, bad.delivery, false));
  expect(states.managed_config).toEqual({ key: 'managed_config', severity: 'error', message: 'ignored_keys', data: 'idle_lock_seconds' });
  expect(states.connection).toEqual({ key: 'connection', severity: 'error', message: 'managed_server_invalid' });

  const other = managed({ server_url: 'https://assigned.example.com' });
  expect(byKey(feedbackStates(signedIn, other.config, other.delivery, false)).connection).toEqual({ key: 'connection', severity: 'error', message: 'not_managed_server' });
  const none = managed({ require_device_lock: true });
  expect(byKey(feedbackStates(signedIn, none.config, none.delivery, false)).connection).toEqual({ key: 'connection', severity: 'info', message: 'unmanaged_server' });

  const ok = managed({ server_url: ORIGIN });
  const revoked = byKey(feedbackStates({ ...signedOut, endReason: 'device_revoked' }, ok.config, ok.delivery, false));
  expect(revoked.sign_in).toEqual({ key: 'sign_in', severity: 'error', message: 'signed_out_device_revoked' });
  expect(byKey(feedbackStates({ ...signedOut, endReason: 'user_logout' }, ok.config, ok.delivery, false)).sign_in.severity).toBe('info');

  const upgrade = byKey(feedbackStates({ ...signedIn, upgradeRequired: { minVersion: '2.4.0' } }, ok.config, ok.delivery, false));
  expect(upgrade.policy).toEqual({ key: 'policy', severity: 'error', message: 'update_required', data: '2.4.0' });
  // A minimum that is not a plain version is not passed on.
  expect(byKey(feedbackStates({ ...signedIn, upgradeRequired: { minVersion: '<b>x' } }, ok.config, ok.delivery, false)).policy.data).toBeUndefined();
});

test('never user content: only fixed keys, codes, managed key names and versions', () => {
  const secret = 'alex@acme.example.com';
  const sessions = [
    signedIn,
    signedOut,
    { ...signedOut, endReason: 'credential_rejected' as const },
    { ...signedIn, serverOrigin: `https://${secret.replace('@', '.')}`, upgradeRequired: { minVersion: '9.9.9' } },
  ];
  const configs = [
    managed({ server_url: ORIGIN, lock_server: true }),
    managed({ server_url: `https://${secret}/x?token=abc`, idle_lock_seconds: -1, unknown_key: secret }),
    managed({ allow_screenshots: 'no' }),
  ];
  const keyNames = /^[a-z_]+(,[a-z_]+)*$/;
  for (const session of sessions) {
    for (const { config, delivery } of configs) {
      for (const state of feedbackStates(session, config, delivery, false)) {
        expect(FEEDBACK_KEYS).toContain(state.key);
        expect(FEEDBACK_CODES[state.key] as readonly string[]).toContain(state.message);
        if (state.data !== undefined) expect(state.data).toMatch(state.key === 'policy' ? /^\d+\.\d+\.\d+$/ : keyNames);
        expect(JSON.stringify(state)).not.toMatch(/acme|example|token|@|https?:/);
      }
    }
  }
});

test('ignored keys name only the keys the app reads', () => {
  const raw = { unknown_key: 'x', idle_lock_seconds: 'soon', lock_server: 'yes' };
  expect(managedDelivery(raw).ignoredKeys).toEqual(['lock_server', 'idle_lock_seconds']);
});

test('iOS: the managed feedback dictionary, written on change and removed when dormant', () => {
  jest.replaceProperty(Platform, 'OS', 'ios');
  const { config, delivery } = managed({ server_url: ORIGIN });
  const states = feedbackStates(signedIn, config, delivery, false);
  reportFeedback(states);
  reportFeedback(states);
  expect(Settings.set).toHaveBeenCalledTimes(1);
  expect(Settings.set).toHaveBeenCalledWith({
    [FEEDBACK_KEY]: { managed_config: 'applied', connection: 'managed_server', sign_in: 'signed_in', policy: 'ok' },
  });
  reportFeedback([]);
  expect(Settings.set).toHaveBeenLastCalledWith({ [FEEDBACK_KEY]: null });
  reportFeedback([]);
  expect(Settings.set).toHaveBeenCalledTimes(2);
});

test('iOS: never writes the key on a device that was never managed', () => {
  jest.replaceProperty(Platform, 'OS', 'ios');
  reportFeedback([]);
  expect(Settings.set).not.toHaveBeenCalled();
});

test('the dictionary joins a code and its data', () => {
  expect(feedbackDictionary([{ key: 'policy', severity: 'error', message: 'update_required', data: '2.4.0' }])).toEqual({ policy: 'update_required:2.4.0' });
});

test('Android: keyed app states through the inline module', () => {
  jest.replaceProperty(Platform, 'OS', 'android');
  const native = { reportStates: jest.fn() };
  setKeyedAppStatesLoader((name) => (name === 'ManagedRestrictions' ? native : null));
  const { config, delivery } = managed({ server_url: ORIGIN, idle_lock_seconds: 'soon' });
  reportFeedback(feedbackStates(signedIn, config, delivery, false));
  expect(native.reportStates).toHaveBeenCalledWith([
    { key: 'managed_config', severity: 'error', message: 'ignored_keys', data: 'idle_lock_seconds' },
    { key: 'connection', severity: 'info', message: 'managed_server', data: null },
    { key: 'sign_in', severity: 'info', message: 'signed_in', data: null },
    { key: 'policy', severity: 'info', message: 'ok', data: null },
  ]);
  // The MDM removes the configuration: Android keeps keyed states, so they are replaced once.
  reportFeedback([]);
  expect(native.reportStates).toHaveBeenLastCalledWith(FEEDBACK_KEYS.map((key) => ({ key, severity: 'info', message: 'not_managed', data: null })));
  native.reportStates.mockClear();
  reportFeedback([]);
  expect(native.reportStates).not.toHaveBeenCalled();
  // Missing module (Expo Go) and a throwing one are both survivable.
  setKeyedAppStatesLoader(() => null);
  expect(() => reportFeedback([{ key: 'policy', severity: 'info', message: 'ok' }])).not.toThrow();
  setKeyedAppStatesLoader(() => ({ reportStates: () => { throw new Error('binder'); } }));
  expect(() => reportFeedback([{ key: 'policy', severity: 'error', message: 'update_required' }])).not.toThrow();
});

test('nothing is reported before the session is restored', () => {
  jest.replaceProperty(Platform, 'OS', 'ios');
  const session = require('@/state/session') as typeof import('@/state/session');
  const config = require('@/security/managedConfig') as typeof import('@/security/managedConfig');
  config.refreshManagedConfig(() => ({ server_url: ORIGIN }));
  session.setSession({ hydrated: false });
  const { startManagedFeedback } = require('@/security/managedFeedback') as typeof import('@/security/managedFeedback');
  startManagedFeedback();
  expect(Settings.set).not.toHaveBeenCalled();
  session.setSession({ hydrated: true });
  expect(Settings.set).toHaveBeenCalledTimes(1);
  config.refreshManagedConfig(() => null);
});
