import {
  DEVICE_REVOKED_BODY,
  FIXTURE_ORIGIN,
  TOKEN_REJECTED_BODY,
  opaqueToken,
  tokenResponse,
  upgradeRequiredBody,
} from '@/api/__fixtures__/nativeClient';
import { createGuardedFetch, type BaseFetch } from '@/api/middleware';
import { ResumableStream } from '@/api/sse/stream';
import {
  __resetNativeSessionForTests,
  __setNativeSessionDepsForTests,
  startNativeSession,
} from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { endSession } from '@/security/endSession';
import { __resetSessionForTests, getBearerToken, getSession, setCredential, setSession } from '@/state/session';

import { bindWorkspace } from '@/security/secureStorage';
import { TEST_WORKSPACE } from '@/test/fakeSecureStore';
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

/**
 * The request layer with a NATIVE credential (ADR-0025): proactive refresh,
 * one refresh + one replay on a 401, `device_revoked` wipes, 426 blocks.
 */

const BASE = {
  origin: FIXTURE_ORIGIN,
  clientId: 'ca.zefir.agent',
  tokenEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/token`,
  revocationEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`,
};
const ACCESS_1 = opaqueToken('elnat_', 1);
const ACCESS_2 = opaqueToken('elnat_', 2);

let clock = 5_000_000;
let refresh: jest.Mock<Promise<TokenOutcome>, unknown[]>;

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

/** Answers 401 TOKEN_REJECTED for ACCESS_1 and 200 for anything else. */
function api(respond?: (token: string | null) => Response) {
  const tokens: (string | null)[] = [];
  const fetch: jest.MockedFunction<BaseFetch> = jest.fn(async (_input: Request, init?: RequestInit) => {
    const auth = new Headers(init?.headers).get('Authorization');
    const token = auth ? auth.replace(/^Bearer /, '') : null;
    tokens.push(token);
    if (respond) return respond(token);
    return token === ACCESS_1 ? json(401, TOKEN_REJECTED_BODY) : json(200, { ok: true });
  });
  return { fetch, tokens };
}

beforeEach(async () => {
  bindWorkspace(TEST_WORKSPACE);
  clock = 5_000_000;
  __resetSessionForTests();
  __resetNativeSessionForTests();
  refresh = jest.fn(async (): Promise<TokenOutcome> => ({
    kind: 'ok',
    response: TokenResponseSchema.parse(tokenResponse(2)),
    receivedAt: clock,
  }));
  __setNativeSessionDepsForTests({
    now: () => clock,
    sleep: async (ms) => {
      clock += ms;
    },
    refresh,
    revoke: jest.fn(async () => true),
    clientVersion: () => '0.1.0',
  });
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN, workspaceId: TEST_WORKSPACE, projectId: 1 });
  await startNativeSession(TokenResponseSchema.parse(tokenResponse(1)), clock, BASE);
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

const get = (fetch: BaseFetch) =>
  createGuardedFetch({ origin: FIXTURE_ORIGIN, auth: { kind: 'session' }, fetch })(
    new Request(`${FIXTURE_ORIGIN}/api/v2/projects/project/default/1`),
  );

it('sends the native access token as a Bearer', async () => {
  const { fetch, tokens } = api(() => json(200, []));
  await get(fetch);
  expect(tokens).toEqual([ACCESS_1]);
  expect(refresh).not.toHaveBeenCalled();
});

it('never sends another workspace\'s token: a switch during the refresh fails the request unsent', async () => {
  clock += 899_000;
  let release!: () => void;
  refresh.mockImplementationOnce(async () => {
    await new Promise<void>((resolve) => (release = resolve));
    return { kind: 'ok', response: TokenResponseSchema.parse(tokenResponse(2)), receivedAt: clock };
  });
  const { fetch, tokens } = api();
  const pending = get(fetch);
  await new Promise((r) => setTimeout(r, 0));
  // The user switched to another connected workspace on the same host meanwhile.
  setSession({ workspaceId: 'bbbbbbbbbbbb' });
  setCredential({ kind: 'oauth', token: 'token-of-the-other-workspace' });
  release();
  await expect(pending).rejects.toMatchObject({ kind: 'origin_mismatch' });
  expect(tokens).toEqual([]);
});

it('refreshes before sending when the access token is about to expire', async () => {
  clock += 899_000;
  const { fetch, tokens } = api();
  expect((await get(fetch)).status).toBe(200);
  expect(tokens).toEqual([ACCESS_2]);
  expect(refresh).toHaveBeenCalledTimes(1);
});

it('on a 401 for an expired token: refreshes once and replays the request once', async () => {
  const { fetch, tokens } = api();
  const response = await get(fetch);
  expect(response.status).toBe(200);
  expect(tokens).toEqual([ACCESS_1, ACCESS_2]);
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(getSession().credential).toEqual({ kind: 'oauth' });
});

it('a 401 storm produces one refresh, and every request is replayed', async () => {
  const { fetch } = api();
  const responses = await Promise.all(Array.from({ length: 8 }, () => get(fetch)));
  expect(responses.map((r) => r.status)).toEqual(Array(8).fill(200));
  expect(refresh).toHaveBeenCalledTimes(1);
});

it('does not replay twice when the new token is refused too', async () => {
  const { fetch, tokens } = api(() => json(401, TOKEN_REJECTED_BODY));
  refresh.mockImplementation(async () => ({ kind: 'invalid_grant' }));
  await expect(get(fetch)).rejects.toMatchObject({ kind: 'session_invalid', reason: 'credential_rejected' });
  expect(tokens).toEqual([ACCESS_1]);
  await endSession({ reason: 'credential_rejected' });
  expect(getSession().endReason).toBe('credential_rejected');
});

it('device_revoked on the API wipes without trying to refresh', async () => {
  const { fetch } = api(() => json(401, DEVICE_REVOKED_BODY));
  await expect(get(fetch)).rejects.toMatchObject({ kind: 'session_invalid', reason: 'device_revoked' });
  expect(refresh).not.toHaveBeenCalled();
  await endSession({ reason: 'device_revoked' }); // joins the running wipe
  expect(getSession()).toMatchObject({ credential: null, serverOrigin: null, endReason: 'device_revoked' });
  expect(getBearerToken()).toBeNull();
});

it('device_revoked from the token endpoint during a refresh wipes too', async () => {
  refresh.mockImplementation(async () => ({ kind: 'device_revoked' }));
  const { fetch } = api();
  await expect(get(fetch)).rejects.toMatchObject({ reason: 'device_revoked' });
  await endSession({ reason: 'device_revoked' });
  expect(getSession().endReason).toBe('device_revoked');
});

it('an unreachable token endpoint is a network error and keeps the session', async () => {
  refresh.mockImplementation(async () => ({ kind: 'network' }));
  const { fetch } = api();
  await expect(get(fetch)).rejects.toMatchObject({ kind: 'network' });
  expect(getSession().credential).toEqual({ kind: 'oauth' });
  expect(getSession().ending).toBe(false);
});

it('426 for a native caller raises the update screen and wipes nothing', async () => {
  const { fetch } = api(() => json(426, upgradeRequiredBody('0.3.0')));
  await expect(get(fetch)).rejects.toMatchObject({ kind: 'upgrade_required' });
  expect(getSession().upgradeRequired).toEqual({ minVersion: '0.3.0' });
  expect(getSession().credential).toEqual({ kind: 'oauth' });
});

describe('event stream', () => {
  it('reconnects with the refreshed token after a 401', async () => {
    const seen: string[] = [];
    const transport = jest.fn(async (_url: string, init: { headers: Record<string, string> }) => {
      const token = init.headers.Authorization.replace(/^Bearer /, '');
      seen.push(token);
      if (token === ACCESS_1) {
        return {
          status: 401,
          headers: { get: () => null },
          body: (async function* () {
            yield new TextEncoder().encode(JSON.stringify(TOKEN_REJECTED_BODY));
          })(),
        };
      }
      return {
        status: 200,
        headers: { get: () => null },
        body: (async function* () {
          yield new TextEncoder().encode('id: 1\nevent: execution.node_event\ndata: {}\n\n');
        })(),
      };
    });
    const events: string[] = [];
    const stream = new ResumableStream({
      url: `${FIXTURE_ORIGIN}/api/v2/executions/1/e/events`,
      transport,
      appState: null,
      onEvent: (event) => {
        events.push(event.id ?? '');
        return 'stop';
      },
    });
    await stream.start();
    expect(seen).toEqual([ACCESS_1, ACCESS_2]);
    expect(events).toEqual(['1']);
    expect(refresh).toHaveBeenCalledTimes(1);
  });
});
