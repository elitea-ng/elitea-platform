import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react-native';

import { CAPTURE, conversationDetail13 } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { queryKeys } from '@/api/queries/keys';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { __setOnlineForTests } from '@/lib/network';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

// Issue 1835: the thread draws the answering agent's avatar pack, and only from a server that
// speaks client contract 1.46: from `persona.avatar_pack_v1` (1.49, #1881), else from the object a
// 1.46 to 1.48 server sent as `persona.avatar_pack`.

jest.mock('expo-router', () => ({ useIsFocused: () => true, useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: () => null }, router: { replace: jest.fn(), push: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// The media download goes through expo/fetch (redirects refused); route it to the fake server.
const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));

const P = CAPTURE.projectId;
const STILL = `/api/v2/agent_media/${P}/${'a'.repeat(64)}.png`;
const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d]);
const PNG_DATA = `data:image/png;base64,${btoa(String.fromCharCode(...PNG))}`;
// The checked still is drawn from its file in the caches folder (issue 1882), named by its digest.
const isStillFile = (uri: unknown, still = STILL) => typeof uri === 'string' && uri.startsWith('file:') && uri.endsWith(still.slice(still.lastIndexOf('/')));

const withPack = (key: 'avatar_pack' | 'avatar_pack_v1', still = STILL) => {
  const detail = structuredClone(conversationDetail13) as { participants: { id: number; persona?: unknown }[] };
  const agent = detail.participants.find((p) => p.id === 3)!;
  agent.persona = { display_name: 'Mira', [key]: { schema: 'elitea.avatar_pack/v1', slots: { still: { url: still, media_type: 'image/png' } } } };
  return detail;
};

let server: FakeServer;
let queries: QueryClient | null = null;

async function open(contract: string, { key = 'avatar_pack_v1', still = STILL }: { key?: 'avatar_pack' | 'avatar_pack_v1'; still?: string } = {}) {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: withPack(key, still) });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', still.replace(/^\/api\/v2/, ''), { status: 200, raw: PNG, headers: { 'Content-Type': 'image/png' } });
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  queries.setQueryData(queryKeys.conversations(P), { pages: [{ rows: [], total: 0, offset: 0 }], pageParams: [0] });
  await render(
    <QueryClientProvider client={queries}>
      <ThreadScreen />
    </QueryClientProvider>,
  );
}

const mediaRequests = (still = STILL) => server.requests.filter((r) => r.url.pathname === still);
const images = () => screen.queryAllByTestId('avatar-image', { includeHiddenElements: true }).map((node) => node.props.source?.uri);

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  __setOnlineForTests(true);
  clearLiveTurns();
  resetPolicy();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  mockExpoFetch.mockImplementation((url: string, init?: RequestInit) => server.fetch(new Request(url, init)));
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

describe('avatar packs in the thread', () => {
  it("shows the answering agent's pack still beside its answer, read from avatar_pack_v1", async () => {
    await open('1.49');
    await waitFor(() => expect(images().some((uri) => isStillFile(uri))).toBe(true));
    const [request] = mediaRequests();
    expect(request).toBeDefined();
    expect(request.url.origin).toBe(FAKE_ORIGIN);
  });

  it('reads the object a 1.46 server sent as persona.avatar_pack', async () => {
    const still = `/api/v2/agent_media/${P}/${'b'.repeat(64)}.png`;
    await open('1.46', { key: 'avatar_pack', still });
    await waitFor(() => expect(images().some((uri) => isStillFile(uri, still))).toBe(true));
  });

  it("downloads the primary agent's still once when its icon is the same file (issue 1882)", async () => {
    const still = `/api/v2/agent_media/${P}/${'c'.repeat(64)}.png`;
    server.on('GET', `/elitea_core/application/prompt_lib/${P}/1`, {
      status: 200,
      body: { id: 1, name: 'Elitea Tool Agent', version_details: { id: 1, meta: { icon_meta: { url: still } } } },
    });
    await open('1.49', { still });
    await waitFor(() => expect(images().some((uri) => isStillFile(uri, still))).toBe(true));
    await waitFor(() => expect(server.requests.some((r) => r.url.pathname.endsWith(`/application/prompt_lib/${P}/1`))).toBe(true));
    expect(mediaRequests(still)).toHaveLength(1);
  });

  it('reads no pack and downloads no media from a server before 1.46', async () => {
    await open('1.45');
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(mediaRequests()).toEqual([]);
    expect(images()).not.toContain(PNG_DATA);
  });
});
