/**
 * Issue 1882: checked avatar pack media is kept as files in the caches folder, one folder per
 * workspace origin, deleted with the workspace even when it is not the active one.
 */
import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { agentMediaFileName, agentMediaFiles } from '@/features/chat/agentMediaFiles';
import { wipeStoredWorkspace } from '@/security/endSession';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace, setSecureItemIn } from '@/security/secureStorage';
import { __resetRegistryForTests } from '@/security/workspaceRegistry';
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
    get name() {
      return this.uri.slice(this.uri.lastIndexOf('/') + 1);
    }
    moveSync(destination: { uri: string }) {
      const bytes = files.get(this.uri);
      if (bytes === undefined) throw new Error('no such file');
      if (failMoves.value) throw new Error('disk full');
      files.delete(this.uri);
      files.set(destination.uri, bytes);
      this.uri = destination.uri;
    }
  }
  const failMoves = { value: false };
  return { __files: files, __failMoves: failMoves, Directory, File, Paths: { cache: 'file:///cache/' } };
});

const { __files: files, __failMoves: failMoves } = jest.requireMock<{ __files: Map<string, Uint8Array>; __failMoves: { value: boolean } }>('expo-file-system');
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const OTHER = 'bbbbbbbbbbbb';
const OTHER_ORIGIN = 'https://other.example.org';
const STILL = `/api/v2/agent_media/7/${'a'.repeat(64)}.png`;
const BYTES = new Uint8Array([1, 2, 3]);

beforeEach(() => {
  __resetRegistryForTests();
  files.clear();
  failMoves.value = false;
  fake.values.clear();
  jest.spyOn(console, 'info').mockImplementation(() => {});
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

it('names a file only for a pack url', () => {
  expect(agentMediaFileName(`${FIXTURE_ORIGIN}${STILL}`)).toBe(`${'a'.repeat(64)}.png`);
  expect(agentMediaFileName(`${FIXTURE_ORIGIN}/api/v2/agent_media/7/${'a'.repeat(64)}.svg`)).toBeNull();
  expect(agentMediaFileName(`${FIXTURE_ORIGIN}/api/v2/agent_media/7/../../x.png`)).toBeNull();
  expect(agentMediaFileName(`${FIXTURE_ORIGIN}/icons/x.png`)).toBeNull();
});

it('stores checked bytes once and finds them again by the url', () => {
  const uri = `${FIXTURE_ORIGIN}${STILL}`;
  expect(agentMediaFiles.cached(uri)).toBeNull();
  const stored = agentMediaFiles.store(uri, BYTES);
  expect(stored).toMatch(/^file:\/\/\/cache\/agent-media\/[0-9a-f]{32}\/a{64}\.png$/);
  expect(files.get(stored!)).toEqual(BYTES);
  expect(agentMediaFiles.cached(uri)).toBe(stored);
  expect(agentMediaFiles.store(`${FIXTURE_ORIGIN}/icons/x.png`, BYTES)).toBeNull();
  // Only the final file is left: the temporary one was moved into place.
  expect([...files.keys()]).toEqual([stored]);
});

it('leaves no file under the final name when the write fails, so the next read downloads again', () => {
  const uri = `${FIXTURE_ORIGIN}${STILL}`;
  failMoves.value = true;
  expect(agentMediaFiles.store(uri, BYTES)).toBeNull();
  expect(agentMediaFiles.cached(uri)).toBeNull();
  expect(files.size).toBe(0);
});

it("deletes a removed workspace's pack media and keeps the active one's", async () => {
  bindWorkspace(TEST_WORKSPACE);
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([TEST_WORKSPACE, OTHER]));
  await setSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.serverOrigin, FIXTURE_ORIGIN);
  await setSecureItemIn(OTHER, SECURE_KEYS.serverOrigin, OTHER_ORIGIN);
  const mine = agentMediaFiles.store(`${FIXTURE_ORIGIN}${STILL}`, BYTES);
  const theirs = agentMediaFiles.store(`${OTHER_ORIGIN}${STILL}`, BYTES);
  expect(mine).not.toBe(theirs);

  const { wiped } = await wipeStoredWorkspace(OTHER, { reason: 'user_logout' });

  expect(wiped).toBe(true);
  expect(files.has(theirs!)).toBe(false);
  expect(files.has(mine!)).toBe(true);
});
