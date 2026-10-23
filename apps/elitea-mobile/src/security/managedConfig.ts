import { DeviceEventEmitter, Platform, Settings } from 'react-native';
import { useSyncExternalStore } from 'react';
import { z } from 'zod';

import { log } from '@/lib/log';

/**
 * Managed app configuration (MDM AppConfig, iOS): the dictionary an MDM
 * pushes into the app's managed UserDefaults under
 * `com.apple.configuration.managed`, read through React Native's Settings
 * (NSUserDefaults) module. Keys (see docs/mdm-appconfig.md):
 *
 * - `server_url`: the workspace address the connect screen starts with;
 * - `require_device_lock`, `idle_lock_seconds`, `allow_screenshots`: a lock
 *   policy FLOOR. It only ever tightens what a workspace sends (true lock,
 *   the shorter idle time, never "0 = no relock"; screenshots off); it never loosens one.
 *
 * Device configuration, not workspace data: it is not wiped with a
 * workspace, and a value of the wrong type is ignored (never trusted half).
 * Android managed configurations are not read yet.
 */

export const MANAGED_KEY = 'com.apple.configuration.managed';

export interface ManagedConfig {
  serverUrl: string | null;
  requireDeviceLock: boolean | null;
  idleLockSeconds: number | null;
  allowScreenshots: boolean | null;
}

export const NO_MANAGED_CONFIG: Readonly<ManagedConfig> = Object.freeze({
  serverUrl: null,
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

function readNative(): unknown {
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
 * defaults change). It listens to `settingsUpdated` itself rather than
 * Settings.watchKeys: watchKeys fires only for keys present in the body, so
 * the MDM REMOVING the AppConfig (or unenrolling the device) would never be
 * seen and the lock floor would stay until the next launch.
 */
export function startManagedConfig(): void {
  refreshManagedConfig();
  if (watching || Platform.OS !== 'ios') return;
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
