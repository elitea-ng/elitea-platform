/**
 * WS-SEC-3: removing a workspace that is not active (wipeStoredWorkspace)
 * never emptied the caches directory, so its downloaded brand font files
 * stayed on the device (they name its brand and server). Font files now live
 * in a folder per workspace origin, deleted with the workspace.
 */
import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { wipeStoredWorkspace } from '@/security/endSession';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace, setSecureItemIn } from '@/security/secureStorage';
import { __resetRegistryForTests } from '@/security/workspaceRegistry';
import { downloadBrandFont } from '@/theme/brandAssets';
import { TEST_WORKSPACE, registryValue, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => {
  const files = new Map<string, Uint8Array>();
  const dirs = new Set<string>();
  const join = (parts: unknown[]) =>
    parts
      .map((p) => (typeof p === 'string' ? p : (p as { uri: string }).uri))
      .map((p, i) => (i === 0 ? p.replace(/\/$/, '') : p.replace(/^\/|\/$/g, '')))
      .join('/');
  class Directory {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    get exists() {
      return dirs.has(this.uri);
    }
    create() {
      dirs.add(this.uri);
    }
    delete() {
      for (const d of [...dirs]) if (d === this.uri || d.startsWith(`${this.uri}/`)) dirs.delete(d);
      for (const f of [...files.keys()]) if (f.startsWith(`${this.uri}/`)) files.delete(f);
    }
    list() {
      return [];
    }
  }
  class File {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    get exists() {
      return files.has(this.uri);
    }
    create() {
      files.set(this.uri, new Uint8Array());
    }
    write(bytes: Uint8Array) {
      files.set(this.uri, bytes);
    }
    delete() {
      files.delete(this.uri);
    }
  }
  return { __files: files, Directory, File, Paths: { cache: 'file:///cache/' } };
});

const files = jest.requireMock<{ __files: Map<string, Uint8Array> }>('expo-file-system').__files;
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const OTHER = 'bbbbbbbbbbbb';
const OTHER_ORIGIN = 'https://other.example.org';
const font = (origin: string) => ({ alias: 'brand-0123456789abcdef0123456789abcdef', url: `${origin}/api/v2/branding/assets/acme.ttf` });
const ttf = async () =>
  new Response(new Uint8Array([0, 1, 0, 0]) as unknown as BodyInit, { status: 200, headers: { 'Content-Type': 'font/ttf' } });

beforeEach(() => {
  __resetRegistryForTests();
  files.clear();
  fake.values.clear();
  jest.spyOn(console, 'info').mockImplementation(() => {});
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

it("deletes a removed workspace's brand font files and keeps the active one's", async () => {
  bindWorkspace(TEST_WORKSPACE);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([TEST_WORKSPACE, OTHER]));
  await setSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.serverOrigin, FIXTURE_ORIGIN);
  await setSecureItemIn(OTHER, SECURE_KEYS.serverOrigin, OTHER_ORIGIN);
  const mine = await downloadBrandFont(font(FIXTURE_ORIGIN), ttf);
  const theirs = await downloadBrandFont(font(OTHER_ORIGIN), ttf);
  expect(mine).not.toBeNull();
  expect(theirs).not.toBeNull();
  // The same alias in two workspaces is still two files.
  expect(mine).not.toBe(theirs);

  const { wiped } = await wipeStoredWorkspace(OTHER, { reason: 'user_logout' });

  expect(wiped).toBe(true);
  expect(files.has(theirs!)).toBe(false);
  expect(files.has(mine!)).toBe(true);
});
