import { useSyncExternalStore } from 'react';
import { Appearance } from 'react-native';

import type { SchemePreference } from '@/design/resolve';
import { log } from '@/lib/log';
import { DEVICE_KEYS, getDeviceItem, setDeviceItem } from '@/security/secureStorage';

/**
 * Settings → Appearance (docs/design-system.md §3.4): System, Light or Dark, per device. The OS
 * setting rules by default and a brand pack never forces a scheme.
 *
 * The choice is applied with `Appearance.setColorScheme`, so the native chrome (navigation bars,
 * the tab bar, sheets, the keyboard, Liquid Glass) follows it together with the app's tokens:
 * `useColorScheme()` then reports the chosen scheme everywhere.
 */

const VALUES: readonly SchemePreference[] = ['system', 'light', 'dark'];

let current: SchemePreference = 'system';
const listeners = new Set<() => void>();

export function parseSchemePreference(raw: string | null | undefined): SchemePreference {
  return VALUES.includes(raw as SchemePreference) ? (raw as SchemePreference) : 'system';
}

function apply(preference: SchemePreference): void {
  try {
    Appearance.setColorScheme(preference === 'system' ? 'auto' : preference);
  } catch {
    // No native appearance module (tests, web): the tokens still follow `useColorScheme()`.
  }
}

function set(preference: SchemePreference): void {
  if (current === preference) return;
  current = preference;
  apply(preference);
  for (const listener of [...listeners]) listener();
}

export function getSchemePreference(): SchemePreference {
  return current;
}

/** Restores the stored choice (launch). A failed read keeps "System". */
export async function hydrateSchemePreference(): Promise<void> {
  try {
    set(parseSchemePreference(await getDeviceItem(DEVICE_KEYS.appearance)));
  } catch (error) {
    log.warn('appearance: could not read the stored scheme', { error: String(error) });
  }
}

/** Applies a new choice at once and stores it. */
export async function setSchemePreference(preference: SchemePreference): Promise<void> {
  set(preference);
  try {
    await setDeviceItem(DEVICE_KEYS.appearance, preference);
  } catch (error) {
    log.warn('appearance: could not store the scheme', { error: String(error) });
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useSchemePreference(): SchemePreference {
  return useSyncExternalStore(subscribe, getSchemePreference, getSchemePreference);
}

/** Test seam. */
export function resetSchemePreferenceForTests(): void {
  current = 'system';
  listeners.clear();
}
