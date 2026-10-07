/**
 * #29: a Stop before any output makes the server delete the question
 * (cancelChatExecution); the files that went with it come back to the tray
 * for a one-tap resend instead of being named only. Shapes: the captured
 * cancelChatExecution 204 and the first frame of cancelled.sse (zefir-wave,
 * main 22403f640); the send answer as stop.test.ts.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { ActionSheetIOS } from 'react-native';

import { questionGroup } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ThreadScreen from '@/app/(app)/c/[id]';
import { loadFixtureText } from '@/features/chat/__fixtures__/loadFixture';
import { clearLiveTurns } from '@/features/chat/turnController';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

let mockParams: Record<string, string> = {};
jest.mock('expo-router', () => ({
  router: { replace: jest.fn(), push: jest.fn() },
  useLocalSearchParams: () => mockParams,
  Stack: { Screen: () => null },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const mockDeleted: string[] = [];
let mockStream = fakeStreamTransport();
jest.mock('@/api/sse/transport', () => ({
  expoStreamTransport: (url: string, init: unknown) => mockStream.transport(url, init as never),
}));
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));
// The picked file on "disk": a small markdown document.
jest.mock('expo-file-system', () => {
  const bytes = new TextEncoder().encode('# notes\nhello');
  class File {
    uri: string;
    constructor(uri: string) {
      this.uri = uri;
    }
    get size() {
      return bytes.byteLength;
    }
    get exists() {
      return true;
    }
    delete() {
      mockDeleted.push(this.uri);
    }
    open() {
      return { offset: 0, readBytes: async (n: number) => bytes.slice(0, n), close() {} };
    }
  }
  class Directory {
    exists = false;
    list() {
      return [];
    }
    create() {}
    delete() {}
  }
  return { File, Directory, Paths: { cache: 'file:///cache', document: 'file:///docs' } };
});
// The outbox: a send queued (offline, or behind an unsent message) or handed
// off (workspace unreachable) is only recorded here.
let mockQueueSends = false;
const mockQueued: unknown[] = [];
jest.mock('@/sync/appOutbox', () => ({
  ...jest.requireActual('@/sync/appOutbox'),
  shouldQueue: () => mockQueueSends,
  queueSend: async (prepared: unknown) => {
    mockQueued.push(prepared);
  },
  handOffToOutbox: async (prepared: unknown) => {
    mockQueued.push(prepared);
    return true;
  },
}));
const mockPickFiles = jest.fn();
jest.mock('@/features/chat/attachments/pickers', () => ({
  PickerPermissionError: class extends Error {},
  pickFiles: (...args: unknown[]) => mockPickFiles(...args),
  pickPhotos: jest.fn(async () => []),
  takePhoto: jest.fn(async () => []),
  // The picker's copy of the file: what a release must delete and a restore must keep.
  discardPickerCopy: (uri: string) => mockDeleted.push(uri),
}));

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const MODEL = 'vllm/E2E-MOCK-MODEL';
const Q = '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b';
const ANSWER = 'aa5ea578-4aae-55a8-a361-08dae0136a9e';
const UPLOAD = `/elitea_core/attachments/prompt_lib/${P}/${UUID}`;
const SEND = `/elitea_core/messages/prompt_lib/${P}/${UUID}`;
const STOP = `/elitea_core/task/prompt_lib/${P}/${ANSWER}`;
const PICKED = 'file:///picked/notes.md';

const conversation = (groups: unknown[]) => ({
  id: '3',
  uuid: UUID,
  project_id: String(P),
  name: 'files',
  created_by: '7',
  is_private: true,
  message_count: groups.length,
  meta: {},
  created_at: '2026-10-05T07:00:00Z',
  updated_at: '2026-10-05T07:00:00Z',
  participants: [
    { id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} },
    { id: 4, entity_name: 'dummy', entity_meta: {}, entity_settings: { llm_settings: { model_name: MODEL, model_project_id: P } }, meta: { name: 'EliteA' } },
  ],
  message_groups: groups,
});

/** `afterStop`: the transcript the stop's refetch reads (no question: the server removed it). */
function installServer(afterStop: unknown[]): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: conversation([]) }, { status: 200, body: conversation(afterStop) });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/notes-8000.md`, file_size: 13 }] });
  server.on('POST', SEND, {
    status: 200,
    body: { execution_id: 'e1', events_url: `/api/v2/executions/${P}/e1/events`, response_message_id: ANSWER, created: true },
  });
  server.on('DELETE', STOP, { status: 204 });
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  mockDeleted.length = 0;
  mockQueueSends = false;
  mockQueued.length = 0;
  mockStream = fakeStreamTransport();
  mockParams = { id: '3' };
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13()) });
  mockPickFiles.mockResolvedValue([{ uri: PICKED, name: 'notes.md', size: 13, mimeType: 'text/markdown' }]);
  jest.spyOn(ActionSheetIOS, 'showActionSheetWithOptions').mockImplementation((_options, callback) => callback(2));
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

async function sendWithNotesThenStop(server: FakeServer) {
  await renderWithQueries(<ThreadScreen />);
  await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
  await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
  await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
  await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(mockStream.connections).toHaveLength(1));
  // Sent: the file left the tray, but its copy is kept until the answer settles.
  await waitFor(() => expect(screen.queryByLabelText(/notes\.md, /)).toBeNull());
  expect(mockDeleted).not.toContain(PICKED);
  mockStream.connections[0].push(
    `id: 90\nevent: execution.node_event\ndata: ${JSON.stringify({ type: 'agent_start', stream_id: UUID, message_id: ANSWER, question_id: null, content: null })}\n\n`,
  );
  await fireEvent.press(await screen.findByRole('button', { name: 'Stop answer' }));
  await waitFor(() => expect(server.requests.some((r) => r.method === 'DELETE' && r.url.pathname.endsWith(STOP))).toBe(true));
}

describe('Stop before any output with files (#29)', () => {
  it('puts the removed message and its file back; Send uploads the file again', async () => {
    const server = installServer([]);
    await sendWithNotesThenStop(server);
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Stopped before an answer. Your message and its files are back here to edit or send again.'));
    expect(screen.getByLabelText('Message').props.value).toBe('summarise this');
    expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy();
    expect(mockDeleted).not.toContain(PICKED);
  });

  it('the screen was left and reopened while it streamed (the tray let go of the file): the note names it to attach again', async () => {
    const server = installServer([]);
    const first = await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
    await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(mockStream.connections).toHaveLength(1));
    mockStream.connections[0].push(
      `id: 90\nevent: execution.node_event\ndata: ${JSON.stringify({ type: 'agent_start', stream_id: UUID, message_id: ANSWER, question_id: null, content: null })}\n\n`,
    );
    await waitFor(() => expect(screen.getByRole('button', { name: 'Stop answer' })).toBeTruthy());
    // Back to the list: the thread's tray is cleared (its copies deleted); the turn keeps running.
    first.unmount();
    await waitFor(() => expect(mockDeleted).toContain(PICKED));
    render(<QueryClientProvider client={queries as QueryClient}><ThreadScreen /></QueryClientProvider>);
    await waitFor(() => expect(mockStream.connections.length).toBeGreaterThanOrEqual(2));
    mockStream.connections[mockStream.connections.length - 1].push(
      `id: 90\nevent: execution.node_event\ndata: ${JSON.stringify({ type: 'agent_start', stream_id: UUID, message_id: ANSWER, question_id: null, content: null })}\n\n`,
    );
    await fireEvent.press(await screen.findByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'DELETE' && r.url.pathname.endsWith(STOP))).toBe(true));
    // Nothing came back to the tray, so the file must be named, not silently dropped.
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('Stopped before an answer. Your message is back here; attach notes.md again to send it.'),
    );
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
  });

  it('a retried send (the failure let go of the file): Stop before output names it to attach again', async () => {
    // The first events stream is refused (the turn fails, retryable); Retry re-sends the same body.
    mockStream = fakeStreamTransport((index) => (index === 0 ? { status: 404 } : {}));
    const server = installServer([]);
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
    await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    // Failed: the question is kept on the server, so the held copy is deleted.
    await fireEvent.press(await screen.findByRole('button', { name: 'Retry' }));
    expect(mockDeleted).toContain(PICKED);
    await waitFor(() => expect(mockStream.connections).toHaveLength(2));
    mockStream.connections[1].push(
      `id: 90\nevent: execution.node_event\ndata: ${JSON.stringify({ type: 'agent_start', stream_id: UUID, message_id: ANSWER, question_id: null, content: null })}\n\n`,
    );
    await fireEvent.press(await screen.findByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'DELETE' && r.url.pathname.endsWith(STOP))).toBe(true));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('Stopped before an answer. Your message is back here; attach notes.md again to send it.'),
    );
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
  });

  it('stopped elsewhere (another device): the turn ends stopped with no Stop of ours, and the held copy is deleted (SEC-1)', async () => {
    installServer([{ ...questionGroup, uuid: Q, content: 'summarise this' }]);
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
    await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(mockStream.connections).toHaveLength(1));
    expect(mockDeleted).not.toContain(PICKED);
    // The transcript read after the end hangs (a slow network): the turn stays
    // done and stopped, not yet settled or dismissed.
    const answer = globalThis.fetch;
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) =>
      new Request(input, init).url.includes('/elitea_core/conversation/') ? new Promise<Response>(() => {}) : answer(input, init)) as typeof fetch;
    // The captured stream of an answer stopped with DELETE: it ends with execution.failed CANCELLED.
    mockStream.connections[0].push(loadFixtureText('cancelled'));
    await waitFor(() => expect(screen.getByText('Stopped.')).toBeTruthy());
    await waitFor(() => expect(mockDeleted).toContain(PICKED));
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
  });

  it('a question the server kept (output was written): the file copy is deleted, nothing comes back', async () => {
    const server = installServer([{ ...questionGroup, uuid: Q, content: 'summarise this' }]);
    await sendWithNotesThenStop(server);
    await waitFor(() => expect(mockDeleted).toContain(PICKED));
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
    expect(screen.queryByText(/Stopped before an answer/)).toBeNull();
  });
});

describe('Files of a send that went to the outbox (F3)', () => {
  async function sendWithNotes() {
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
    await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
  }

  it('a queued send: the uploaded file\'s local copy is deleted at once (nothing can be stopped)', async () => {
    installServer([]);
    mockQueueSends = true;
    await sendWithNotes();
    await waitFor(() => expect(mockQueued).toHaveLength(1));
    await waitFor(() => expect(mockDeleted).toContain(PICKED));
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
  });

  it('a send handed off to the outbox (workspace unreachable): the held copy is deleted', async () => {
    const server = installServer([]);
    server.on('POST', SEND, 'network_error');
    await sendWithNotes();
    await waitFor(() => expect(mockQueued).toHaveLength(1));
    await waitFor(() => expect(mockDeleted).toContain(PICKED));
    expect(screen.queryByLabelText(/notes\.md, /)).toBeNull();
  });
});
