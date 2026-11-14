import { requireOptionalNativeModule } from 'expo-modules-core';
import { useSyncExternalStore } from 'react';

import { log } from '@/lib/log';

/**
 * The system's low-data state (ADR-0050 decision 6, #1650), which `expo-network` does not
 * expose: iOS `NWPath.isConstrained || isExpensive`, Android Data Saver
 * (`getRestrictBackgroundStatus() == ENABLED`). Read through the inline module
 * native-modules/DataSaver.{swift,kt}. Absent in Expo Go and in tests: then it is false and the
 * avatars animate as before.
 */
export interface DataSaverNative {
  isLowData(): boolean;
  addListener(event: 'onChange', listener: (change: { lowData?: unknown }) => void): { remove(): void };
}

type LoadNative = (name: string) => DataSaverNative | null;
let loadNative: LoadNative = (name) => requireOptionalNativeModule<DataSaverNative>(name);

let current = false;
let started = false;
let subscription: { remove(): void } | null = null;
const listeners = new Set<() => void>();

function set(next: boolean): void {
  if (next === current) return;
  current = next;
  for (const listener of listeners) listener();
}

function read(native: DataSaverNative): boolean {
  try {
    return native.isLowData() === true;
  } catch (error) {
    log.info('data saver state not read', { error });
    return false;
  }
}

function start(): void {
  if (started) return;
  const native = loadNative('DataSaver');
  if (native === null) return;
  started = true;
  current = read(native);
  try {
    subscription = native.addListener('onChange', (change) => set(typeof change.lowData === 'boolean' ? change.lowData : read(native)));
  } catch (error) {
    log.info('data saver not watched', { error });
  }
}

function stop(): void {
  subscription?.remove();
  subscription = null;
  started = false;
}

/** Tests only: the inline module stand-in. Resets the watcher. */
export function setDataSaverLoader(load: LoadNative): void {
  stop();
  current = false;
  loadNative = load;
}

/** Whether the system asks apps to save data right now. */
export function isDataSaverOn(): boolean {
  start();
  return current;
}

function subscribe(listener: () => void): () => void {
  start();
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) stop();
  };
}

/** The low-data state, following changes while the component is mounted. */
export function useDataSaver(): boolean {
  return useSyncExternalStore(subscribe, isDataSaverOn, () => false);
}
