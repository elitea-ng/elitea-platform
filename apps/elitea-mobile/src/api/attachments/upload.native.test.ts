import { FIXTURE_ORIGIN, TOKEN_REJECTED_BODY, opaqueToken, tokenResponse } from '@/api/__fixtures__/nativeClient';
import { uploadAttachment } from '@/api/attachments/upload';
import { memoryDisk, readMultipart } from '@/api/attachments/uploadHarness';
import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { __resetNativeSessionForTests, __setNativeSessionDepsForTests, startNativeSession } from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: 'file:///cache' } }));

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const ACCESS_1 = opaqueToken('elnat_', 1);
const ACCESS_2 = opaqueToken('elnat_', 2);
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

let clock = 5_000_000;

beforeEach(async () => {
  __resetSessionForTests();
  __resetNativeSessionForTests();
  __setNativeSessionDepsForTests({
    now: () => clock,
    sleep: async (ms) => {
      clock += ms;
    },
    refresh: jest.fn(async (): Promise<TokenOutcome> => ({ kind: 'ok', response: TokenResponseSchema.parse(tokenResponse(2)), receivedAt: clock })),
    revoke: jest.fn(async () => true),
    clientVersion: () => '0.1.0',
  });
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN, projectId: 7 });
  await startNativeSession(TokenResponseSchema.parse(tokenResponse(1)), clock, {
    origin: FIXTURE_ORIGIN,
    clientId: 'ca.zefir.agent',
    tokenEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/token`,
    revocationEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`,
  });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

it('a 401 mid-upload refreshes the token and resends the same chunk, then goes on', async () => {
  const seen: { token: string | null; chunk: string; version: string | null }[] = [];
  let expired = false;
  const fetch: BaseFetch = async (input, init) => {
    const headers = new Headers(init?.headers);
    const token = headers.get('Authorization')?.replace(/^Bearer /, '') ?? null;
    const chunk = readMultipart(await input.clone().text()).fields.chunk_index;
    seen.push({ token, chunk, version: headers.get('X-Client-Version') });
    // The access token expires after the first chunk.
    if (chunk === '1' && token === ACCESS_1) {
      expired = true;
      return json(401, TOKEN_REJECTED_BODY);
    }
    if (chunk === '2') return json(201, [{ filepath: `/chat-attachments/${UUID}/big-0000.pdf`, file_size: 13 }]);
    return json(202, { status: 'chunk_received' });
  };
  const disk = memoryDisk({ 'file:///big.pdf': new TextEncoder().encode('AAAAABBBBBCCC') });
  const client = createApiClient({ origin: FIXTURE_ORIGIN, auth: { kind: 'session' }, fetch });
  const uploaded = await uploadAttachment(7, UUID, { uri: 'file:///big.pdf', uploadName: 'big-0000.pdf', size: 13, mimeType: 'application/pdf' }, 5, {
    client,
    ...disk.deps,
    newFileId: () => 'f1',
  });
  expect(expired).toBe(true);
  expect(uploaded.filepath).toBe(`/chat-attachments/${UUID}/big-0000.pdf`);
  expect(seen.map((s) => [s.chunk, s.token])).toEqual([
    ['0', ACCESS_1],
    ['1', ACCESS_1],
    ['1', ACCESS_2],
    ['2', ACCESS_2],
  ]);
  // Every request carried the client version and the bearer only in a header.
  expect(seen.every((s) => s.version === '0.1.0')).toBe(true);
});
