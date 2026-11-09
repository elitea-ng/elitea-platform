import { requireOptionalNativeModule } from 'expo-modules-core';
import { DeviceEventEmitter, Platform, Settings } from 'react-native';
import { useSyncExternalStore } from 'react';
import { z } from 'zod';

import { log } from '@/lib/log';

/**
 * Managed app configuration, one parser for both platforms (see docs/mdm-appconfig.md):
 *
 * - iOS (MDM AppConfig): the dictionary an MDM pushes into the app's managed
 *   UserDefaults under `com.apple.configuration.managed`, read through React
 *   Native's Settings (NSUserDefaults) module;
 * - Android (EMM managed configuration): the app restrictions an EMM sets,
 *   declared by plugins/withManagedConfiguration.js and read by the inline
 *   module native-modules/ManagedRestrictions.kt.
 *
 * Keys:
 *
 * - `server_url`: the workspace address; the connect screen shows it read only;
 * - `lock_server`: with `server_url`, the app keeps only that workspace;
 * - `require_device_lock`, `idle_lock_seconds`, `allow_screenshots`: a lock
 *   policy FLOOR. It only ever tightens what a workspace sends (true lock,
 *   the shorter idle time, never "0 = no relock"; screenshots off); it never loosens one.
 *
 * Device configuration, not workspace data: it is not wiped with a
 * workspace, and a value of the wrong type is ignored (never trusted half).
 */

export const MANAGED_KEY = 'com.apple.configuration.managed';

export interface ManagedConfig {
  serverUrl: string | null;
  lockServer: boolean | null;
  requireDeviceLock: boolean | null;
  idleLockSeconds: number | null;
  allowScreenshots: boolean | null;
}

export const NO_MANAGED_CONFIG: Readonly<ManagedConfig> = Object.freeze({
  serverUrl: null,
  lockServer: null,
  requireDeviceLock: null,
  idleLockSeconds: null,
  allowScreenshots: null,
});

const field = <T extends z.ZodType>(schema: T) => schema.optional().catch(undefined);
/**
 * A plist boolean. React Native's Settings module hands an NSNumber boolean to
 * JS as the number 1 or 0 (seen on iOS: `-bool true` reads back as 1), so 1/0
 * count as true/false. Lenient on purpose and safe: the floor only tightens.
 */
const plistBoolean = z.union([z.boolean(), z.literal(1).transform(() => true), z.literal(0).transform(() => false)]);
const Managed = z.looseObject({
  server_url: field(z.string().trim().min(1).max(2048)),
  lock_server: field(plistBoolean),
  require_device_lock: field(plistBoolean),
  idle_lock_seconds: field(z.number().int().min(0).max(86_400)),
  allow_screenshots: field(plistBoolean),
});

/** The managed dictionary as the app uses it; anything that is not a dictionary is "none". */
export function parseManagedConfig(raw: unknown): ManagedConfig {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return NO_MANAGED_CONFIG;
  const parsed = Managed.safeParse(raw);
  if (!parsed.success) return NO_MANAGED_CONFIG;
  const d = parsed.data;
  return {
    serverUrl: d.server_url ?? null,
    lockServer: d.lock_server ?? null,
    requireDeviceLock: d.require_device_lock ?? null,
    idleLockSeconds: d.idle_lock_seconds ?? null,
    allowScreenshots: d.allow_screenshots ?? null,
  };
}

export function hasManagedConfig(config: ManagedConfig): boolean {
  return Object.values(config).some((v) => v !== null);
}

/**
 * The latest `settingsUpdated` body (RCTSettingsManager sends the WHOLE
 * NSUserDefaults dictionary on every change). Once one has arrived it is the
 * truth: React Native's Settings cache, which Settings.get reads, only merges
 * the keys a body carries, so it keeps a managed dictionary the MDM removed.
 */
let latestDefaults: Record<string, unknown> | null = null;

/** The Android inline module (absent on iOS, in Expo Go and in tests). */
export interface ManagedRestrictionsNative {
  read(): Record<string, unknown>;
  addListener(event: 'onChange', listener: (restrictions: Record<string, unknown>) => void): { remove(): void };
}

type LoadNative = (name: string) => ManagedRestrictionsNative | null;
let loadNative: LoadNative = (name) => requireOptionalNativeModule<ManagedRestrictionsNative>(name);

/** Tests only: the Android inline module stand-in. */
export function setManagedRestrictionsLoader(load: LoadNative): void {
  loadNative = load;
}

function managedRestrictions(): ManagedRestrictionsNative | null {
  if (Platform.OS !== 'android') return null;
  return loadNative('ManagedRestrictions');
}

/** The managed keys as the current platform hands them over (null: none, or not readable). */
function readNative(): unknown {
  if (Platform.OS === 'android') {
    try {
      return managedRestrictions()?.read() ?? null;
    } catch (error) {
      log.info('managed configuration not readable', { error });
      return null;
    }
  }
  if (Platform.OS !== 'ios') return null;
  if (latestDefaults !== null) return latestDefaults[MANAGED_KEY] ?? null;
  try {
    return Settings.get(MANAGED_KEY);
  } catch (error) {
    log.info('managed configuration not readable', { error });
    return null;
  }
}

let current: ManagedConfig = NO_MANAGED_CONFIG;
let currentKey = JSON.stringify(current);
const listeners = new Set<() => void>();

export function getManagedConfig(): ManagedConfig {
  return current;
}

/** Re-reads the managed dictionary (launch, an MDM push while running). */
export function refreshManagedConfig(read: () => unknown = readNative): ManagedConfig {
  const next = parseManagedConfig(read());
  const key = JSON.stringify(next);
  if (key !== currentKey) {
    current = next;
    currentKey = key;
    for (const listener of [...listeners]) listener();
  }
  return current;
}

export function subscribeManagedConfig(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useManagedConfig(): ManagedConfig {
  return useSyncExternalStore(subscribeManagedConfig, getManagedConfig, getManagedConfig);
}

let watching = false;

/**
 * Reads it now and follows MDM pushes while the app runs (iOS posts a
 * defaults change; Android broadcasts ACTION_APPLICATION_RESTRICTIONS_CHANGED,
 * which the inline module forwards as `onChange`, also when the EMM removes
 * every key). On iOS it listens to `settingsUpdated` itself rather than
 * Settings.watchKeys: watchKeys fires only for keys present in the body, so
 * the MDM REMOVING the AppConfig (or unenrolling the device) would never be
 * seen and the lock floor would stay until the next launch.
 */
export function startManagedConfig(): void {
  refreshManagedConfig();
  if (watching) return;
  if (Platform.OS === 'android') {
    const native = managedRestrictions();
    if (native === null) return;
    watching = true;
    try {
      // The event carries the whole new map; re-read rather than trust it half.
      native.addListener('onChange', () => refreshManagedConfig());
    } catch (error) {
      log.info('managed configuration not watched', { error });
    }
    return;
  }
  if (Platform.OS !== 'ios') return;
  watching = true;
  try {
    DeviceEventEmitter.addListener('settingsUpdated', (body: unknown) => {
      if (body === null || typeof body !== 'object' || Array.isArray(body)) return;
      latestDefaults = body as Record<string, unknown>;
      refreshManagedConfig();
    });
  } catch (error) {
    log.info('managed configuration not watched', { error });
  }
}
