import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import { Alert } from 'react-native';

import { CAPTURE, conversationDetail13 } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { queryKeys } from '@/api/queries/keys';
import { HANDOFF_POLL_MS, mergeHandoff } from '@/api/queries/workspaceHandoff';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

// A desktop "Local work" thread opened on the phone (ADR-0034): the composer works only while the
// cloud executor runs the thread, and the notice offers Take back (owner) and Erase (owner or a
// project settings editor).

let mockSelf: number | null = 8;
jest.mock('@/state/selfIdentity', () => ({
  ...jest.requireActual('@/state/selfIdentity'),
  useSelfUserId: () => mockSelf,
}));
const mockPush = jest.fn();
jest.mock('expo-router', () => ({
  useIsFocused: () => true,
  useLocalSearchParams: () => ({ id: '3' }),
  Stack: { Screen: () => null },
  router: { replace: jest.fn(), push: (...args: unknown[]) => mockPush(...args) },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));

const P = CAPTURE.projectId;
const UUID = CAPTURE.conversationUuid;
const DETAIL = `/elitea_core/conversation/prompt_lib/${P}/3`;
const HANDOFF = `/elitea_core/workspace_handoff/prompt_lib/${P}/${UUID}`;
const RECLAIM = `/elitea_core/workspace_handoff_reclaim_request/prompt_lib/${P}/${UUID}`;
const SEND = `/elitea_core/messages/prompt_lib/${P}/${UUID}`;

interface Setup {
  executor?: 'local' | 'cloud' | 'returning';
  /** The handoff read; omitted: the route is not scripted (404). */
  state?: string;
  reclaimPending?: boolean;
  owner?: boolean;
  settingsEditor?: boolean;
  answerRanOn?: 'desktop';
  contract?: string;
  folder?: string;
  /** The handoff read's `pull_request_credential` (contract 1.29). */
  credential?: { type: 'github' | 'gitlab'; elitea_title: string; private: boolean } | null;
}

let queries: QueryClient | null = null;
let server: FakeServer;

async function open(setup: Setup = {}) {
  const { executor = 'cloud', owner = true, contract = '1.8' } = setup;
  mockSelf = owner ? 8 : 99;
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  const groups = conversationDetail13.message_groups.map((g) =>
    setup.answerRanOn === undefined || g.author_participant_id !== 3 ? g : { ...g, meta: { ...g.meta, executed_by: setup.answerRanOn } },
  );
  server.on('GET', DETAIL, {
    status: 200,
    body: {
      ...conversationDetail13,
      message_groups: groups,
      source: 'local_work',
      workspace_executor: executor,
      meta: { local_work: { folder_name: setup.folder ?? 'api-server' } },
    },
  });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  if (setup.state !== undefined) {
    server.on('GET', HANDOFF, { status: 200, body: { state: setup.state, epoch: 3, reclaim_pending: setup.reclaimPending === true, pull_request_credential: setup.credential ?? null } });
  }
  server.on('GET', `/auth/permissions/prompt_lib/${P}`, {
    status: 200,
    body: setup.settingsEditor ? [{ name: 'models.project_settings.edit', enabled: true }] : [{ name: 'models.chat.messages.create', enabled: true }],
  });
  server.on('POST', RECLAIM, { status: 202, body: {} });
  server.on('DELETE', HANDOFF, { status: 200, body: { state: 'LOCAL', epoch: 4 } });
  // Refused, so no stream is opened: the request is what the send tests check.
  server.on('POST', SEND, { status: 422, body: { error: 'rejected_for_test' } });
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(
    <QueryClientProvider client={queries}>
      <ThreadScreen />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

describe('composer per executor', () => {
  it('cloud: the composer works, with the "Running in the cloud" notice above it', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    expect(await screen.findByLabelText(/^Message/)).toBeTruthy();
    expect(screen.getByText('Running in the cloud')).toBeTruthy();
  });

  it('cloud: a message goes through the normal chat send route', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    const input = await screen.findByLabelText(/^Message/);
    await fireEvent.changeText(input, 'continue from the phone');
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST' && r.url.pathname.endsWith(SEND))).toBe(true));
    const send = server.requests.find((r) => r.method === 'POST' && r.url.pathname.endsWith(SEND))!;
    expect(JSON.parse(send.body ?? '{}')).toMatchObject({ payload: { user_input: 'continue from the phone' } });
  });

  it('returning: read-only, with the laptop-is-taking-it-back notice', async () => {
    await open({ executor: 'returning', state: 'RECLAIMING' });
    expect(await screen.findByText('Your laptop is taking this thread back.')).toBeTruthy();
    expect(screen.queryByLabelText(/^Message/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Ask laptop to take back' })).toBeNull();
  });

  it('local: read-only transcript naming the folder', async () => {
    await open({ executor: 'local', state: 'LOCAL', folder: 'api-server' });
    expect(
      await screen.findByText('This thread runs on api-server on your laptop. Send it to the cloud from the desktop app to continue here.'),
    ).toBeTruthy();
    expect(screen.queryByLabelText(/^Message/)).toBeNull();
    expect(screen.getByText(/MOCK: Show me code/)).toBeTruthy();
  });

  it('a server older than client contract 1.8 reads the executor from the conversation alone', async () => {
    await open({ executor: 'cloud', contract: '1.4' });
    expect(await screen.findByLabelText(/^Message/)).toBeTruthy();
    expect(server.requests.some((r) => r.url.pathname.endsWith(HANDOFF))).toBe(false);
  });

  it('follows the handoff state over a stale conversation read', async () => {
    // The conversation still says local; the handoff read says the cloud has it.
    await open({ executor: 'local', state: 'CLOUD_RUNNING' });
    expect(await screen.findByLabelText(/^Message/)).toBeTruthy();
    expect(screen.getByText('Running in the cloud')).toBeTruthy();
  });

  it('polls the handoff state every 10 seconds while the screen is open', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await screen.findByText('Running in the cloud');
    const query = queries!.getQueryCache().find({ queryKey: queryKeys.handoff(P, UUID) });
    expect(HANDOFF_POLL_MS).toBe(10_000);
    expect(query?.observers[0].options.refetchInterval).toBe(HANDOFF_POLL_MS);
  });
});

describe('Take back and Erase permissions', () => {
  it('the owner sees both and Take back asks the laptop (a POST, no state change)', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Ask laptop to take back' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST' && r.url.pathname.endsWith(RECLAIM))).toBe(true));
    expect(screen.getByLabelText(/^Message/)).toBeTruthy();
    expect(await screen.findByText('Sent to your laptop')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Asked laptop' })).toBeTruthy();
  });

  it('shows a take back the laptop was already asked for', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_RUNNING', reclaimPending: true });
    expect(await screen.findByRole('button', { name: 'Asked laptop' })).toBeTruthy();
  });

  it('someone else without the settings permission gets neither action, but can still message', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', owner: false });
    await screen.findByText('Running in the cloud');
    expect(screen.getByLabelText(/^Message/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Ask laptop to take back' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Erase cloud copy' })).toBeNull();
  });

  it('a project settings editor who is not the owner can erase, but not take back', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', owner: false, settingsEditor: true });
    expect(await screen.findByRole('button', { name: 'Erase cloud copy' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Ask laptop to take back' })).toBeNull();
  });

  it('Erase asks first; Keep it sends nothing, Erase sends the DELETE', async () => {
    const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Erase cloud copy' }));
    expect(alert).toHaveBeenCalledTimes(1);
    expect(server.requests.some((r) => r.method === 'DELETE')).toBe(false);
    const buttons = alert.mock.calls[0][2] as { text: string; onPress?: () => void }[];
    expect(buttons.map((b) => b.text)).toEqual(['Keep it', 'Erase cloud copy']);

    await act(async () => buttons[1].onPress?.());
    await waitFor(() => expect(server.requests.some((r) => r.method === 'DELETE' && r.url.pathname.endsWith(HANDOFF))).toBe(true));
    expect(await screen.findByText('The cloud copy was erased')).toBeTruthy();
  });

  it('a returning thread keeps Erase within reach for its owner', async () => {
    await open({ executor: 'returning', state: 'RECLAIMING' });
    expect(await screen.findByRole('button', { name: 'Erase cloud copy' })).toBeTruthy();
  });
});

describe('Regenerate', () => {
  it('is offered for an answer the cloud ran', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.getByRole('button', { name: 'Regenerate answer' })).toBeTruthy();
  });

  it('is not offered when the newest answer ran on a desktop', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', answerRanOn: 'desktop' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.queryByRole('button', { name: 'Regenerate answer' })).toBeNull();
  });
});

describe('erase back to the laptop', () => {
  it('LOCAL replaces a cloud state even with a lower epoch; a stale cloud read does not move it back', () => {
    expect(mergeHandoff({ state: 'CLOUD_IDLE', epoch: 7 }, { state: 'LOCAL', epoch: 0 })).toEqual({ state: 'LOCAL', epoch: 0 });
    expect(mergeHandoff({ state: 'CLOUD_RUNNING', epoch: 7 }, { state: 'CLOUD_IDLE', epoch: 5 })).toEqual({ state: 'CLOUD_RUNNING', epoch: 7 });
    expect(mergeHandoff({ state: 'CLOUD_IDLE', epoch: 5 }, { state: 'RECLAIMING', epoch: 6 })).toEqual({ state: 'RECLAIMING', epoch: 6 });
    expect(mergeHandoff(undefined, { state: 'LOCAL', epoch: 0 })).toEqual({ state: 'LOCAL', epoch: 0 });
  });

  it('after Erase the thread is read-only again and stays so on the next poll (LOCAL, epoch 0)', async () => {
    const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Erase cloud copy' }));
    // The server now has no handoff row for the thread: LOCAL at epoch 0 (below the cached 3).
    server.on('GET', HANDOFF, { status: 200, body: { state: 'LOCAL', epoch: 0 } });
    const buttons = alert.mock.calls[0][2] as { text: string; onPress?: () => void }[];
    await act(async () => buttons[1].onPress?.());
    expect(await screen.findByText(/This thread runs on api-server on your laptop/)).toBeTruthy();
    await act(async () => {
      await queries!.refetchQueries({ queryKey: queryKeys.handoff(P, UUID) });
    });
    expect(screen.queryByLabelText(/^Message/)).toBeNull();
    expect(screen.queryByText('Running in the cloud')).toBeNull();
  });
});

describe('a thread the laptop runs', () => {
  it('offers no Regenerate (the composer is off)', async () => {
    await open({ executor: 'local', state: 'LOCAL' });
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.queryByRole('button', { name: 'Regenerate answer' })).toBeNull();
  });

  it('is not re-read for a conversation without workspace_executor, nor on every poll', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE' });
    await screen.findByText('Running in the cloud');
    const reads = () => server.requests.filter((r) => r.method === 'GET' && r.url.pathname.endsWith(DETAIL)).length;
    const before = reads();
    await act(async () => {
      await queries!.refetchQueries({ queryKey: queryKeys.handoff(P, UUID) });
      await queries!.refetchQueries({ queryKey: queryKeys.handoff(P, UUID) });
    });
    expect(reads()).toBe(before);
  });
});

describe('the push and pull-request credential (contract 1.29)', () => {
  it('the owner sees the chosen credential and opens the sheet that changes it', async () => {
    mockPush.mockClear();
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', contract: '1.29', credential: { type: 'gitlab', elitea_title: 'work', private: false } });
    await fireEvent.press(await screen.findByRole('button', { name: 'Pushes and pull requests: work (GitLab)' }));
    expect(mockPush).toHaveBeenCalledWith({ pathname: '/pr-credential', params: { uuid: UUID } });
  });

  it('offers a choice when none is set', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', contract: '1.29' });
    expect(await screen.findByRole('button', { name: 'Pushes and pull requests: choose a credential' })).toBeTruthy();
  });

  it('is not offered to someone else, nor by an older server', async () => {
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', contract: '1.29', owner: false });
    await screen.findByText('Running in the cloud');
    expect(screen.queryByTestId('cloud-pr-credential')).toBeNull();
    queries?.clear();
    await open({ executor: 'cloud', state: 'CLOUD_IDLE', contract: '1.28' });
    await screen.findByText('Running in the cloud');
    expect(screen.queryByTestId('cloud-pr-credential')).toBeNull();
  });
});
