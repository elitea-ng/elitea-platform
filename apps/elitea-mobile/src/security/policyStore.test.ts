import {
  FIXTURE_ORIGIN,
  clientPolicy,
  discoveryDocument,
  discoveryWithoutNativeAuth,
  tokenResponse,
} from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import {
  __resetNativeSessionForTests,
  __setNativeSessionDepsForTests,
  refreshNativeSession,
  startNativeSession,
} from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { endSession } from '@/security/endSession';
import { STRICT_DEFAULTS, getPolicy, getPolicyState, resetPolicy, subscribePolicy } from '@/security/policy';
import { LockController } from '@/security/lock/lockController';
import {
  applyDiscoveryPolicy,
  applyServerPolicy,
  policyFrom,
  recheckMinimumVersion,
  resetPolicySources,
  UPGRADE_RECHECK_INTERVAL_MS,
  restorePolicy,
} from '@/security/policyStore';
import { SECURE_KEYS, getSecureItemIn, bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, getSession, setSession } from '@/state/session';
import * as workspaceInfo from '@/state/workspaceInfo';
import { __resetWorkspaceInfoForTests, acceptDiscovery } from '@/state/workspaceInfo';

import { TEST_WORKSPACE } from '@/test/fakeSecureStore';
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const BASE = {
  origin: FIXTURE_ORIGIN,
  clientId: 'ca.zefir.agent',
  tokenEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/token`,
  revocationEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`,
};
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
const discovery = (doc: unknown) => DiscoveryDocumentSchema.parse(doc);

beforeEach(() => {
  bindWorkspace(TEST_WORKSPACE);
  __resetSessionForTests();
  __resetNativeSessionForTests();
  __resetWorkspaceInfoForTests();
  resetPolicy();
  resetPolicySources();
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('policyFrom', () => {
  it('reads the server policy fields', () => {
    expect(policyFrom(clientPolicy())).toEqual({
      require_device_lock: true,
      idle_lock_seconds: 120,
      allow_screenshots: false,
      offline_retention_days: 0,
      offline_max_mb: 512,
      offline_attachments: false,
    });
  });

  it('drops wrong types and out-of-range values', () => {
    expect(
      policyFrom({
        require_device_lock: 'yes',
        idle_lock_seconds: 86_401,
        allow_screenshots: 1,
        offline_retention_days: -1,
        offline_max_mb: 1.5,
      }),
    ).toEqual({});
  });
});

describe('precedence', () => {
  it('uses strict defaults with no workspace policy', () => {
    expect(getPolicyState()).toEqual({ policy: STRICT_DEFAULTS, source: 'default' });
  });

  it('applies the discovery subset before sign-in, strict otherwise', () => {
    applyDiscoveryPolicy(discovery(discoveryWithoutNativeAuth()), '0.1.0');
    expect(getPolicyState()).toEqual({
      policy: { ...STRICT_DEFAULTS, require_device_lock: false },
      source: 'discovery',
    });
  });

  it('a token response overrides discovery', async () => {
    applyDiscoveryPolicy(discovery(discoveryDocument()), '0.1.0');
    await applyServerPolicy(clientPolicy({ require_device_lock: false, allow_screenshots: true, idle_lock_seconds: 0 }));
    expect(getPolicyState()).toMatchObject({
      source: 'server',
      policy: { require_device_lock: false, allow_screenshots: true, idle_lock_seconds: 0 },
    });
  });

  it('a later discovery document cannot loosen a server policy, but can tighten the lock', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false, allow_screenshots: true }));
    applyDiscoveryPolicy(discovery(discoveryWithoutNativeAuth()), '0.1.0'); // says no lock
    expect(getPolicyState().policy.require_device_lock).toBe(false);
    expect(getPolicyState().source).toBe('server');
    applyDiscoveryPolicy(discovery(discoveryDocument()), '0.1.0'); // says lock required
    expect(getPolicyState()).toMatchObject({
      source: 'server',
      policy: { require_device_lock: true, allow_screenshots: true },
    });
  });

  it('a discovery document saying offline_enabled: false turns the offline store off', async () => {
    await applyServerPolicy(clientPolicy({ offline_retention_days: 30 }));
    applyDiscoveryPolicy(discovery(discoveryWithoutNativeAuth()), '0.1.0'); // offline_enabled: true
    expect(getPolicyState().policy.offline_retention_days).toBe(30);
    applyDiscoveryPolicy(discovery(discoveryDocument()), '0.1.0'); // offline_enabled: false
    expect(getPolicyState()).toMatchObject({ source: 'server', policy: { offline_retention_days: 0 } });
    // A later discovery document does not turn it back on; the next token response decides.
    applyDiscoveryPolicy(discovery(discoveryWithoutNativeAuth()), '0.1.0');
    expect(getPolicyState().policy.offline_retention_days).toBe(0);
    await applyServerPolicy(clientPolicy({ offline_retention_days: 30 }));
    expect(getPolicyState().policy.offline_retention_days).toBe(30);
  });

  it('keeps the last policy when a token response carries none', async () => {
    await applyServerPolicy(clientPolicy({ allow_screenshots: true }));
    await applyServerPolicy(undefined);
    expect(getPolicyState().policy.allow_screenshots).toBe(true);
  });
});

describe('wiring', () => {
  it('every token response (sign-in and refresh) feeds the policy', async () => {
    __setNativeSessionDepsForTests({
      refresh: jest.fn(
        async (): Promise<TokenOutcome> => ({
          kind: 'ok',
          response: TokenResponseSchema.parse(tokenResponse(2, { client_policy: clientPolicy({ idle_lock_seconds: 30 }) })),
          receivedAt: Date.now(),
        }),
      ),
      revoke: jest.fn(async () => true),
    });
    await startNativeSession(TokenResponseSchema.parse(tokenResponse(1)), Date.now(), BASE);
    await flush();
    expect(getPolicyState()).toMatchObject({ source: 'server', policy: { idle_lock_seconds: 120 } });
    await refreshNativeSession();
    await flush();
    expect(getPolicyState().policy.idle_lock_seconds).toBe(30);
  });

  it('discovery for the connected workspace feeds the policy', () => {
    acceptDiscovery(FIXTURE_ORIGIN, discovery(discoveryWithoutNativeAuth()));
    expect(getPolicyState().source).toBe('discovery');
  });
});

// DEF-R1 (Agent Zefir E2E run 2): the workspace turned require_device_lock on
// while the app was signed in with a policy that did not require it. Two cold
// relaunches opened straight into the list; the lock appeared only after the
// 15-minute access token had refreshed AND the app was relaunched.
describe('a tightened lock policy applies promptly (DEF-R1)', () => {
  function guardedApp() {
    const controller = new LockController({
      deviceAuth: { security: jest.fn(async () => 'biometric' as const), authenticate: jest.fn(() => new Promise(() => {})) },
      getPolicy,
      appState: { addEventListener: () => ({ remove: () => {} }) },
    });
    const unsubscribe = subscribePolicy(() => controller.policyChanged());
    return { controller, unsubscribe };
  }

  it('locks the running app as soon as discovery says a lock is required', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false }));
    const app = guardedApp();
    await app.controller.start('launch');
    expect(app.controller.getState().status).toBe('unlocked');

    acceptDiscovery(FIXTURE_ORIGIN, discovery(discoveryDocument())); // require_device_lock: true
    expect(app.controller.getState().status).toBe('locked');
    await flush();
    expect(app.controller.getState().authInFlight).toBe(true);
    app.unsubscribe();
  });

  it('locks the running app when a token response turns the lock on', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false }));
    const app = guardedApp();
    await app.controller.start('launch');
    await applyServerPolicy(clientPolicy({ require_device_lock: true }));
    expect(app.controller.getState().status).toBe('locked');
    app.unsubscribe();
  });

  it('persists a discovery tightening, so the next launch locks before any network call', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false, idle_lock_seconds: 45 }));
    applyDiscoveryPolicy(discovery(discoveryDocument()), '0.1.0');
    await flush();
    resetPolicy();
    resetPolicySources();
    await restorePolicy(FIXTURE_ORIGIN);
    expect(getPolicyState()).toMatchObject({ source: 'server', policy: { require_device_lock: true, idle_lock_seconds: 45 } });
  });

  it('re-reads discovery after every token response', async () => {
    const refresh = jest
      .spyOn(workspaceInfo, 'refreshWorkspaceInfo')
      .mockResolvedValue({ ok: false, error: 'invalid' } as Awaited<ReturnType<typeof workspaceInfo.refreshWorkspaceInfo>>);
    __setNativeSessionDepsForTests({
      refresh: jest.fn(
        async (): Promise<TokenOutcome> => ({
          kind: 'ok',
          response: TokenResponseSchema.parse(tokenResponse(2)),
          receivedAt: Date.now(),
        }),
      ),
      revoke: jest.fn(async () => true),
    });
    await startNativeSession(TokenResponseSchema.parse(tokenResponse(1)), Date.now(), BASE);
    await refreshNativeSession();
    await flush();
    expect(refresh).toHaveBeenCalledTimes(2);
  });
});

describe('minimum client version', () => {
  it('shows the update screen when a token response says the app is too old', async () => {
    await applyServerPolicy(clientPolicy({ min_client_version: '0.2.0' }), { clientVersion: '0.1.0' });
    expect(getSession().upgradeRequired).toEqual({ minVersion: '0.2.0' });
  });

  it('shows the update screen from discovery (pre-check before any 426)', () => {
    applyDiscoveryPolicy(discovery(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '1.0.0' } })), '0.1.0');
    expect(getSession().upgradeRequired).toEqual({ minVersion: '1.0.0' });
  });

  // DEF-R4 (Agent Zefir E2E run 2): min_client_version went 9.0.0 → '' and the
  // update screen stayed until a relaunch.
  it('clears the update screen when discovery lowers or removes the minimum', () => {
    applyDiscoveryPolicy(discovery(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '9.0.0' } })), '0.1.0');
    expect(getSession().upgradeRequired).toEqual({ minVersion: '9.0.0' });
    applyDiscoveryPolicy(discovery(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '0.1.0' } })), '0.1.0');
    expect(getSession().upgradeRequired).toBeNull();
    setSession({ upgradeRequired: {} }); // a 426 that named no version
    applyDiscoveryPolicy(discovery(discoveryDocument()), '0.1.0'); // no minimum
    expect(getSession().upgradeRequired).toBeNull();
  });

  it('clears the update screen when a token response accepts this version', async () => {
    setSession({ upgradeRequired: { minVersion: '9.0.0' } });
    await applyServerPolicy(clientPolicy({ min_client_version: '' }), { clientVersion: '0.1.0' });
    expect(getSession().upgradeRequired).toBeNull();
  });

  it('keeps the update screen while discovery still names a higher minimum', () => {
    setSession({ upgradeRequired: { minVersion: '9.0.0' } });
    applyDiscoveryPolicy(discovery(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '9.0.0' } })), '0.1.0');
    expect(getSession().upgradeRequired).toEqual({ minVersion: '9.0.0' });
  });

  it('re-reads discovery for a successful request at most once per interval', () => {
    const refresh = jest
      .spyOn(workspaceInfo, 'refreshWorkspaceInfo')
      .mockResolvedValue({ ok: false, error: 'invalid' } as Awaited<ReturnType<typeof workspaceInfo.refreshWorkspaceInfo>>);
    recheckMinimumVersion(1_000);
    recheckMinimumVersion(1_000 + UPGRADE_RECHECK_INTERVAL_MS - 1);
    expect(refresh).toHaveBeenCalledTimes(1);
    recheckMinimumVersion(1_000 + UPGRADE_RECHECK_INTERVAL_MS);
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('lets a current app through', async () => {
    await applyServerPolicy(clientPolicy({ min_client_version: '0.1.0' }), { clientVersion: '0.1.0' });
    expect(getSession().upgradeRequired).toBeNull();
  });
});

describe('persistence', () => {
  it('persists the server policy per workspace and restores it at launch', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false, idle_lock_seconds: 45 }));
    expect(JSON.parse((await getSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.policy))!)).toMatchObject({ v: 1, origin: FIXTURE_ORIGIN });
    resetPolicy();
    resetPolicySources();
    await restorePolicy(FIXTURE_ORIGIN);
    expect(getPolicyState()).toMatchObject({ source: 'server', policy: { require_device_lock: false, idle_lock_seconds: 45 } });
  });

  it('ignores a persisted policy of another workspace', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false }));
    resetPolicy();
    resetPolicySources();
    await restorePolicy('https://other.example.com');
    expect(getPolicyState().source).toBe('default');
  });

  it('is wiped with the session, back to strict defaults', async () => {
    await applyServerPolicy(clientPolicy({ require_device_lock: false, allow_screenshots: true }));
    await endSession({ reason: 'user_logout' });
    expect(getPolicyState()).toEqual({ policy: STRICT_DEFAULTS, source: 'default' });
    expect(await getSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.policy)).toBeNull();
    // A discovery document after the wipe starts from scratch (no stale server policy).
    applyDiscoveryPolicy(discovery(discoveryWithoutNativeAuth()), '0.1.0');
    expect(getPolicyState().source).toBe('discovery');
  });
});
