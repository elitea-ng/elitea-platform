import { requireOptionalNativeModule } from 'expo-modules-core';
import { Platform, Settings } from 'react-native';

import { log } from '@/lib/log';
import {
  getManagedConfig,
  getManagedDelivery,
  subscribeManagedConfig,
  type ManagedConfig,
  type ManagedDelivery,
} from '@/security/managedConfig';
import { managedServerOrigin } from '@/security/managedServer';
import { getSession, subscribeSession, type EndReason, type SessionState } from '@/state/session';

/**
 * MDM feedback (ADR-0052 decision 7, W6.1): the app tells the device management
 * how it is doing, so an administrator sees a misconfigured or signed-out app in
 * the console without asking the user.
 *
 * - iOS: the managed feedback dictionary, `com.apple.feedback.managed` in the
 *   app's UserDefaults, which the MDM reads with ManagedApplicationFeedback.
 * - Android: keyed app states (androidx.enterprise KeyedAppStatesReporter,
 *   through the inline module native-modules/ManagedRestrictions.kt) to the
 *   device or profile owner and Play.
 *
 * Dormant: it reports only while the device has a managed configuration. When
 * the MDM removes it, the iOS dictionary is removed; nothing is reported again.
 *
 * NEVER USER CONTENT. A state is a fixed key, a fixed severity, a fixed code
 * from the lists below and, at most, managed key names or a version number.
 * No server address (not even the managed one), no account, project, name,
 * email, message or error text. `feedbackStates` is the only place states are
 * made, and its test holds every value to those lists.
 */

export const FEEDBACK_KEY = 'com.apple.feedback.managed';

export type FeedbackSeverity = 'info' | 'error';

export interface FeedbackState {
  /** A fixed key: `managed_config`, `connection`, `sign_in`, `policy`. */
  key: (typeof FEEDBACK_KEYS)[number];
  severity: FeedbackSeverity;
  /** A fixed code. */
  message: string;
  /** Managed key names, or a version number; never anything a user typed or received. */
  data?: string;
}

export const FEEDBACK_KEYS = ['managed_config', 'connection', 'sign_in', 'policy'] as const;

/** Every message code a state may carry. */
export const FEEDBACK_CODES = Object.freeze({
  managed_config: ['applied', 'ignored_keys'],
  connection: ['managed_server', 'other_server', 'managed_server_invalid', 'not_connected'],
  sign_in: ['signed_in', 'signed_out', 'signed_out_device_revoked', 'signed_out_credential_rejected', 'signed_out_policy', 'signed_out_managed_server'],
  policy: ['ok', 'update_required'],
} as const satisfies Record<(typeof FEEDBACK_KEYS)[number], readonly string[]>);

const SIGNED_OUT_BY: Partial<Record<EndReason, string>> = {
  device_revoked: 'signed_out_device_revoked',
  credential_rejected: 'signed_out_credential_rejected',
  policy: 'signed_out_policy',
  managed_server: 'signed_out_managed_server',
};

const VERSION = /^\d+\.\d+\.\d+$/;

/** The states to report now; empty when the device has no managed configuration (dormant). */
export function feedbackStates(
  session: Pick<SessionState, 'serverOrigin' | 'credential' | 'endReason' | 'upgradeRequired'>,
  managed: ManagedConfig,
  delivery: ManagedDelivery,
  allowPrivateHttp: boolean = __DEV__,
): FeedbackState[] {
  if (!delivery.present) return [];
  const states: FeedbackState[] = [];

  states.push(
    delivery.ignoredKeys.length > 0
      ? { key: 'managed_config', severity: 'error', message: 'ignored_keys', data: delivery.ignoredKeys.join(',') }
      : { key: 'managed_config', severity: 'info', message: 'applied' },
  );

  const assigned = managedServerOrigin(managed, allowPrivateHttp);
  if (managed.serverUrl !== null && assigned === null) {
    states.push({ key: 'connection', severity: 'error', message: 'managed_server_invalid' });
  } else if (session.serverOrigin === null) {
    states.push({ key: 'connection', severity: 'info', message: 'not_connected' });
  } else if (assigned !== null && session.serverOrigin !== assigned) {
    // Connected elsewhere while the MDM assigned a workspace: worth an administrator's look.
    states.push({ key: 'connection', severity: 'error', message: 'other_server' });
  } else {
    states.push({ key: 'connection', severity: 'info', message: assigned !== null ? 'managed_server' : 'other_server' });
  }

  if (session.credential !== null) {
    states.push({ key: 'sign_in', severity: 'info', message: 'signed_in' });
  } else {
    const code = session.endReason ? SIGNED_OUT_BY[session.endReason] : undefined;
    states.push({ key: 'sign_in', severity: code ? 'error' : 'info', message: code ?? 'signed_out' });
  }

  if (session.upgradeRequired !== null) {
    const min = session.upgradeRequired.minVersion;
    states.push({ key: 'policy', severity: 'error', message: 'update_required', ...(min && VERSION.test(min) ? { data: min } : {}) });
  } else {
    states.push({ key: 'policy', severity: 'info', message: 'ok' });
  }
  return states;
}

/** iOS: the managed feedback dictionary, one string per key (`<code>` or `<code>:<data>`). */
export function feedbackDictionary(states: readonly FeedbackState[]): Record<string, string> {
  return Object.fromEntries(states.map((s) => [s.key, s.data ? `${s.message}:${s.data}` : s.message]));
}

/** The Android inline module's reporter (absent on iOS, in Expo Go and in tests). */
export interface KeyedAppStatesNative {
  reportStates(states: { key: string; severity: FeedbackSeverity; message: string; data: string | null }[]): void;
}

type LoadNative = (name: string) => KeyedAppStatesNative | null;
let loadNative: LoadNative = (name) => requireOptionalNativeModule<KeyedAppStatesNative>(name);

/** Tests only. */
export function setKeyedAppStatesLoader(load: LoadNative): void {
  loadNative = load;
}

let lastKey: string | null = null;

/** Writes the states if they changed. An empty list removes the iOS dictionary once. */
export function reportFeedback(states: readonly FeedbackState[]): void {
  const key = JSON.stringify(states);
  if (key === lastKey) return;
  const wasReporting = lastKey !== null && lastKey !== '[]';
  lastKey = key;
  try {
    if (Platform.OS === 'ios') {
      // A null value removes the key (RCTSettingsManager setValues).
      if (states.length > 0) Settings.set({ [FEEDBACK_KEY]: feedbackDictionary(states) });
      else if (wasReporting) Settings.set({ [FEEDBACK_KEY]: null });
      return;
    }
    if (Platform.OS === 'android' && states.length > 0) {
      loadNative('ManagedRestrictions')?.reportStates(states.map((s) => ({ key: s.key, severity: s.severity, message: s.message, data: s.data ?? null })));
    }
  } catch (error) {
    log.info('managed feedback not reported', { error });
  }
}

function report(): void {
  reportFeedback(feedbackStates(getSession(), getManagedConfig(), getManagedDelivery()));
}

let started = false;

/** Reports now and on every change of the session or the managed configuration (idempotent). */
export function startManagedFeedback(): void {
  if (started) return;
  started = true;
  report();
  subscribeSession(report);
  subscribeManagedConfig(report);
}

/** Tests only. */
export function __resetManagedFeedbackForTests(): void {
  lastKey = null;
  started = false;
}
