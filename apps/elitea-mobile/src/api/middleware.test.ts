import { createApiClient } from '@/api/client';
import { CLIENT_VERSION_HEADER, getClientVersion } from '@/api/clientVersion';
import { ApiError, unwrap } from '@/api/errors';
import { classifySessionFailure, createGuardedFetch, setUpgradeRecheck, type BaseFetch } from '@/api/middleware';
import { abortAll } from '@/lib/abortRegistry';
import { registerWipeHook } from '@/security/endSession';
import {
  __resetSessionForTests,
  getBearerToken,
  getSession,
  setCredential,
  setSession,
} from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fakeStore = jest.requireMock<import('@/test/fakeSecureStore').FakeSecureStore>('expo-secure-store');

const ORIGIN = 'https://ai.example.com';

type Seen = { url: string; method: string; headers: Headers };

function jsonResponse(status: number, body: unknown, init: { url?: string; headers?: Record<string, string> } = {}) {
  const response = new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...init.headers },
  });
  if (init.url) Object.defineProperty(response, 'url', { value: init.url });
  return response;
}

function recordingFetch(respond: (seen: Seen) => Response | Promise<Response>) {
  const seen: Seen[] = [];
  const fetch: jest.MockedFunction<BaseFetch> = jest.fn(async (input: Request, init?: RequestInit) => {
    const entry = { url: input.url, method: input.method, headers: new Headers(init?.headers) };
    seen.push(entry);
    return respond(entry);
  });
  return { fetch, seen };
}

function signIn(token = 'pat-secret') {
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 1 });
  setCredential({ kind: 'pat', token });
}

const listProjects = (client: ReturnType<typeof createApiClient>) =>
  client.GET('/projects/project/default/{public_project_id}', {
    params: { path: { public_project_id: 1 } },
  });

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('guarded fetch: headers and origin guard', () => {
  it('attaches Bearer, X-Client-Version and Accept for the workspace origin', async () => {
    signIn();
    const { fetch, seen } = recordingFetch(() => jsonResponse(200, []));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });

    await expect(unwrap(listProjects(client))).resolves.toEqual([]);

    expect(seen[0].url).toBe(`${ORIGIN}/api/v2/projects/project/default/1`);
    expect(seen[0].headers.get('Authorization')).toBe('Bearer pat-secret');
    expect(seen[0].headers.get(CLIENT_VERSION_HEADER)).toBe(getClientVersion());
    expect(seen[0].headers.get('Accept')).toBe('application/json');
  });

  it('never sends the token to another origin', async () => {
    signIn();
    const { fetch } = recordingFetch(() => jsonResponse(200, []));
    const client = createApiClient({ origin: 'https://evil.example', auth: { kind: 'session' }, fetch });

    await expect(listProjects(client)).rejects.toMatchObject({ kind: 'origin_mismatch' });
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    for (const url of ['https://evil.example/api/v2/x', 'http://ai.example.com/api/v2/x', 'https://ai.example.com.evil.example/api/v2/x']) {
      await expect(guarded(new Request(url))).rejects.toMatchObject({ kind: 'origin_mismatch' });
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  it('never attaches a token for an anonymous client, but still sends the version', async () => {
    signIn();
    const { fetch, seen } = recordingFetch(() => jsonResponse(200, []));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'anonymous' }, fetch });
    await listProjects(client);
    expect(seen[0].headers.has('Authorization')).toBe(false);
    expect(seen[0].headers.get(CLIENT_VERSION_HEADER)).toBe(getClientVersion());
  });

  it('asks a browser not to store any response (DEF-R2)', async () => {
    const { fetch } = recordingFetch(() => jsonResponse(200, {}));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'anonymous' }, fetch, platform: 'web' });
    await guarded(new Request(`${ORIGIN}/api/v2/auth/native/token`, { method: 'POST', body: 'x' }));
    expect(fetch.mock.calls[0][1]).toMatchObject({ cache: 'no-store' });
  });

  it('leaves the GET URL alone on native, where fetch would only append a cache-buster (DEF-R2)', async () => {
    const { fetch, seen } = recordingFetch(() => jsonResponse(200, {}));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'anonymous' }, fetch, platform: 'ios' });
    await guarded(new Request(`${ORIGIN}/api/v2/x`));
    expect(fetch.mock.calls[0][1]).not.toHaveProperty('cache');
    expect(seen[0].url).toBe(`${ORIGIN}/api/v2/x`);
  });

  it('strips a caller-supplied Authorization header from an anonymous request', async () => {
    const { fetch, seen } = recordingFetch(() => jsonResponse(200, {}));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'anonymous' }, fetch });
    await guarded(new Request(`${ORIGIN}/api/v2/x`, { headers: { Authorization: 'Bearer leaked' } }));
    expect(seen[0].headers.has('Authorization')).toBe(false);
  });

  it('refuses a session request when signed out, without a network call', async () => {
    setSession({ hydrated: true, serverOrigin: ORIGIN });
    const { fetch } = recordingFetch(() => jsonResponse(200, []));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(listProjects(client)).rejects.toMatchObject({ kind: 'session_invalid', reason: 'no_credential' });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('refuses new session requests while signing out', async () => {
    signIn();
    setSession({ ending: true });
    const { fetch } = recordingFetch(() => jsonResponse(200, []));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(listProjects(client)).rejects.toMatchObject({ kind: 'session_ending' });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('rejects an answer redirected to another origin without reading it', async () => {
    signIn();
    const { fetch } = recordingFetch(() => jsonResponse(200, [], { url: 'https://evil.example/landing' }));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(listProjects(client)).rejects.toMatchObject({ kind: 'origin_mismatch' });
  });
});

describe('guarded fetch: 401 and 426', () => {
  it('ends the session exactly once for parallel device_revoked answers', async () => {
    signIn();
    const wipes = jest.fn();
    const unregister = registerWipeHook('count', wipes);
    const { fetch } = recordingFetch(() => jsonResponse(401, { error: 'device_revoked' }));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });

    const results = await Promise.allSettled(Array.from({ length: 5 }, () => listProjects(client)));

    for (const result of results) {
      expect(result.status).toBe('rejected');
      expect((result as PromiseRejectedResult).reason).toBeInstanceOf(ApiError);
      expect((result as PromiseRejectedResult).reason.kind).toBe('session_invalid');
    }
    // Let the (fire-and-forget) wipe finish.
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(wipes).toHaveBeenCalledTimes(1);
    expect(getSession().endReason).toBe('device_revoked');
    expect(getBearerToken()).toBeNull();
    unregister();
  });

  it('device_revoked calls endSession("device_revoked")', async () => {
    signIn();
    const endSession = jest.fn(async () => ({ reason: 'device_revoked' as const, errors: [], storageWiped: true }));
    const { fetch } = recordingFetch(() => jsonResponse(401, { error: 'device_revoked' }));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch, endSession });
    await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({ reason: 'device_revoked' });
    expect(endSession).toHaveBeenCalledWith({ reason: 'device_revoked' });
  });

  it('a generic 401 with a PAT ends the session as credential_rejected', async () => {
    signIn();
    const endSession = jest.fn(async () => ({ reason: 'credential_rejected' as const, errors: [], storageWiped: true }));
    const { fetch } = recordingFetch(() => jsonResponse(401, { error: 'Unauthorized' }));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch, endSession });
    await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({
      kind: 'session_invalid',
      reason: 'credential_rejected',
      status: 401,
    });
    expect(endSession).toHaveBeenCalledTimes(1);
    expect(endSession).toHaveBeenCalledWith({ reason: 'credential_rejected' });
  });

  it('a 401 for a credential that was already replaced does not end the new session', async () => {
    signIn('old');
    const endSession = jest.fn();
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const { fetch } = recordingFetch(async () => {
      await gate;
      return jsonResponse(401, {});
    });
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch, endSession });
    const pending = guarded(new Request(`${ORIGIN}/api/v2/x`));
    setCredential({ kind: 'pat', token: 'new' });
    release();
    await expect(pending).rejects.toMatchObject({ reason: 'stale_credential' });
    expect(endSession).not.toHaveBeenCalled();
  });

  it('a 401 while validating an explicit token is returned to the caller, not a wipe', async () => {
    setSession({ hydrated: true, serverOrigin: ORIGIN });
    const endSession = jest.fn();
    const { fetch } = recordingFetch(() => jsonResponse(401, { error: 'Unauthorized' }));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'token', token: 'candidate' }, fetch, endSession });
    const response = await guarded(new Request(`${ORIGIN}/api/v2/x`));
    expect(response.status).toBe(401);
    expect(endSession).not.toHaveBeenCalled();
  });

  it('426 sets upgradeRequired and does not end the session', async () => {
    signIn();
    const endSession = jest.fn();
    const { fetch } = recordingFetch(() => jsonResponse(426, { error: 'upgrade_required', min_client_version: '1.2.0' }));
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch, endSession });
    await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({ kind: 'upgrade_required' });
    expect(getSession().upgradeRequired).toEqual({ minVersion: '1.2.0' });
    expect(endSession).not.toHaveBeenCalled();
    expect(getBearerToken()).toBe('pat-secret');
  });

  // WS-SEC-1: a device_revoked that answers a workspace being put aside must
  // mark THAT workspace for its wipe, not end whatever session is current.
  it('a device_revoked for a workspace that is no longer active marks its slot for a wipe and ends nothing', async () => {
    signIn('token-a');
    setSession({ workspaceId: 'bbbbbbbbbbbb' });
    const endSession = jest.fn();
    const error = await classifySessionFailure(
      { status: 401, json: async () => ({ error: 'device_revoked' }) },
      { token: 'token-a', sessionAuth: true, endSession, workspaceId: 'aaaaaaaaaaaa' },
    );
    expect(error).toMatchObject({ kind: 'session_invalid', reason: 'stale_credential' });
    expect(endSession).not.toHaveBeenCalled();
    expect(JSON.parse(fakeStore.values.get('az.ws.aaaaaaaaaaaa.wipe.pending')!)).toMatchObject({ reason: 'device_revoked' });
    expect(fakeStore.values.has('az.ws.bbbbbbbbbbbb.wipe.pending')).toBe(false);
  });

  // F4: a 426 answering the previous workspace's request is not the new one's.
  it("a 426 for a workspace that is no longer active does not put the new one into 'upgrade required'", async () => {
    signIn();
    setSession({ workspaceId: 'aaaaaaaaaaaa' });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const { fetch } = recordingFetch(async () => {
      await gate;
      return jsonResponse(426, { error: 'upgrade_required', min_client_version: '9.0.0' });
    });
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    const pending = guarded(new Request(`${ORIGIN}/api/v2/x`));
    await new Promise((resolve) => setTimeout(resolve, 0));
    // The user switched to workspace B (same host in this fixture) meanwhile.
    setSession({ workspaceId: 'bbbbbbbbbbbb' });
    release();
    await expect(pending).rejects.toMatchObject({ kind: 'upgrade_required' });
    expect(getSession().upgradeRequired).toBeNull();
  });

  // DEF-R4 (Agent Zefir E2E run 2): after the workspace lowered its minimum
  // again, the update screen stayed until a relaunch.
  it('a signed-in 2xx while the update screen is up re-checks the minimum version', async () => {
    signIn();
    const recheck = jest.fn();
    setUpgradeRecheck(recheck);
    try {
      const answers = [jsonResponse(426, { error: 'client_upgrade_required', min_client_version: '9.0.0' }), jsonResponse(200, {})];
      const { fetch } = recordingFetch(() => answers.shift()!);
      const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
      await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({ kind: 'upgrade_required' });
      expect(recheck).not.toHaveBeenCalled();
      await guarded(new Request(`${ORIGIN}/api/v2/x`));
      expect(recheck).toHaveBeenCalledTimes(1);

      // Not without the update screen, and never for an anonymous request.
      setSession({ upgradeRequired: null });
      const plain = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch: recordingFetch(() => jsonResponse(200, {})).fetch });
      await plain(new Request(`${ORIGIN}/api/v2/x`));
      setSession({ upgradeRequired: { minVersion: '9.0.0' } });
      const anon = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'anonymous' }, fetch: recordingFetch(() => jsonResponse(200, {})).fetch });
      await anon(new Request(`${ORIGIN}/.well-known/elitea-client`));
      expect(recheck).toHaveBeenCalledTimes(1);
    } finally {
      setUpgradeRecheck(null);
    }
  });
});

describe('guarded fetch: transport failures', () => {
  const hangingFetch: BaseFetch = (_input, init) =>
    new Promise((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new Error('aborted')));
    });

  it('times out', async () => {
    signIn();
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch: hangingFetch, timeoutMs: 5 });
    await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({ kind: 'timeout' });
  });

  it('reports a caller abort as aborted', async () => {
    signIn();
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch: hangingFetch });
    const controller = new AbortController();
    const pending = guarded(new Request(`${ORIGIN}/api/v2/x`, { signal: controller.signal }));
    controller.abort();
    await expect(pending).rejects.toMatchObject({ kind: 'aborted' });
  });

  it('is cancelled by abortAll (endSession)', async () => {
    signIn();
    const guarded = createGuardedFetch({ origin: ORIGIN, auth: { kind: 'session' }, fetch: hangingFetch });
    const pending = guarded(new Request(`${ORIGIN}/api/v2/x`));
    expect(abortAll()).toBe(1);
    await expect(pending).rejects.toMatchObject({ kind: 'aborted' });
  });

  it('reports a transport failure as network', async () => {
    signIn();
    const guarded = createGuardedFetch({
      origin: ORIGIN,
      auth: { kind: 'session' },
      fetch: async () => {
        throw new TypeError('Network request failed');
      },
    });
    await expect(guarded(new Request(`${ORIGIN}/api/v2/x`))).rejects.toMatchObject({ kind: 'network' });
  });

  it('unwrap maps other statuses to http errors with the server code and Retry-After', async () => {
    signIn();
    const { fetch } = recordingFetch(() => jsonResponse(503, { error: 'busy' }, { headers: { 'Retry-After': '2' } }));
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(unwrap(listProjects(client))).rejects.toMatchObject({
      kind: 'http',
      status: 503,
      code: 'busy',
      retryAfterMs: 2000,
    });
  });
});
