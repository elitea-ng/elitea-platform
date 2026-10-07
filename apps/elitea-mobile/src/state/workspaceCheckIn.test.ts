import { opaqueToken, tokenResponse } from '@/api/__fixtures__/nativeClient';
import {
  __resetNativeSessionForTests,
  __setNativeSessionDepsForTests,
  checkInStoredSession,
} from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace } from '@/security/secureStorage';
import { __resetRegistryForTests } from '@/security/workspaceRegistry';
import { __resetSessionForTests, setSession } from '@/state/session';
import { __resetWorkspaceTransitionsForTests, wipeRevokedWorkspace } from '@/state/workspace';
import {
  CHECK_IN_INTERVAL_MS,
  __resetWorkspaceCheckInsForTests,
  checkInPutAsideWorkspaces,
  type CheckInDeps,
} from '@/state/workspaceCheckIn';
import { registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/db/offlineStore', () => ({ offlineStore: { wipe: jest.fn(async () => undefined) } }));
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const A = 'aaaaaaaaaaaa';
const B = 'bbbbbbbbbbbb';
const ORIGIN_A = 'https://a.example.com';
const ORIGIN_B = 'https://b.example.com';
let clock = 1_000_000;

function nativeSlot(id: string, origin: string) {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, id), origin);
  fake.values.set(wsKey(SECURE_KEYS.credentialKind, id), 'oauth');
  fake.values.set(
    wsKey(SECURE_KEYS.refreshToken, id),
    JSON.stringify({
      v: 1,
      origin,
      clientId: 'ca.zefir.agent',
      tokenEndpoint: `${origin}/api/v2/auth/native/token`,
      revocationEndpoint: `${origin}/api/v2/auth/native/revoke`,
      refreshToken: opaqueToken('elnrt_', 1),
      refreshExpiresAt: 0,
      deviceId: 'd',
    }),
  );
}

/** A is active; B is put aside, signed in natively. */
function twoWorkspaces() {
  nativeSlot(A, ORIGIN_A);
  nativeSlot(B, ORIGIN_B);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A, B], A));
  bindWorkspace(A);
  setSession({ hydrated: true, serverOrigin: ORIGIN_A, workspaceId: A });
}

function deps(overrides: Partial<CheckInDeps> = {}): CheckInDeps & { wipe: jest.Mock; wipeOfflineStore: jest.Mock } {
  return {
    now: () => clock,
    checkIn: checkInStoredSession,
    wipe: jest.fn(async () => true),
    wipeOfflineStore: jest.fn(async () => undefined),
    ...overrides,
  } as CheckInDeps & { wipe: jest.Mock; wipeOfflineStore: jest.Mock };
}

function tokenEndpoint(outcome: TokenOutcome) {
  const refresh = jest.fn(async (_origin: string, _endpoint: string, _request: unknown) => outcome);
  __setNativeSessionDepsForTests({ now: () => clock, sleep: async () => {}, refresh, clientVersion: () => '0.1.0' });
  return refresh;
}

const ok = (policy: Record<string, unknown>): TokenOutcome => ({
  kind: 'ok',
  response: TokenResponseSchema.parse({ ...tokenResponse(2), client_policy: policy }),
  receivedAt: clock,
});

beforeEach(() => {
  clock = 1_000_000;
  __resetSessionForTests();
  __resetRegistryForTests();
  __resetWorkspaceTransitionsForTests();
  __resetNativeSessionForTests();
  __resetWorkspaceCheckInsForTests();
  bindWorkspace(null);
  fake.values.clear();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

// WS-SEC-1: a workspace that is only put aside must still hear its server.
describe('checkInPutAsideWorkspaces', () => {
  it('wipes a workspace put aside whose server revoked this device', async () => {
    twoWorkspaces();
    const refresh = tokenEndpoint({ kind: 'device_revoked' });
    const d = deps();
    await checkInPutAsideWorkspaces(d);
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(refresh).toHaveBeenCalledWith(ORIGIN_B, `${ORIGIN_B}/api/v2/auth/native/token`, expect.anything());
    expect(d.wipe).toHaveBeenCalledWith(B, 'device_revoked');
    expect(d.wipe).not.toHaveBeenCalledWith(A, expect.anything());
  });

  it('a revoked workspace put aside leaves nothing stored; the active one keeps everything', async () => {
    twoWorkspaces();
    tokenEndpoint({ kind: 'device_revoked' });
    const beforeA = [...fake.values].filter(([k]) => k.startsWith(`az.ws.${A}.`));
    await checkInPutAsideWorkspaces(deps({ wipe: wipeRevokedWorkspace }));
    expect([...fake.values.keys()].filter((k) => k.startsWith(`az.ws.${B}.`))).toEqual([]);
    expect([...fake.values].filter(([k]) => k.startsWith(`az.ws.${A}.`))).toEqual(beforeA);
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!)).toEqual({ v: 1, active: A, ids: [A] });
  });

  it('stores the rotated pair and the new policy in the slot, and deletes the database when offline storage is off', async () => {
    twoWorkspaces();
    tokenEndpoint(ok({ require_device_lock: true, offline_retention_days: 0 }));
    const d = deps();
    await checkInPutAsideWorkspaces(d);
    expect(JSON.parse(fake.values.get(wsKey(SECURE_KEYS.refreshToken, B))!).refreshToken).toBe(opaqueToken('elnrt_', 2));
    expect(JSON.parse(fake.values.get(wsKey(SECURE_KEYS.policy, B))!)).toEqual({
      v: 1,
      origin: ORIGIN_B,
      policy: { require_device_lock: true, offline_retention_days: 0 },
    });
    expect(d.wipeOfflineStore).toHaveBeenCalledWith(B);
    expect(d.wipe).not.toHaveBeenCalled();
  });

  it('keeps the database while the policy still allows offline storage', async () => {
    twoWorkspaces();
    tokenEndpoint(ok({ offline_retention_days: 30 }));
    const d = deps();
    await checkInPutAsideWorkspaces(d);
    expect(d.wipeOfflineStore).not.toHaveBeenCalled();
  });

  it('never checks in the active workspace, and checks in a workspace at most once per interval', async () => {
    twoWorkspaces();
    const refresh = tokenEndpoint({ kind: 'network' });
    const d = deps();
    await checkInPutAsideWorkspaces(d);
    await checkInPutAsideWorkspaces(d);
    expect(refresh.mock.calls.every((call) => call[0] === ORIGIN_B)).toBe(true);
    const first = refresh.mock.calls.length;
    clock += CHECK_IN_INTERVAL_MS;
    await checkInPutAsideWorkspaces(d);
    expect(refresh.mock.calls.length).toBeGreaterThan(first);
    expect(d.wipe).not.toHaveBeenCalled();
  });

  it('leaves a slot with a wipe under way to that wipe', async () => {
    twoWorkspaces();
    fake.values.set(wsKey(SECURE_KEYS.wipePending, B), JSON.stringify({ reason: 'user_logout' }));
    const refresh = tokenEndpoint({ kind: 'device_revoked' });
    await checkInPutAsideWorkspaces(deps());
    expect(refresh).not.toHaveBeenCalled();
  });
});
