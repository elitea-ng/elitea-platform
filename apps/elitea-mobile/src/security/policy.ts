import { useSyncExternalStore } from 'react';

import { registerWipeHook } from '@/security/endSession';
import { getManagedConfig, subscribeManagedConfig, type ManagedConfig } from '@/security/managedConfig';

/**
 * Client policy: how strictly this device must protect workspace data.
 *
 * Fed by src/security/policyStore.ts from the workspace (ADR-0025 WP4): the
 * full policy of the latest token response, else the discovery document's
 * public subset, else these STRICT defaults (the 1.3 data controls default
 * to the server's own defaults, see `DATA_CONTROL_DEFAULTS`).
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
  /** Client contract 1.3 data controls (see `DataControls`). */
  allow_share_out: boolean;
  allow_share_in: boolean;
  allow_cloud_stt: boolean;
  notification_preview: NotificationPreview;
  allow_notification_actions: boolean;
  allow_system_surfaces: boolean;
}

/** What a notification may show on the lock screen. */
export type NotificationPreview = 'none' | 'title';

/**
 * The client contract 1.3 data controls of the native client policy. Their
 * defaults are the server's own defaults for an unsaved policy (owner
 * decision, SaaS behaviour), so a server before 1.3, which sends none of
 * them, behaves like a default 1.3 server:
 *
 * - `allow_share_out` (copy, share, export): on;
 * - `allow_share_in` (the system share sheet into the app): on;
 * - `allow_cloud_stt` (dictation off the device): OFF, on-device only;
 * - `notification_preview`: `none`;
 * - `allow_notification_actions`: on;
 * - `allow_system_surfaces` (titles on widgets and quick actions): OFF,
 *   counts only.
 *
 * An enterprise admin turns each one off (or `preview` to `none`); a
 * discovery document can only tighten them (src/security/policyStore.ts).
 */
export type DataControls = Pick<
  ClientPolicy,
  | 'allow_share_out'
  | 'allow_share_in'
  | 'allow_cloud_stt'
  | 'notification_preview'
  | 'allow_notification_actions'
  | 'allow_system_surfaces'
>;

export const DATA_CONTROL_DEFAULTS: Readonly<DataControls> = Object.freeze({
  allow_share_out: true,
  allow_share_in: true,
  allow_cloud_stt: false,
  notification_preview: 'none',
  allow_notification_actions: true,
  allow_system_surfaces: false,
});

const DATA_CONTROL_FLAGS = [
  'allow_share_out',
  'allow_share_in',
  'allow_cloud_stt',
  'allow_notification_actions',
  'allow_system_surfaces',
] as const;

/** The data controls of a server object; a wrong type or an unknown preview reads as absent. */
export function dataControlsFrom(raw: Record<string, unknown>): Partial<DataControls> {
  const out: Partial<DataControls> = {};
  for (const key of DATA_CONTROL_FLAGS) {
    if (typeof raw[key] === 'boolean') out[key] = raw[key];
  }
  const preview = raw.notification_preview;
  if (preview === 'none' || preview === 'title') out.notification_preview = preview;
  else if (preview !== undefined) out.notification_preview = 'none';
  return out;
}

/** The stricter of two sets of data controls: an off wins, and `none` wins over `title`. */
export function tightenControls(base: Partial<DataControls>, by: Partial<DataControls>): Partial<DataControls> {
  const out: Partial<DataControls> = { ...base };
  for (const key of DATA_CONTROL_FLAGS) {
    if (by[key] === false) out[key] = false;
  }
  if (by.notification_preview === 'none') out.notification_preview = 'none';
  return out;
}

export type PolicySource = 'default' | 'discovery' | 'server';

export const STRICT_DEFAULTS: Readonly<ClientPolicy> = Object.freeze({
  require_device_lock: true,
  idle_lock_seconds: 300,
  allow_screenshots: false,
  offline_retention_days: 0,
  offline_max_mb: 0,
  offline_attachments: false,
  ...DATA_CONTROL_DEFAULTS,
});

export interface PolicyState {
  policy: Readonly<ClientPolicy>;
  source: PolicySource;
  /** Present (true) when the device's managed configuration (MDM) tightened the lock policy. */
  managed?: boolean;
}

/**
 * The managed (MDM) lock floor: it only tightens. A required lock wins, the
 * shorter (non-zero) idle time wins, and screenshots off wins (src/security/managedConfig.ts).
 */
export function applyManagedFloor(policy: ClientPolicy, managed: ManagedConfig): { policy: ClientPolicy; tightened: boolean } {
  const next = { ...policy };
  if (managed.requireDeviceLock === true) next.require_device_lock = true;
  // `idle_lock_seconds` 0 means "never relock on resume", the loosest value: a
  // managed 0 changes nothing, and any managed positive time beats a 0.
  const idle = managed.idleLockSeconds;
  if (idle !== null && idle > 0 && next.require_device_lock && (next.idle_lock_seconds === 0 || idle < next.idle_lock_seconds)) {
    next.idle_lock_seconds = idle;
  }
  if (managed.allowScreenshots === false) next.allow_screenshots = false;
  const tightened =
    next.require_device_lock !== policy.require_device_lock ||
    next.idle_lock_seconds !== policy.idle_lock_seconds ||
    next.allow_screenshots !== policy.allow_screenshots;
  return { policy: next, tightened };
}

function withFloor(policy: ClientPolicy, source: PolicySource): PolicyState {
  const floored = applyManagedFloor(policy, getManagedConfig());
  return { policy: Object.freeze(floored.policy), source, ...(floored.tightened ? { managed: true } : {}) };
}

/** The policy as the workspace set it, before the managed floor (re-floored when MDM changes). */
let unfloored: { policy: ClientPolicy; source: PolicySource } = { policy: STRICT_DEFAULTS, source: 'default' };
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
  Object.assign(next, dataControlsFrom(policy as Record<string, unknown>));
  unfloored = { policy: next, source };
  state = withFloor(next, source);
  for (const listener of [...listeners]) listener();
}

/** Back to the strict defaults (end of session). */
export function resetPolicy(): void {
  unfloored = { policy: STRICT_DEFAULTS, source: 'default' };
  state = withFloor(STRICT_DEFAULTS, 'default');
  for (const listener of [...listeners]) listener();
}

// An MDM push while the app runs re-applies the floor at once (the lock controller follows).
subscribeManagedConfig(() => {
  state = withFloor(unfloored.policy, unfloored.source);
  for (const listener of [...listeners]) listener();
});

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

/**
 * `allow_share_out` for a component that shows workspace content: with it
 * off, nothing may be copied or shared, and that includes the system
 * text-selection menu (Copy, Share, Look Up), so such text is not
 * selectable. Read here, at the leaf, so no screen can forget to pass it.
 */
export function useShareOut(): boolean {
  return usePolicy().policy.allow_share_out;
}
