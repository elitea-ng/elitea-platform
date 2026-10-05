import { FIXTURE_ORIGIN, brandPack, discoveryDocument, discoveryWithoutNativeAuth } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema, type DiscoveryResult } from '@/api/discovery';
import { canConnect, checkWorkspace, confirmConnection, displayHost } from '@/features/connect/connectFlow';
import { SECURE_KEYS, getSecureItem } from '@/security/secureStorage';
import { __resetSessionForTests, getSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, getWorkspaceInfo } from '@/state/workspaceInfo';
import { BrandPackSchema, mapBrandPack } from '@/theme/brandPack';
import { __resetBrandThemeForTests, getBrandTheme } from '@/theme/brandStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const noBrand = async () => null;
const ok = (doc: unknown): DiscoveryResult => ({ ok: true, document: DiscoveryDocumentSchema.parse(doc) });

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  __resetBrandThemeForTests();
});

describe('checkWorkspace', () => {
  it('returns a confirmation with the assessment and stores nothing', async () => {
    const check = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ok(discoveryDocument()),
      fetchBrandTheme: noBrand,
      clientVersion: '0.1.0',
    });
    expect(check).toMatchObject({ kind: 'confirm', origin: FIXTURE_ORIGIN, assessment: { kind: 'ready' } });
    expect(getSession().serverOrigin).toBeNull();
    expect(await getSecureItem(SECURE_KEYS.serverOrigin)).toBeNull();
  });

  it('passes a failure through, offering only a clean canonical origin', async () => {
    const canonical = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ({ ok: false, error: 'origin_mismatch', canonicalOrigin: 'https://www.example.com' }),
    });
    expect(canonical).toEqual({
      kind: 'failed',
      origin: FIXTURE_ORIGIN,
      error: 'origin_mismatch',
      canonicalOrigin: 'https://www.example.com',
    });
    const odd = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ({ ok: false, error: 'origin_mismatch', canonicalOrigin: 'https://WWW.example.com:443' }),
    });
    expect(odd).toMatchObject({ kind: 'failed', canonicalOrigin: undefined });
  });

  it('never offers a plain-http canonical origin (the address rules of a typed one apply)', async () => {
    // A discovery document naming an http:// issuer must not lead the user,
    // one tap later, to a cleartext workspace: displayHost hides the scheme.
    for (const allowPrivateHttp of [false, true]) {
      const check = await checkWorkspace(FIXTURE_ORIGIN, {
        fetchDiscovery: async () => ({ ok: false, error: 'origin_mismatch', canonicalOrigin: 'http://www.example.com' }),
        allowPrivateHttp,
      });
      expect(check).toMatchObject({ kind: 'failed', error: 'origin_mismatch', canonicalOrigin: undefined });
    }
    const lan = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ({ ok: false, error: 'origin_mismatch', canonicalOrigin: 'http://192.168.1.10:8080' }),
      allowPrivateHttp: false,
    });
    expect(lan).toMatchObject({ kind: 'failed', canonicalOrigin: undefined });
    const devLan = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ({ ok: false, error: 'origin_mismatch', canonicalOrigin: 'http://192.168.1.10:8080' }),
      allowPrivateHttp: true,
    });
    expect(devLan).toMatchObject({ kind: 'failed', canonicalOrigin: 'http://192.168.1.10:8080' });
  });
});

describe('canConnect', () => {
  const confirm = async (doc: unknown, clientVersion = '0.1.0') =>
    checkWorkspace(FIXTURE_ORIGIN, { fetchDiscovery: async () => ok(doc), fetchBrandTheme: noBrand, clientVersion });

  it('allows a ready workspace', async () => {
    expect(canConnect(await confirm(discoveryDocument()), false)).toBe(true);
  });

  it('allows a workspace without native sign-in only for developer sign-in', async () => {
    const check = await confirm(discoveryWithoutNativeAuth());
    expect(canConnect(check, false)).toBe(false);
    expect(canConnect(check, true)).toBe(true);
  });

  it('blocks an outdated app and an unsupported contract even in development', async () => {
    expect(canConnect(await confirm(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '9.0.0' } })), true)).toBe(false);
    expect(canConnect(await confirm(discoveryDocument({ client_contract: '2.0' })), true)).toBe(false);
  });

  it('allows a server without discovery only for developer sign-in', () => {
    const failed = { kind: 'failed', origin: FIXTURE_ORIGIN, error: 'not_supported' } as const;
    expect(canConnect(failed, false)).toBe(false);
    expect(canConnect(failed, true)).toBe(true);
    expect(canConnect({ ...failed, error: 'unreachable' }, true)).toBe(false);
  });
});

describe('confirmConnection', () => {
  it('stores the origin first, then applies the previewed brand and publishes the document', async () => {
    const brand = mapBrandPack(BrandPackSchema.parse(brandPack()), FIXTURE_ORIGIN, 'file');
    const fetchBrand = jest.fn(async () => brand);
    const check = await checkWorkspace(FIXTURE_ORIGIN, {
      fetchDiscovery: async () => ok(discoveryDocument()),
      fetchBrandTheme: fetchBrand,
    });
    expect(check).toMatchObject({ kind: 'confirm', brand: { productName: 'Elitea' } });
    expect(fetchBrand).toHaveBeenCalledWith(FIXTURE_ORIGIN, discoveryDocument().brand_pack_url);
    expect(getBrandTheme()).toBeNull(); // nothing applied before the user confirms

    expect(await confirmConnection(check)).toBe(true);
    expect(await getSecureItem(SECURE_KEYS.serverOrigin)).toBe(FIXTURE_ORIGIN);
    expect(getSession().serverOrigin).toBe(FIXTURE_ORIGIN);
    expect(getWorkspaceInfo()?.discovery.display_name).toBe('Elitea');
    expect(getBrandTheme()).toBe(brand);
  });
});

it('shows the host without the scheme', () => {
  expect(displayHost('https://ai.example.com:8443')).toBe('ai.example.com:8443');
});
