import { FIXTURE_ORIGIN, discoveryDocument } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema, type DiscoveryResult } from '@/api/discovery';
import { endSession } from '@/security/endSession';
import { __resetSessionForTests, setSession } from '@/state/session';
import {
  __resetWorkspaceInfoForTests,
  getWorkspaceInfo,
  onDiscovery,
  refreshWorkspaceInfo,
} from '@/state/workspaceInfo';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const doc = DiscoveryDocumentSchema.parse(discoveryDocument());

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
});

it('refreshes the connected workspace once for concurrent callers and tells listeners', async () => {
  setSession({ serverOrigin: FIXTURE_ORIGIN });
  const seen: string[] = [];
  const off = onDiscovery((origin) => seen.push(origin));
  const fetchImpl = jest.fn(async (): Promise<DiscoveryResult> => ({ ok: true, document: doc }));
  await Promise.all([refreshWorkspaceInfo(fetchImpl), refreshWorkspaceInfo(fetchImpl)]);
  off();
  expect(fetchImpl).toHaveBeenCalledTimes(1);
  expect(getWorkspaceInfo()).toEqual({ origin: FIXTURE_ORIGIN, discovery: doc });
  expect(seen).toEqual([FIXTURE_ORIGIN]);
});

it('drops a document that arrives after the workspace changed', async () => {
  setSession({ serverOrigin: FIXTURE_ORIGIN });
  let release: (r: DiscoveryResult) => void = () => {};
  const pending = refreshWorkspaceInfo(() => new Promise((resolve) => (release = resolve)));
  setSession({ serverOrigin: 'https://other.example.com' });
  release({ ok: true, document: doc });
  await pending;
  expect(getWorkspaceInfo()).toBeNull();
});

it('keeps the last document when a refresh fails', async () => {
  setSession({ serverOrigin: FIXTURE_ORIGIN });
  await refreshWorkspaceInfo(async () => ({ ok: true, document: doc }));
  await refreshWorkspaceInfo(async () => ({ ok: false, error: 'unreachable' }));
  expect(getWorkspaceInfo()?.discovery).toBe(doc);
});

it('is wiped with the session', async () => {
  setSession({ serverOrigin: FIXTURE_ORIGIN });
  await refreshWorkspaceInfo(async () => ({ ok: true, document: doc }));
  await endSession({ reason: 'server_switch' });
  expect(getWorkspaceInfo()).toBeNull();
});

// F7: endSession clears this store (step 4) before serverOrigin is reset (step 6);
// a discovery landing in between must not repopulate it.
it('drops a document that arrives while the session is ending', async () => {
  setSession({ serverOrigin: FIXTURE_ORIGIN });
  let answer!: (result: DiscoveryResult) => void;
  const pending = refreshWorkspaceInfo(() => new Promise<DiscoveryResult>((resolve) => (answer = resolve)));
  setSession({ ending: true });
  answer({ ok: true, document: doc });
  await pending;
  expect(getWorkspaceInfo()).toBeNull();
});
