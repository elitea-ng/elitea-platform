import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import * as Clipboard from 'expo-clipboard';
import * as Speech from 'expo-speech';
import type { ReactElement } from 'react';
import { Share } from 'react-native';

import { CAPTURE, conversationDetail13, feedbackDisliked, feedbackEmpty } from '@/api/__fixtures__/chat13';
import { discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { resetPolicy, setPolicy, STRICT_DEFAULTS } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';
import { copyableTexts, selectableTexts } from '@/test/selectable';

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: () => null }, router: { replace: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-clipboard', () => ({ setStringAsync: jest.fn(async () => true) }));

const P = CAPTURE.projectId;
const FEEDBACK = `/elitea_core/message_feedback/prompt_lib/${P}/${CAPTURE.answerUuid}`;

function installServer(): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: conversationDetail13 });
  server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

function workspace(contract: '1.1' | '1.3') {
  const doc = contract === '1.3' ? discoveryDocument13() : discoveryDocument11();
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(doc) });
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  (Clipboard.setStringAsync as jest.Mock).mockClear();
});
afterEach(() => {
  queries?.clear();
  queries = null;
  jest.restoreAllMocks();
});

describe('message actions on a captured answer', () => {
  it('copies the answer and its code block, and shares it', async () => {
    installServer();
    workspace('1.3');
    const share = jest.spyOn(Share, 'share').mockResolvedValue({ action: 'sharedAction', activityType: undefined });
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    await fireEvent.press(screen.getByRole('button', { name: 'Copy python code' }));
    await waitFor(() => expect(Clipboard.setStringAsync).toHaveBeenCalledWith("print('hi')"));
    expect(await screen.findByText('Code copied.')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Copy' }));
    await waitFor(() => expect(Clipboard.setStringAsync).toHaveBeenLastCalledWith(conversationDetail13.message_groups[0].content));
    await fireEvent.press(screen.getByRole('button', { name: 'Share…' }));
    await waitFor(() => expect(share).toHaveBeenCalledWith({ message: conversationDetail13.message_groups[0].content }));
  });

  it('allow_share_out off: no copy or share, the sheet says the workspace does not allow it', async () => {
    installServer();
    workspace('1.3');
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.getByText(/does not allow copying or sharing/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Copy' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Share…' })).toBeNull();
    expect(screen.queryByRole('button', { name: /Copy python code/ })).toBeNull();
  });

  it('allow_share_out on: the transcript text is selectable', async () => {
    installServer();
    workspace('1.3');
    await renderWithQueries(<ThreadScreen />);
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(selectableTexts(screen.toJSON()).join('\n')).toContain('MOCK: Show me code');
  });

  it("allow_share_out off: no transcript text is selectable, so the system Copy/Share menu can't take it either", async () => {
    installServer();
    workspace('1.3');
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
    await renderWithQueries(<ThreadScreen />);
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(selectableTexts(screen.toJSON())).toEqual([]);
  });

  it('a dislike with a comment is sent to the workspace (1.3)', async () => {
    const server = installServer();
    workspace('1.3');
    server.on('GET', FEEDBACK, { status: 200, body: feedbackEmpty });
    server.on('POST', FEEDBACK, { status: 200, body: feedbackDisliked });
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Bad answer' })).not.toBeDisabled());
    await fireEvent.press(screen.getByRole('button', { name: 'Bad answer' }));
    await fireEvent.changeText(screen.getByLabelText('Feedback comment'), 'too short');
    await fireEvent.press(screen.getByRole('button', { name: 'Send feedback' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST')).toBe(true));
    expect(JSON.parse(server.requests.find((r) => r.method === 'POST')!.body!)).toEqual({ rating: -1, comment: 'too short' });
    expect(await screen.findByText(/Thanks/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Bad answer' })).toBeSelected();
  });

  it('allow_share_out off: the feedback comment field offers no system Copy/Share menu', async () => {
    const server = installServer();
    workspace('1.3');
    setPolicy({ ...STRICT_DEFAULTS, allow_share_out: false }, 'server');
    server.on('GET', FEEDBACK, { status: 200, body: feedbackEmpty });
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Bad answer' })).not.toBeDisabled());
    await fireEvent.press(screen.getByRole('button', { name: 'Bad answer' }));
    await fireEvent.changeText(screen.getByLabelText('Feedback comment'), 'too short');
    expect(screen.getByLabelText('Feedback comment').props.contextMenuHidden).toBe(true);
    expect(copyableTexts(screen.toJSON())).toEqual([]);
  });

  it('before client contract 1.3 there is no feedback (and no request for it)', async () => {
    const server = installServer();
    workspace('1.1');
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.getByRole('button', { name: 'Copy' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Good answer' })).toBeNull();
    expect(server.requests.some((r) => r.url.pathname.includes('message_feedback'))).toBe(false);
  });

  it('reads the answer aloud on the device, and stops it', async () => {
    installServer();
    workspace('1.3');
    (Speech.speak as jest.Mock).mockClear();
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    await fireEvent.press(screen.getByRole('button', { name: 'Read aloud' }));
    expect(Speech.speak).toHaveBeenCalledWith('MOCK: Show me code:\n Code block. \nthanks', expect.any(Object));
    await fireEvent.press(await screen.findByRole('button', { name: 'Stop reading aloud' }));
    expect(Speech.stop).toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop reading aloud' })).toBeNull());
  });

  it('offers on-device dictation in the composer', async () => {
    installServer();
    workspace('1.3');
    await renderWithQueries(<ThreadScreen />);
    expect(await screen.findByRole('button', { name: 'Dictate' })).toBeTruthy();
  });
});
