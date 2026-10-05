import { useSyncExternalStore } from 'react';

import { registerWipeHook } from '@/security/endSession';

/**
 * Client policy: how strictly this device must protect workspace data.
 *
 * Fed by src/security/policyStore.ts from the workspace (ADR-0025 WP4): the
 * full policy of the latest token response, else the discovery document's
 * public subset, else these STRICT defaults.
 */
export interface ClientPolicy {
  /** Device lock (biometrics or passcode) on launch and after `idle_lock_seconds` in the background. */
  require_device_lock: boolean;
  /** Seconds in the background before resuming requires unlocking again; 0 = never relock on resume. */
  idle_lock_seconds: number;
  /** When false, Android sets FLAG_SECURE (no screenshots, blank Recents). */
  allow_screenshots: boolean;
  /** Days of offline data kept on the device; 0 = no offline store (B4). */
  offline_retention_days: number;
  /** Size cap of the offline store in MB; 0 = none (B4). */
  offline_max_mb: number;
  /** Whether attachments may be cached on the device (B4). */
  offline_attachments: boolean;
}

export type PolicySource = 'default' | 'discovery' | 'server';

export const STRICT_DEFAULTS: Readonly<ClientPolicy> = Object.freeze({
  require_device_lock: true,
  idle_lock_seconds: 300,
  allow_screenshots: false,
  offline_retention_days: 0,
  offline_max_mb: 0,
  offline_attachments: false,
});

export interface PolicyState {
  policy: Readonly<ClientPolicy>;
  source: PolicySource;
}

let state: PolicyState = { policy: STRICT_DEFAULTS, source: 'default' };
const listeners = new Set<() => void>();

export function getPolicy(): Readonly<ClientPolicy> {
  return state.policy;
}

export function getPolicyState(): PolicyState {
  return state;
}

/** Replaces the policy (B4: from the server). Unknown/invalid values keep the strict default. */
export function setPolicy(policy: Partial<ClientPolicy>, source: PolicySource): void {
  const next: ClientPolicy = { ...STRICT_DEFAULTS };
  if (typeof policy.require_device_lock === 'boolean') next.require_device_lock = policy.require_device_lock;
  if (Number.isSafeInteger(policy.idle_lock_seconds) && policy.idle_lock_seconds! >= 0) {
    next.idle_lock_seconds = policy.idle_lock_seconds!;
  }
  if (typeof policy.allow_screenshots === 'boolean') next.allow_screenshots = policy.allow_screenshots;
  if (Number.isSafeInteger(policy.offline_retention_days) && policy.offline_retention_days! >= 0) {
    next.offline_retention_days = policy.offline_retention_days!;
  }
  if (Number.isSafeInteger(policy.offline_max_mb) && policy.offline_max_mb! >= 0) {
    next.offline_max_mb = policy.offline_max_mb!;
  }
  if (typeof policy.offline_attachments === 'boolean') next.offline_attachments = policy.offline_attachments;
  state = { policy: Object.freeze(next), source };
  for (const listener of [...listeners]) listener();
}

/** Back to the strict defaults (end of session). */
export function resetPolicy(): void {
  state = { policy: STRICT_DEFAULTS, source: 'default' };
  for (const listener of [...listeners]) listener();
}

// A policy belongs to the workspace session that delivered it.
registerWipeHook('policy', resetPolicy);

export function subscribePolicy(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function usePolicy(): PolicyState {
  return useSyncExternalStore(subscribePolicy, getPolicyState, getPolicyState);
}
