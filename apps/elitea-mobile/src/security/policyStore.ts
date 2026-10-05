import { z } from 'zod';

import { getClientVersion } from '@/api/clientVersion';
import { setUpgradeRecheck } from '@/api/middleware';
import { minimumVersionFor, type DiscoveryDocument } from '@/api/discovery';
import { onTokenGrant } from '@/auth/nativeSession';
import { log } from '@/lib/log';
import { isBelowMinimum } from '@/lib/semver';
import { registerWipeHook } from '@/security/endSession';
import { getPolicyState, setPolicy, type ClientPolicy } from '@/security/policy';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import { getSession, setSession } from '@/state/session';
import { onDiscovery, refreshWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Feeds the client policy from the workspace (ADR-0025 decision 5, WP4).
 *
 * Precedence: the full `client_policy` of the latest token response (sign-in
 * and every refresh, so a change reaches the device within one access-token
 * lifetime) → the discovery document's public subset (before sign-in: only
 * `require_device_lock` is known, everything else stays strict) → the strict
 * defaults. A discovery document can only TIGHTEN a server policy
 * (`require_device_lock`), never loosen it.
 *
 * The server policy is persisted (az.policy, wiped with the workspace) so the
 * launch lock decision is made with it before any network call. A discovery
 * tightening is persisted onto it, so the next launch locks first too.
 *
 * Freshness (DEF-R1): the discovery document is re-read on every foreground
 * (src/state/foregroundRefresh.ts) and after every token response, so a
 * tightening reaches a running app within seconds, not at the next access
 * token refresh. A tightening applies at once (the lock controller locks
 * now); a loosening only comes with the next token response.
 *
 * Enforcement points: the lock controller (require_device_lock,
 * idle_lock_seconds; changes apply at once), the privacy hook
 * (allow_screenshots), and the minimum version (`min_client_version` from the
 * token response or discovery → the blocking update screen; the server also
 * answers 426), and the encrypted offline store (offline_*: src/db/, opened
 * only when the server policy keeps `offline_retention_days > 0`; a discovery
 * document's `offline_enabled: false` turns it off, as a tightening).
 */

const RawPolicy = z.record(z.string(), z.unknown());

/** The policy fields of a server object; wrong types are dropped (strict default kept). */
export function policyFrom(raw: Record<string, unknown>): Partial<ClientPolicy> {
  const out: Partial<ClientPolicy> = {};
  if (typeof raw.require_device_lock === 'boolean') out.require_device_lock = raw.require_device_lock;
  if (typeof raw.allow_screenshots === 'boolean') out.allow_screenshots = raw.allow_screenshots;
  if (typeof raw.offline_attachments === 'boolean') out.offline_attachments = raw.offline_attachments;
  const int = (value: unknown, max: number) =>
    typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 && value <= max ? value : undefined;
  const idle = int(raw.idle_lock_seconds, 86_400);
  if (idle !== undefined) out.idle_lock_seconds = idle;
  const retention = int(raw.offline_retention_days, 90);
  if (retention !== undefined) out.offline_retention_days = retention;
  const maxMb = int(raw.offline_max_mb, 102_400);
  if (maxMb !== undefined) out.offline_max_mb = maxMb;
  return out;
}

/**
 * Shows the blocking update screen when this app is below `minimum`, and
 * clears it when it no longer is (DEF-R4): a workspace that lowers or removes
 * its minimum lets the app back in on the next discovery read or token
 * response, without a relaunch. An empty or missing minimum means none.
 */
function enforceMinimum(minimum: unknown, clientVersion: string): void {
  if (typeof minimum === 'string' && minimum !== '' && isBelowMinimum(clientVersion, minimum)) {
    if (getSession().upgradeRequired?.minVersion !== minimum) {
      log.warn('workspace requires a newer app version');
      setSession({ upgradeRequired: { minVersion: minimum } });
    }
    return;
  }
  if (getSession().upgradeRequired !== null) {
    log.info('workspace accepts this app version again');
    setSession({ upgradeRequired: null });
  }
}

/** Last server policy received, kept to merge discovery tightenings onto. */
let serverPolicy: Partial<ClientPolicy> | null = null;
/** The raw `client_policy` it came from, persisted with any discovery tightening. */
let serverRaw: Record<string, unknown> | null = null;
let discoveryLock = false;
/** The discovery document says `offline_enabled: false`: no offline store, whatever the server policy says. */
let discoveryOfflineOff = false;

function publish(): void {
  if (serverPolicy !== null) {
    setPolicy(
      {
        ...serverPolicy,
        require_device_lock: serverPolicy.require_device_lock === true || discoveryLock,
        ...(discoveryOfflineOff ? { offline_retention_days: 0 } : {}),
      },
      'server',
    );
  }
}

/** A token response's `client_policy` (absent: keep the last one). */
export async function applyServerPolicy(
  raw: Record<string, unknown> | undefined,
  options: { origin?: string | null; clientVersion?: string; persist?: boolean } = {},
): Promise<void> {
  if (raw === undefined) return;
  serverPolicy = policyFrom(raw);
  serverRaw = raw;
  discoveryLock = false;
  discoveryOfflineOff = false;
  publish();
  enforceMinimum(raw.min_client_version, options.clientVersion ?? getClientVersion());
  const origin = options.origin ?? getSession().serverOrigin;
  if (options.persist === false || origin === null) return;
  await persist(origin, raw);
}

async function persist(origin: string, raw: Record<string, unknown>): Promise<void> {
  try {
    await setSecureItem(SECURE_KEYS.policy, JSON.stringify({ v: 1, origin, policy: raw }));
  } catch (error) {
    log.info('client policy not persisted', { error });
  }
}

/** The persisted server policy with the discovery tightenings applied. */
function persistTightening(): void {
  const origin = getSession().serverOrigin;
  if (serverRaw === null || origin === null) return;
  void persist(origin, {
    ...serverRaw,
    ...(discoveryLock ? { require_device_lock: true } : {}),
    ...(discoveryOfflineOff ? { offline_retention_days: 0 } : {}),
  });
}

/** A discovery document: its public subset before sign-in, a tightening after. */
export function applyDiscoveryPolicy(document: DiscoveryDocument, clientVersion: string = getClientVersion()): void {
  enforceMinimum(minimumVersionFor(document), clientVersion);
  const requireLock = document.client_policy.require_device_lock;
  const offlineOff = document.client_policy.offline_enabled === false;
  if (serverPolicy !== null) {
    let tightened = false;
    if (requireLock && !discoveryLock) {
      discoveryLock = true;
      tightened = true;
    }
    if (offlineOff && !discoveryOfflineOff) {
      discoveryOfflineOff = true;
      tightened = true;
    }
    if (tightened) {
      publish();
      persistTightening();
    }
    return;
  }
  if (getPolicyState().source === 'server') return;
  setPolicy({ require_device_lock: requireLock }, 'discovery');
}

const Persisted = z.object({ v: z.literal(1), origin: z.string(), policy: RawPolicy });

/** Launch: the last server policy of this workspace, before the lock decision. */
export async function restorePolicy(origin: string): Promise<void> {
  let raw: string | null;
  try {
    raw = await getSecureItem(SECURE_KEYS.policy);
  } catch {
    return;
  }
  if (raw === null) return;
  try {
    const parsed = Persisted.parse(JSON.parse(raw));
    if (parsed.origin !== origin) return;
    await applyServerPolicy(parsed.policy, { origin, persist: false });
  } catch {
    log.info('persisted client policy ignored');
  }
}

onTokenGrant((response) => {
  void applyServerPolicy(response.client_policy);
  // The public policy may be stricter than this grant's (DEF-R1): re-read it.
  void refreshWorkspaceInfo();
});

/** At most one minimum-version re-check per this interval, however many requests succeed. */
export const UPGRADE_RECHECK_INTERVAL_MS = 10_000;
let lastUpgradeRecheck = Number.NEGATIVE_INFINITY;

/**
 * A signed-in request succeeded while the update screen is up (DEF-R4): re-read
 * the discovery document, whose minimum version clears the screen when it no
 * longer applies. Throttled.
 */
export function recheckMinimumVersion(now: number = Date.now()): void {
  if (now - lastUpgradeRecheck < UPGRADE_RECHECK_INTERVAL_MS) return;
  lastUpgradeRecheck = now;
  void refreshWorkspaceInfo();
}

setUpgradeRecheck(() => recheckMinimumVersion());

onDiscovery((origin, document) => {
  if (getSession().serverOrigin === origin) applyDiscoveryPolicy(document);
});

/** Forgets the server policy (the policy itself is reset by policy.ts's own wipe hook). */
export function resetPolicySources(): void {
  lastUpgradeRecheck = Number.NEGATIVE_INFINITY;
  serverPolicy = null;
  serverRaw = null;
  discoveryLock = false;
  discoveryOfflineOff = false;
}

registerWipeHook('policy_sources', resetPolicySources);
