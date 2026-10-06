import type { ConfigContext } from 'expo/config';

import appConfig from '../../app.config';
import { devSignInEnabled, pickDefaultProject, signInWithPat } from './devSignIn';
import type { BaseFetch } from '@/api/middleware';
import { SECURE_KEYS, bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, getBearerToken, getSession, setSession } from '@/state/session';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

import { TEST_WORKSPACE, wsKey } from '@/test/fakeSecureStore';
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const store = jest.requireMock<FakeSecureStore>('expo-secure-store');
const ORIGIN = 'https://ai.example.com';

describe('developer sign-in gate', () => {
  it('is on exactly in development builds', () => {
    expect(devSignInEnabled(true)).toBe(true);
    expect(devSignInEnabled(false)).toBe(false);
  });

  it('has no configuration switch a release build could carry', () => {
    const extra = appConfig({ config: {} } as ConfigContext).extra ?? {};
    expect(Object.keys(extra).filter((key) => /dev.?sign.?in/i.test(key))).toEqual([]);
  });
});

describe('pickDefaultProject', () => {
  it('returns the first project that is not suspended', () => {
    expect(pickDefaultProject([{ id: 3, suspended: true }, { id: 5, suspended: false }, { id: 7 }])).toBe(5);
    expect(pickDefaultProject([{ id: 3, suspended: true }])).toBeNull();
    expect(pickDefaultProject([])).toBeNull();
  });
});

describe('signInWithPat', () => {
  const project = (id: number, suspended = false) => ({
    id,
    name: `p${id}`,
    owner_id: 1,
    plugins: [],
    groups: [],
    keycloak_groups: {},
    suspended,
    create_success: true,
  });

  function serverReturning(status: number, body: unknown) {
    const calls: { url: string; auth: string | null }[] = [];
    const fetch: BaseFetch = jest.fn(async (input: Request, init?: RequestInit) => {
      calls.push({ url: input.url, auth: new Headers(init?.headers).get('Authorization') });
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
    });
    return { fetch, calls };
  }

  beforeEach(() => {
  bindWorkspace(TEST_WORKSPACE);
    __resetSessionForTests();
    store.values.clear();
    jest.clearAllMocks();
    setSession({ hydrated: true, serverOrigin: ORIGIN });
    jest.spyOn(console, 'warn').mockImplementation(() => {});
  });

  afterEach(() => jest.restoreAllMocks());

  it('validates the token with listProjects, stores it device-only and signs in', async () => {
    const { fetch, calls } = serverReturning(200, [project(2, true), project(4)]);

    const result = await signInWithPat('  candidate-token \n', { fetch, enabled: true });

    expect(result).toEqual({ ok: true, projectId: 4 });
    expect(calls).toEqual([
      { url: `${ORIGIN}/api/v2/projects/project/default/1`, auth: 'Bearer candidate-token' },
    ]);
    expect(store.values.get(wsKey(SECURE_KEYS.pat))).toBe('candidate-token');
    expect(store.values.get(wsKey(SECURE_KEYS.credentialKind))).toBe('pat');
    for (const call of store.setItemAsync.mock.calls) {
      expect(call[2]).toEqual({ keychainAccessible: store.WHEN_UNLOCKED_THIS_DEVICE_ONLY });
    }
    expect(getSession().credential).toEqual({ kind: 'pat' });
    expect(getSession().projectId).toBe(4);
    expect(getBearerToken()).toBe('candidate-token');
  });

  it('reports a rejected token without storing it or ending the session', async () => {
    const { fetch } = serverReturning(401, { error: 'Unauthorized' });
    await expect(signInWithPat('bad', { fetch, enabled: true })).resolves.toEqual({ ok: false, error: 'rejected' });
    expect(store.setItemAsync).not.toHaveBeenCalled();
    expect(getSession().serverOrigin).toBe(ORIGIN);
    expect(getSession().credential).toBeNull();
  });

  it("reports an unreachable server as can't reach", async () => {
    const fetch: BaseFetch = async () => {
      throw new TypeError('Network request failed');
    };
    await expect(signInWithPat('tok', { fetch, enabled: true })).resolves.toEqual({ ok: false, error: 'unreachable' });
    expect(store.setItemAsync).not.toHaveBeenCalled();
  });

  it('refuses when there is no usable project', async () => {
    const { fetch } = serverReturning(200, [project(2, true)]);
    await expect(signInWithPat('tok', { fetch, enabled: true })).resolves.toEqual({ ok: false, error: 'no_project' });
    expect(getSession().credential).toBeNull();
  });

  it('does nothing when developer sign-in is disabled', async () => {
    const { fetch } = serverReturning(200, [project(1)]);
    await expect(signInWithPat('tok', { fetch, enabled: false })).resolves.toEqual({ ok: false, error: 'disabled' });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('rejects an empty token without a request', async () => {
    const { fetch } = serverReturning(200, [project(1)]);
    await expect(signInWithPat('   ', { fetch, enabled: true })).resolves.toEqual({ ok: false, error: 'empty' });
    expect(fetch).not.toHaveBeenCalled();
  });
});
