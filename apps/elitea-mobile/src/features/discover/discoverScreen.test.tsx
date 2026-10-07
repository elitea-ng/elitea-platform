import { QueryClient, QueryClientProvider, type InfiniteData } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { addedPublicAgent, createdConversation, publicDetail, publicList } from '@/api/__fixtures__/discover13';
import { discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { publicAgentPageOf, type PublicAgentPage } from '@/api/queries/discover';
import PublicAgentScreen from '@/app/(app)/discover/[id]';
import DiscoverScreen, { SEARCH_DEBOUNCE_MS } from '@/app/(app)/(tabs)/discover/index';
import { monogramOf } from '@/features/discover/AgentIcon';
import { readHandoff } from '@/features/intents/intentHandoff';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

const mockPush = jest.fn();
const mockReplace = jest.fn();
const mockBack = jest.fn();
let mockParams: Record<string, string> = {};
jest.mock('expo-router', () => ({
  router: {
    push: (...args: unknown[]) => mockPush(...args),
    replace: (...args: unknown[]) => mockReplace(...args),
    back: () => mockBack(),
  },
  useLocalSearchParams: () => mockParams,
}));
// Icons are downloaded with redirects refused (expo/fetch, redirect: 'error'), never handed to <Image> as a URL.
const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));
const PNG_BYTES = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d]);
const PNG_DATA = `data:image/png;base64,${btoa(String.fromCharCode(...PNG_BYTES))}`;
const pngAnswer = () => new Response(PNG_BYTES as unknown as BodyInit, { status: 200, headers: { 'Content-Type': 'image/png' } });
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
let mockUuid = 0;
jest.mock('expo-crypto', () => ({ randomUUID: () => `00000000-0000-4000-8000-${String(++mockUuid).padStart(12, '0')}` }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// The active project: Alice's personal project in the capture.
jest.mock('@/features/projects/useProjectSelection', () => ({
  useProjectSelection: () => ({ projectId: 90501, project: { id: 90501, name: 'project_user_8', suspended: false }, noProject: false, isLoading: false, error: null }),
}));
jest.mock('@/state/selfIdentity', () => ({ usePersonalProjectId: () => 90501 }));

const P = 90501;
const LIST = '/elitea_core/public_applications/prompt_lib';
const CREATE = `/elitea_core/conversations/prompt_lib/${P}`;
const ADD = `/elitea_core/participants/prompt_lib/${P}/${createdConversation.uuid}`;

function install(contract: '1.1' | '1.3'): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', LIST, { status: 200, body: publicList });
  server.on('GET', '/elitea_core/public_application/prompt_lib/2/v1', { status: 200, body: publicDetail });
  server.on('POST', CREATE, { status: 201, body: createdConversation });
  server.on('POST', ADD, { status: 200, body: addedPublicAgent });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(contract === '1.3' ? discoveryDocument13() : discoveryDocument11()) });
  return server;
}

let queries: QueryClient;
function withQueries(ui: ReactElement) {
  return <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  mockPush.mockReset();
  mockReplace.mockReset();
  mockBack.mockReset();
  mockParams = {};
  mockExpoFetch.mockReset();
  mockExpoFetch.mockImplementation(async () => pngAnswer());
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});
afterEach(() => {
  queries.clear();
});

/** The onPress a host element's Pressable carries, to call it without a render in between. */
function pressHandlerOf(element: { unstable_fiber?: unknown }): () => void {
  type Fiber = { memoizedProps?: { onPress?: () => void } | null; return: Fiber | null };
  let fiber = element.unstable_fiber as Fiber | null | undefined;
  while (fiber && typeof fiber.memoizedProps?.onPress !== 'function') fiber = fiber.return;
  if (!fiber?.memoizedProps?.onPress) throw new Error('no onPress');
  return fiber.memoizedProps.onPress;
}

/** Renders the list and waits for the catalogue, so the details screen finds the agent in the cache. */
async function openCatalogue() {
  const view = await render(withQueries(<DiscoverScreen />));
  expect(await screen.findByText('Zefir Travel Planner')).toBeTruthy();
  return view;
}

describe('Discover is client contract 1.3', () => {
  // The tab is always there (discoverTab.test.tsx), so the screen does the gating.
  it('before the workspace is known the screen waits for it, without asking the server or saying "not available"', async () => {
    const server = install('1.3');
    __resetWorkspaceInfoForTests();
    await render(withQueries(<DiscoverScreen />));
    expect(screen.getByText('Checking this workspace')).toBeTruthy();
    expect(screen.queryByText("Discover isn't available")).toBeNull();
    expect(server.requests).toHaveLength(0);
    await act(async () => setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13()) }));
    expect(await screen.findByText('Zefir Travel Planner')).toBeTruthy();
  });

  it('on a 1.1 workspace the screen says it is not available without asking the server', async () => {
    const server = install('1.1');
    await render(withQueries(<DiscoverScreen />));
    expect(screen.getByText("Discover isn't available")).toBeTruthy();
    expect(server.requests).toHaveLength(0);
  });
});

describe('Discover list', () => {
  it('shows each public agent with its description, category, tags and icon (workspace origin only)', async () => {
    install('1.3');
    await openCatalogue();
    expect(screen.getByText('Zefir Concierge')).toBeTruthy();
    expect(screen.getByText('Zefir Concierge: a public agent anyone can find in Discover.')).toBeTruthy();
    expect(screen.getByText('Project Management')).toBeTruthy();
    expect(screen.getByText('#planning')).toBeTruthy();
    expect(screen.getByText('#travel')).toBeTruthy();
    expect(screen.getByText('Other')).toBeTruthy();
    // The icon is downloaded through the origin guard with redirects refused and shown from its bytes.
    await waitFor(() => expect(screen.getAllByTestId('agent-icon-image')).toHaveLength(1));
    expect(screen.getByTestId('agent-icon-image').props.source).toEqual({ uri: PNG_DATA });
    expect(mockExpoFetch).toHaveBeenCalledTimes(1);
    const [url, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${FAKE_ORIGIN}/icons/1/076958cfcd17545b.png`);
    expect(init.redirect).toBe('error');
    expect(new Headers(init.headers).get('Authorization')).toBeNull();
    expect(screen.getByText('ZC')).toBeTruthy();
  });

  it('an icon path that redirects (or answers anything but a vetted image) shows the monogram; <Image> never gets a URL', async () => {
    install('1.3');
    mockExpoFetch.mockImplementation(async () => {
      throw new TypeError("Redirect is not allowed when redirect mode is 'error'");
    });
    await openCatalogue();
    await waitFor(() => expect(mockExpoFetch).toHaveBeenCalled());
    await act(async () => {});
    // AgentIcon is the only <Image> on the list: none is rendered, so none was given the URL.
    expect(screen.queryByTestId('agent-icon-image')).toBeNull();
    expect(screen.getByText('ZT')).toBeTruthy();
  });

  it('searches on the server once typing pauses', async () => {
    const server = install('1.3');
    await openCatalogue();
    await fireEvent.changeText(screen.getByLabelText('Search public agents'), 'trav');
    await fireEvent.changeText(screen.getByLabelText('Search public agents'), 'travel');
    expect(server.requests.some((r) => r.url.searchParams.has('query'))).toBe(false);
    await waitFor(() => expect(server.requests.some((r) => r.url.searchParams.get('query') === 'travel')).toBe(true), {
      timeout: SEARCH_DEBOUNCE_MS * 5,
    });
    expect(server.requests.some((r) => r.url.searchParams.get('query') === 'trav')).toBe(false);
  });

  it('says so when nothing matches', async () => {
    const server = install('1.3');
    server.on('GET', LIST, { status: 200, body: { rows: [], total: 0 } });
    await render(withQueries(<DiscoverScreen />));
    expect(await screen.findByText('This workspace has no public agents yet.')).toBeTruthy();
  });

  it('a row opens the agent', async () => {
    install('1.3');
    await openCatalogue();
    await fireEvent.press(screen.getByTestId('public-agent-2'));
    expect(mockPush).toHaveBeenCalledWith({ pathname: '/discover/[id]', params: { id: '2' } });
  });
});

describe('Agent details', () => {
  it('shows the agent, its welcome message and starters, and starts a chat in the active project', async () => {
    const server = install('1.3');
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    expect(screen.getByText('Zefir Travel Planner')).toBeTruthy();
    expect(screen.getByText('Agent · version v1')).toBeTruthy();
    expect(await screen.findByText('Welcome to Zefir Travel Planner!')).toBeTruthy();
    expect(screen.getByText('What can you do?')).toBeTruthy();
    expect(screen.getByText('The conversation is created in My project.')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Start a chat in My project' }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '9' } }));
    const writes = server.requests.filter((r) => r.method === 'POST');
    expect(writes.map((r) => r.url.pathname)).toEqual([`/api/v2${CREATE}`, `/api/v2${ADD}`]);
    expect(JSON.parse(writes[1].body!)[0]).toEqual({
      entity_name: 'application',
      entity_meta: { id: 2, project_id: 1, name: 'Zefir Travel Planner' },
      entity_settings: { version_id: 2 },
    });
  });

  it('a starter opens the new chat with its text in the composer (handed over in memory, never sent)', async () => {
    install('1.3');
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    await fireEvent.press(await screen.findByTestId('starter-1'));
    await waitFor(() => expect(mockReplace).toHaveBeenCalled());
    const target = mockReplace.mock.calls[0][0] as { pathname: string; params: { id: string; intent: string } };
    expect(target.pathname).toBe('/c/[id]');
    expect(target.params.id).toBe('9');
    expect(readHandoff(target.params.intent)).toEqual({ agent: null, prefill: 'Give me three ideas' });
  });

  it('a second tap before the first one re-renders starts nothing (one conversation, not two)', async () => {
    const server = install('1.3');
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    const starter = await screen.findByTestId('starter-1');
    const button = screen.getByTestId('start-public-chat');
    // Three touches inside one frame (one act): the disabled state has not rendered yet.
    await act(async () => {
      pressHandlerOf(button)();
      pressHandlerOf(button)();
      pressHandlerOf(starter)();
    });
    await waitFor(() => expect(mockReplace).toHaveBeenCalled());
    expect(mockReplace).toHaveBeenCalledTimes(1);
    const creates = server.requests.filter((r) => r.method === 'POST' && r.url.pathname === `/api/v2${CREATE}`);
    expect(creates).toHaveLength(1);
  });
  it('a refused start says why and stays on the agent', async () => {
    const server = install('1.3');
    server.on('POST', CREATE, { status: 403, body: { error: 'forbidden' } });
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    await fireEvent.press(screen.getByTestId('start-public-chat'));
    expect(await screen.findByText("You can't start conversations in this project.")).toBeTruthy();
    expect(mockReplace).not.toHaveBeenCalled();
  });

  it('a start that may have left an empty conversation behind says so and refreshes the conversation list', async () => {
    const server = install('1.3');
    server.on('POST', ADD, 'network_error');
    server.on('DELETE', `/elitea_core/conversation/prompt_lib/${P}/`, 'network_error');
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    const invalidate = jest.spyOn(queries, 'invalidateQueries');
    await fireEvent.press(screen.getByTestId('start-public-chat'));
    expect(await screen.findByText("Couldn't start the conversation. An empty one may be left in your list.")).toBeTruthy();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['p', P, 'conversations'] });
    expect(mockReplace).not.toHaveBeenCalled();
  });

  it('works without details (getPublicApplication failing)', async () => {
    const server = install('1.3');
    server.on('GET', '/elitea_core/public_application/prompt_lib/2/v1', { status: 404, body: { error: 'application not found' } });
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    await waitFor(() => expect(server.requests.some((r) => r.url.pathname.includes('/public_application/'))).toBe(true));
    expect(screen.queryByText('Start with')).toBeNull();
    expect(screen.getByTestId('start-public-chat')).toBeTruthy();
  });

  it('follows the catalogue when it refetches underneath: a republished version is the one the chat pins', async () => {
    const server = install('1.3');
    server.on('GET', '/elitea_core/public_application/prompt_lib/2/v2', { status: 404, body: { error: 'application not found' } });
    const list = await openCatalogue();
    await list.unmount();
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    expect(screen.getByText('Agent · version v1')).toBeTruthy();
    // Settled: nothing else on the page re-renders it from here on.
    expect(await screen.findByText('Welcome to Zefir Travel Planner!')).toBeTruthy();
    // The list query refetched (app focus, reconnect) and the agent now has a new published version.
    await act(async () => {
      queries.setQueriesData<InfiniteData<PublicAgentPage, number>>({ queryKey: ['p', P, 'discover', 'list'] }, (data) =>
        data === undefined
          ? data
          : {
              ...data,
              pages: data.pages.map((page) => ({
                ...page,
                agents: page.agents.map((a) => (a.id === 2 ? { ...a, versionId: 5, versionName: 'v2' } : a)),
              })),
            },
      );
    });
    expect(await screen.findByText('Agent · version v2')).toBeTruthy();
    await fireEvent.press(screen.getByTestId('start-public-chat'));
    await waitFor(() => expect(mockReplace).toHaveBeenCalled());
    const add = server.requests.find((r) => r.method === 'POST' && r.url.pathname === `/api/v2${ADD}`);
    expect(JSON.parse(add!.body!)[0].entity_settings).toEqual({ version_id: 5 });
  });

  it('shows the agent once the catalogue arrives (opened before the list of this project was loaded)', async () => {
    install('1.3');
    mockParams = { id: '2' };
    await render(withQueries(<PublicAgentScreen />));
    expect(screen.getByText('Agent not found')).toBeTruthy();
    await act(async () => {
      queries.setQueryData<InfiniteData<PublicAgentPage, number>>(['p', P, 'discover', 'list', ''], {
        pages: [publicAgentPageOf(publicList, 0)],
        pageParams: [0],
      });
    });
    expect(await screen.findByText('Zefir Travel Planner')).toBeTruthy();
    expect(screen.queryByText('Agent not found')).toBeNull();
  });

  it('an agent not in the loaded catalogue reads as not found', async () => {
    install('1.3');
    mockParams = { id: '77' };
    await render(withQueries(<PublicAgentScreen />));
    expect(screen.getByText('Agent not found')).toBeTruthy();
    await fireEvent.press(screen.getByText('Back to Discover'));
    expect(mockBack).toHaveBeenCalled();
  });
});

it('monograms', () => {
  expect(monogramOf('Zefir Travel Planner')).toBe('ZT');
  expect(monogramOf('  solo ')).toBe('S');
  expect(monogramOf('')).toBe('?');
});
