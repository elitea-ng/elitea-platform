import { onlineManager } from '@tanstack/react-query';
import { addNetworkStateListener, getNetworkStateAsync, type NetworkState } from 'expo-network';
import { useSyncExternalStore } from 'react';
import { Platform } from 'react-native';

/**
 * The only use of `expo-network`: is the device online? Unknown counts as
 * online (the request itself is the real test); only a definite
 * "not connected" or "internet not reachable" counts as offline.
 *
 * react-query's onlineManager follows it (refetch on reconnect), and the
 * outbox drains and the sync runs when it comes back.
 */

export function isOnlineState(state: Pick<NetworkState, 'isConnected' | 'isInternetReachable'>): boolean {
  return state.isConnected !== false && state.isInternetReachable !== false;
}

type Listener = (online: boolean) => void;

let online = true;
let wired = false;
const listeners = new Set<Listener>();

function set(next: boolean): void {
  if (next === online) return;
  online = next;
  for (const listener of [...listeners]) listener(online);
}

export const network = {
  isOnline: (): boolean => online,
  /** Called on every change; `true` = back online. */
  subscribe(listener: Listener): () => void {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  },
};

const subscribeOnline = (listener: () => void) => network.subscribe(() => listener());

/** The device's online state, re-rendering on change. */
export function useOnline(): boolean {
  return useSyncExternalStore(subscribeOnline, network.isOnline, network.isOnline);
}

/** Starts listening (idempotent). */
export function wireNetwork(): void {
  if (wired || Platform.OS === 'web') return;
  wired = true;
  addNetworkStateListener((state) => set(isOnlineState(state)));
  getNetworkStateAsync()
    .then((state) => set(isOnlineState(state)))
    .catch(() => {});
  onlineManager.setEventListener((setOnline) => network.subscribe(setOnline));
}

/** Test helper. */
export function __setOnlineForTests(next: boolean): void {
  set(next);
}
