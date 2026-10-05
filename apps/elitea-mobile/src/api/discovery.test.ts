import {
  FIXTURE_ORIGIN,
  discoveryDocument,
  discoveryWithoutNativeAuth,
} from '@/api/__fixtures__/nativeClient';
import {
  DiscoveryDocumentSchema,
  __clearDiscoveryCacheForTests,
  assessDiscovery,
  checkDiscoveryOrigins,
  contractMajor,
  fetchDiscovery,
  getCachedDiscovery,
  minimumVersionFor,
} from '@/api/discovery';
import type { BaseFetch } from '@/api/middleware';
import { __resetSessionForTests, getSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

function respond(status: number, body?: unknown, headers: Record<string, string> = {}) {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

function fakeFetch(...responses: (Response | Error)[]) {
  const seen: { url: string; headers: Headers }[] = [];
  const fetch: jest.MockedFunction<BaseFetch> = jest.fn(async (input: Request, init?: RequestInit) => {
    seen.push({ url: input.url, headers: new Headers(init?.headers) });
    const next = responses.shift();
    if (next === undefined) throw new Error('unexpected request');
    if (next instanceof Error) throw next;
    return next;
  });
  return { fetch, seen };
}

beforeEach(() => {
  __clearDiscoveryCacheForTests();
  __resetSessionForTests();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('DiscoveryDocumentSchema', () => {
  it('accepts the server document with and without native auth', () => {
    expect(DiscoveryDocumentSchema.safeParse(discoveryDocument()).success).toBe(true);
    expect(DiscoveryDocumentSchema.safeParse(discoveryWithoutNativeAuth()).success).toBe(true);
  });

  it('tolerates additive keys (the contract is additive within a major)', () => {
    expect(DiscoveryDocumentSchema.safeParse(discoveryDocument({ new_feature: { x: 1 } })).success).toBe(true);
  });

  it.each([
    ['a missing key', (d: Record<string, unknown>) => delete d.client_policy],
    ['an unknown deployment kind', (d: Record<string, unknown>) => (d.deployment_kind = 'cloud')],
    ['a null min_client_version map', (d: Record<string, unknown>) => (d.min_client_version = null)],
    ['a non-string version', (d: Record<string, unknown>) => (d.min_client_version = { 'ca.zefir.agent': 2 })],
    ['native_auth without a token endpoint', (d: Record<string, unknown>) => delete (d.native_auth as Record<string, unknown>).token_endpoint],
  ])('rejects %s', (_label, mutate) => {
    const doc = discoveryDocument() as Record<string, unknown>;
    mutate(doc);
    expect(DiscoveryDocumentSchema.safeParse(doc).success).toBe(false);
  });
});

describe('checkDiscoveryOrigins', () => {
  const parse = (doc: unknown) => DiscoveryDocumentSchema.parse(doc);

  it('accepts a document whose URLs are all on the chosen origin', () => {
    expect(checkDiscoveryOrigins(FIXTURE_ORIGIN, parse(discoveryDocument()))).toEqual({ ok: true });
  });

  it('refuses a token endpoint on another host', () => {
    const doc = discoveryDocument();
    doc.native_auth!.token_endpoint = 'https://evil.example/api/v2/auth/native/token';
    expect(checkDiscoveryOrigins(FIXTURE_ORIGIN, parse(doc)).ok).toBe(false);
  });

  it('reports the canonical origin when the deployment is published elsewhere', () => {
    const canonical = 'https://www.example.com';
    const doc = discoveryDocument({
      native_auth: {
        issuer: canonical,
        authorization_endpoint: `${canonical}/api/v2/auth/native/authorize`,
        token_endpoint: `${canonical}/api/v2/auth/native/token`,
        revocation_endpoint: `${canonical}/api/v2/auth/native/revoke`,
        code_challenge_methods_supported: ['S256'],
      },
    });
    expect(checkDiscoveryOrigins(FIXTURE_ORIGIN, parse(doc))).toEqual({ ok: false, canonicalOrigin: canonical });
  });

  it('refuses a brand pack URL off the pack route', () => {
    const doc = discoveryDocument({ brand_pack_url: `${FIXTURE_ORIGIN}/somewhere/else.json` });
    expect(checkDiscoveryOrigins(FIXTURE_ORIGIN, parse(doc)).ok).toBe(false);
  });
});

describe('fetchDiscovery', () => {
  it('reads the root-mounted document anonymously and caches it with its ETag', async () => {
    const { fetch, seen } = fakeFetch(respond(200, discoveryDocument(), { ETag: '"abc"' }));
    const result = await fetchDiscovery(FIXTURE_ORIGIN, { fetch });
    expect(result.ok).toBe(true);
    expect(seen[0].url).toBe(`${FIXTURE_ORIGIN}/.well-known/elitea-client`);
    expect(seen[0].headers.get('Authorization')).toBeNull();
    expect(seen[0].headers.get('X-Client-Version')).toMatch(/^\d+\.\d+\.\d+$/);
    expect(getCachedDiscovery(FIXTURE_ORIGIN)?.display_name).toBe('Elitea');
  });

  it('revalidates with If-None-Match and serves the cached copy on 304', async () => {
    const { fetch, seen } = fakeFetch(respond(200, discoveryDocument(), { ETag: '"abc"' }), respond(304));
    await fetchDiscovery(FIXTURE_ORIGIN, { fetch });
    const second = await fetchDiscovery(FIXTURE_ORIGIN, { fetch });
    expect(seen[1].headers.get('If-None-Match')).toBe('"abc"');
    expect(second).toEqual({ ok: true, document: expect.objectContaining({ server_version: '1.62' }) });
  });

  it.each([
    [404, 'not_supported'],
    [503, 'unavailable'],
    [500, 'unavailable'],
  ])('maps HTTP %i to %s', async (status, error) => {
    const { fetch } = fakeFetch(respond(status, { error: 'x' }));
    expect(await fetchDiscovery(FIXTURE_ORIGIN, { fetch })).toEqual({ ok: false, error });
  });

  it('maps a transport failure to unreachable', async () => {
    const { fetch } = fakeFetch(new TypeError('Network request failed'));
    expect(await fetchDiscovery(FIXTURE_ORIGIN, { fetch })).toEqual({ ok: false, error: 'unreachable' });
  });

  it('refuses an invalid body', async () => {
    const { fetch } = fakeFetch(respond(200, { hello: 'world' }));
    expect(await fetchDiscovery(FIXTURE_ORIGIN, { fetch })).toEqual({ ok: false, error: 'invalid' });
  });

  it('refuses a response redirected to another origin', async () => {
    const redirected = respond(200, discoveryDocument());
    Object.defineProperty(redirected, 'url', { value: 'https://evil.example/.well-known/elitea-client' });
    const { fetch } = fakeFetch(redirected);
    expect(await fetchDiscovery(FIXTURE_ORIGIN, { fetch })).toEqual({ ok: false, error: 'origin_mismatch' });
  });

  it('never stores anything in the session', async () => {
    const { fetch } = fakeFetch(respond(200, discoveryDocument()));
    await fetchDiscovery(FIXTURE_ORIGIN, { fetch });
    expect(getSession().serverOrigin).toBeNull();
  });
});

describe('assessDiscovery', () => {
  const parse = (doc: unknown) => DiscoveryDocumentSchema.parse(doc);

  it('is ready with native auth, a supported contract and no minimum', () => {
    expect(assessDiscovery(parse(discoveryDocument()), '0.1.0')).toEqual({ kind: 'ready' });
  });

  it('reports a workspace without native sign-in', () => {
    expect(assessDiscovery(parse(discoveryWithoutNativeAuth()), '0.1.0')).toEqual({ kind: 'native_auth_unavailable' });
  });

  it('refuses a workspace without S256', () => {
    const doc = discoveryDocument();
    doc.native_auth!.code_challenge_methods_supported = ['plain'];
    expect(assessDiscovery(parse(doc), '0.1.0').kind).toBe('native_auth_unavailable');
  });

  it('refuses another contract major', () => {
    expect(assessDiscovery(parse(discoveryDocument({ client_contract: '2.0' })), '0.1.0')).toEqual({
      kind: 'contract_unsupported',
      serverContract: '2.0',
    });
  });

  it("uses this app's own minimum before the deployment-wide one", () => {
    const doc = parse(
      discoveryDocument({
        client_policy: { require_device_lock: true, offline_enabled: false, min_client_version: '0.1.0' },
        min_client_version: { 'ca.zefir.agent': '0.3.0' },
      }),
    );
    expect(minimumVersionFor(doc, 'ca.zefir.agent')).toBe('0.3.0');
    expect(minimumVersionFor(doc, 'other.client')).toBe('0.1.0');
    expect(assessDiscovery(doc, '0.2.0', 'ca.zefir.agent')).toEqual({ kind: 'upgrade_required', minVersion: '0.3.0' });
    expect(assessDiscovery(doc, '0.3.0', 'ca.zefir.agent')).toEqual({ kind: 'ready' });
  });

  it('parses contract majors', () => {
    expect(contractMajor('1.0')).toBe(1);
    expect(contractMajor('1')).toBe(1);
    expect(contractMajor('one')).toBeNull();
  });
});
