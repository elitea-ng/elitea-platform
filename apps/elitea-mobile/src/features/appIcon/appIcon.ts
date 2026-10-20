import * as AlternateAppIcons from 'expo-alternate-app-icons';
import { useSyncExternalStore } from 'react';
import { Alert, Platform } from 'react-native';

import type { DiscoveryDocument } from '@/api/discovery';
import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import { DEVICE_KEYS, getDeviceItem, setDeviceItem } from '@/security/secureStorage';

/**
 * The bundled app icons (docs/design-system.md §3.6, docs/assets.md): "Zefir" (the default,
 * Zefirca on the pastel gradient) and "Zefir Night" (the same head mark on a neutral slate
 * gradient, for self-hosted workspaces). Exactly two, bundled, never downloaded, never carrying a
 * customer logo; switching is always the user's choice and the OS confirms it (iOS shows its own
 * "You have changed the icon" alert).
 *
 * Native side: `expo-alternate-app-icons`. iOS: an `AppIcon`-style asset catalog set
 * `ZefirNight` (light, dark and tinted appearances) named in
 * `ASSETCATALOG_COMPILER_ALTERNATE_APPICON_NAMES`; Android: an `<activity-alias>`
 * `.MainActivityZefirNight` with its own adaptive icon (background, foreground and monochrome
 * layers), enabled with `PackageManager.setComponentEnabledSetting`.
 */

/** The alternate icon's name in the native projects (`app.config.ts`, PascalCase). */
export const WORKSPACE_ICON_NAME = 'ZefirNight';

export type AppIconChoice = 'default' | 'workspace';

interface NativeIcons {
  supportsAlternateIcons: boolean;
  setAlternateAppIcon(name: string | null): Promise<string | null>;
  getAppIconName(): string | null;
}

/** The native module (tests pass their own). */
function native(): NativeIcons | null {
  return AlternateAppIcons;
}

let current: AppIconChoice | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of [...listeners]) listener();
}

/** Whether this device can change the app icon at all. */
export function appIconSupported(module: NativeIcons | null = native()): boolean {
  try {
    return module?.supportsAlternateIcons === true;
  } catch {
    return false;
  }
}

/** The icon in use, as the OS reports it. */
export function getAppIcon(module: NativeIcons | null = native()): AppIconChoice {
  if (current !== null) return current;
  try {
    current = module?.getAppIconName() === WORKSPACE_ICON_NAME ? 'workspace' : 'default';
  } catch {
    current = 'default';
  }
  return current;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

const read = () => getAppIcon();

export function useAppIcon(): AppIconChoice {
  return useSyncExternalStore(subscribe, read, read);
}

/**
 * Switches the icon. Resolves true when the OS took it (iOS then shows its own alert; Android may
 * close the app on some launchers, which the offer's copy says).
 */
export async function setAppIcon(choice: AppIconChoice, module: NativeIcons | null = native()): Promise<boolean> {
  if (!appIconSupported(module) || module === null) return false;
  try {
    await module.setAlternateAppIcon(choice === 'workspace' ? WORKSPACE_ICON_NAME : null);
    current = choice;
    emit();
    return true;
  } catch (error) {
    log.warn('app icon not changed', { error: String(error) });
    return false;
  }
}

/**
 * Whether connecting this workspace offers the workspace icon: a self-hosted workspace, an icon
 * the device can change, still the default icon, and never offered before on this device.
 */
export function shouldOfferWorkspaceIcon(
  document: Pick<DiscoveryDocument, 'deployment_kind'>,
  state: { supported: boolean; current: AppIconChoice; offered: boolean },
): boolean {
  return document.deployment_kind === 'self_hosted' && state.supported && state.current === 'default' && !state.offered;
}

/** The offer's copy (Android says the app may restart: some launchers close it on the switch). */
export function workspaceIconOfferText(platform: string = Platform.OS): { title: string; message: string; accept: string; decline: string } {
  return {
    title: 'Use the workspace app icon?',
    message:
      `${identity.displayName} has a second, neutral icon for self-hosted workspaces. ` +
      'You can switch back any time in Settings → Appearance.' +
      (platform === 'android' ? ' The app may close while the icon changes.' : ''),
    accept: 'Use workspace icon',
    decline: 'Not now',
  };
}

type Ask = (text: ReturnType<typeof workspaceIconOfferText>) => Promise<boolean>;

const askWithAlert: Ask = (text) =>
  new Promise((resolve) => {
    Alert.alert(text.title, text.message, [
      { text: text.decline, style: 'cancel', onPress: () => resolve(false) },
      { text: text.accept, onPress: () => resolve(true) },
    ], { cancelable: true, onDismiss: () => resolve(false) });
  });

/**
 * Offers the workspace icon ONCE per device, after connecting a self-hosted workspace (§3.6).
 * The offer is recorded before it is shown, so a crash or a second connect never asks again.
 * Resolves whether the icon was switched. Never throws.
 */
export async function offerWorkspaceIconOnce(
  document: Pick<DiscoveryDocument, 'deployment_kind'>,
  deps: { ask?: Ask; module?: NativeIcons | null } = {},
): Promise<boolean> {
  const module = deps.module === undefined ? native() : deps.module;
  let offered = true;
  try {
    offered = (await getDeviceItem(DEVICE_KEYS.appIconOffered)) !== null;
  } catch {
    return false;
  }
  if (!shouldOfferWorkspaceIcon(document, { supported: appIconSupported(module), current: getAppIcon(module), offered })) return false;
  try {
    await setDeviceItem(DEVICE_KEYS.appIconOffered, '1');
  } catch {
    return false;
  }
  const accepted = await (deps.ask ?? askWithAlert)(workspaceIconOfferText());
  return accepted ? setAppIcon('workspace', module) : false;
}

/** Test seam. */
export function __resetAppIconForTests(): void {
  current = null;
  listeners.clear();
}
