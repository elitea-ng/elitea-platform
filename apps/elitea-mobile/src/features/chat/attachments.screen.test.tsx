import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { ActionSheetIOS } from 'react-native';

import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

let mockParams: Record<string, string> = {};
jest.mock('expo-router', () => ({
  router: { replace: jest.fn(), push: jest.fn() },
  useLocalSearchParams: () => mockParams,
  Stack: { Screen: () => null },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
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
    delete() {}
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
const mockPickFiles = jest.fn();
jest.mock('@/features/chat/attachments/pickers', () => ({
  PickerPermissionError: class extends Error {},
  pickFiles: (...args: unknown[]) => mockPickFiles(...args),
  pickPhotos: jest.fn(async () => []),
  takePhoto: jest.fn(async () => []),
}));

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const MODEL = 'vllm/E2E-MOCK-MODEL';
const UPLOAD = `/elitea_core/attachments/prompt_lib/${P}/${UUID}`;
const SEND = `/elitea_core/messages/prompt_lib/${P}/${UUID}`;

function installServer(): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, {
    status: 200,
    body: {
      id: '3',
      uuid: UUID,
      project_id: String(P),
      name: 'files',
      created_by: '7',
      is_private: true,
      message_count: 0,
      meta: {},
      created_at: '2026-10-05T07:00:00Z',
      updated_at: '2026-10-05T07:00:00Z',
      participants: [
        { id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} },
        { id: 4, entity_name: 'dummy', entity_meta: {}, entity_settings: { llm_settings: { model_name: MODEL, model_project_id: P } }, meta: { name: 'EliteA' } },
      ],
      message_groups: [],
    },
  });
  // Refused, so no stream is opened; the request is what these tests check.
  server.on('POST', SEND, { status: 422, body: { error: 'rejected_for_test' } });
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  mockParams = { id: '3' };
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  mockPickFiles.mockResolvedValue([{ uri: 'file:///picked/notes.md', name: 'notes.md', size: 13, mimeType: 'text/markdown' }]);
  // The action sheet answers "Files".
  jest.spyOn(ActionSheetIOS, 'showActionSheetWithOptions').mockImplementation((_options, callback) => callback(2));
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

async function attachNotes() {
  await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
  await waitFor(() => expect(screen.getByLabelText(/notes\.md, ready/)).toBeTruthy());
}

describe('attachments in a model chat', () => {
  it('uploads the picked file first, then sends with payload.attachments', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/notes-8000.md`, file_size: 13 }] });
    await renderWithQueries(<ThreadScreen />);
    await attachNotes();
    await fireEvent.changeText(screen.getByLabelText('Message'), 'summarise this');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST' && r.url.pathname.endsWith(SEND))).toBe(true));
    const posts = server.requests.filter((r) => r.method === 'POST').map((r) => r.url.pathname.replace('/api/v2', ''));
    expect(posts).toEqual([UPLOAD, SEND]);
    const send = server.requests.find((r) => r.url.pathname.endsWith(SEND))!;
    expect(JSON.parse(send.body!)).toMatchObject({
      payload: { user_input: 'summarise this', attachments: [{ filepath: `/chat-attachments/${UUID}/notes-8000.md`, name: 'notes.md' }] },
    });
  });

  it('sends files without text as "(attachment)"', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/notes-8000.md`, file_size: 13 }] });
    await renderWithQueries(<ThreadScreen />);
    await attachNotes();
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(server.requests.some((r) => r.url.pathname.endsWith(SEND))).toBe(true));
    expect(JSON.parse(server.requests.find((r) => r.url.pathname.endsWith(SEND))!.body!).payload.user_input).toBe('(attachment)');
  });

  it('a refused upload sends nothing and keeps the text and the file', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 400, body: { error: 'File size exceeds limit of 1 MB' } });
    await renderWithQueries(<ThreadScreen />);
    await attachNotes();
    await fireEvent.changeText(screen.getByLabelText('Message'), 'keep me');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('notes.md: File size exceeds limit of 1 MB');
    expect(server.requests.some((r) => r.url.pathname.endsWith(SEND))).toBe(false);
    expect(screen.getByLabelText('Message').props.value).toBe('keep me');
    expect(screen.getByLabelText(/notes\.md, failed/)).toBeTruthy();
  });
});
