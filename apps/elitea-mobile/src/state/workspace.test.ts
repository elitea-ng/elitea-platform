import { SECURE_KEYS } from '@/security/secureStorage';
import { __resetSessionForTests, getSession, setCredential, setSession } from '@/state/session';
import { classifyIncomingLink, connectWorkspace, switchWorkspace } from '@/state/workspace';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const OPTS = { connectSchemes: ['agentzefir'], allowPrivateHttp: false };

beforeEach(() => {
  __resetSessionForTests();
  fake.values.clear();
  jest.clearAllMocks();
  // clearAllMocks keeps implementations: put back the default after a test overrode it.
  fake.deleteItemAsync.mockImplementation(async (key: string) => {
    fake.values.delete(key);
  });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('connectWorkspace', () => {
  it('persists the origin before the session changes', async () => {
    let sessionAtWrite: string | null | undefined;
    fake.setItemAsync.mockImplementationOnce(async (key: string, value: string) => {
      sessionAtWrite = getSession().serverOrigin;
      fake.values.set(key, value);
    });
    await expect(connectWorkspace('https://a.example.com')).resolves.toBe(true);
    expect(sessionAtWrite).toBeNull();
    expect(fake.values.get(SECURE_KEYS.serverOrigin)).toBe('https://a.example.com');
    expect(getSession().serverOrigin).toBe('https://a.example.com');
  });

  it('does not connect when the origin cannot be stored', async () => {
    fake.setItemAsync.mockImplementationOnce(async () => {
      throw new Error('keychain');
    });
    await expect(connectWorkspace('https://a.example.com')).resolves.toBe(false);
    expect(getSession().serverOrigin).toBeNull();
  });
});

describe('switchWorkspace', () => {
  it('wipes the current workspace, then hands the new one to the connect screen unconfirmed', async () => {
    setSession({ hydrated: true, serverOrigin: 'https://a.example.com', projectId: 3 });
    setCredential({ kind: 'pat', token: 't' });
    fake.values.set(SECURE_KEYS.pat, 't');
    fake.values.set(SECURE_KEYS.serverOrigin, 'https://a.example.com');

    await switchWorkspace('https://b.example.com');

    expect(getSession()).toMatchObject({
      serverOrigin: null,
      pendingConnectOrigin: 'https://b.example.com',
      credential: null,
      projectId: null,
    });
    expect(fake.values.has(SECURE_KEYS.pat)).toBe(false);
    // Nothing is stored for the new workspace until the user confirms it.
    expect(fake.values.has(SECURE_KEYS.serverOrigin)).toBe(false);
  });

  it('does not connect the new origin while a credential of the old one could not be deleted', async () => {
    setSession({ hydrated: true, serverOrigin: 'https://a.example.com' });
    setCredential({ kind: 'pat', token: 't' });
    fake.values.set(SECURE_KEYS.pat, 't');
    fake.values.set(SECURE_KEYS.serverOrigin, 'https://a.example.com');
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === SECURE_KEYS.pat) throw new Error('keystore error');
      fake.values.delete(key);
    });

    await expect(switchWorkspace('https://b.example.com')).resolves.toBe(false);

    expect(fake.values.get(SECURE_KEYS.serverOrigin)).not.toBe('https://b.example.com');
    expect(getSession().serverOrigin).toBeNull();
    expect(getSession().pendingConnectOrigin).toBeNull();
  });

  it('is a no-op for the same origin', async () => {
    setSession({ serverOrigin: 'https://a.example.com' });
    setCredential({ kind: 'pat', token: 't' });
    await switchWorkspace('https://a.example.com');
    expect(getSession().credential).toEqual({ kind: 'pat' });
    expect(fake.deleteItemAsync).not.toHaveBeenCalled();
  });
});

describe('classifyIncomingLink', () => {
  const link = (server: string) => `agentzefir://connect?server=${encodeURIComponent(server)}`;

  it('asks to switch for another workspace while connected', () => {
    expect(classifyIncomingLink(link('https://b.example.com/x'), 'https://a.example.com', OPTS)).toEqual({
      kind: 'switch',
      from: 'https://a.example.com',
      origin: 'https://b.example.com',
    });
  });

  it('is a no-op for the connected workspace', () => {
    expect(classifyIncomingLink(link('https://a.example.com'), 'https://a.example.com', OPTS)).toEqual({
      kind: 'same',
    });
  });

  it('leaves it to the connect screen when not connected', () => {
    expect(classifyIncomingLink(link('https://b.example.com'), null, OPTS)).toEqual({
      kind: 'connect',
      origin: 'https://b.example.com',
    });
  });

  it('ignores non-connect URLs and invalid links', () => {
    for (const url of [
      null,
      'ca.zefir.agent:/oauth/callback?code=x',
      'exp+agent-zefir://expo-development-client/?url=x',
      'https://b.example.com',
      link('http://b.example.com'),
      'agentzefir://connect',
    ]) {
      expect(classifyIncomingLink(url, 'https://a.example.com', OPTS)).toEqual({ kind: 'ignore' });
    }
  });
});
