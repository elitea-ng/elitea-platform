import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { AccessibilityInfo, ActionSheetIOS, type ActionSheetIOSOptions } from 'react-native';

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
// The scan's files on "disk": what the scanner wrote, and what the pipeline wrote or deleted.
jest.mock('expo-file-system', () => {
  const disk = new Map<string, Uint8Array>();
  const removedDirs: string[] = [];
  const join = (parts: unknown[]) =>
    parts.map((p) => (typeof p === 'string' ? p : (p as { uri: string }).uri).replace(/\/$/, '')).join('/');
  class File {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    get size() {
      return disk.get(this.uri)?.byteLength ?? 0;
    }
    get exists() {
      return disk.has(this.uri);
    }
    write(text: string) {
      disk.set(this.uri, new TextEncoder().encode(text));
    }
    delete() {
      disk.delete(this.uri);
    }
    open() {
      const bytes = disk.get(this.uri) ?? new Uint8Array();
      return { offset: 0, readBytes: async (n: number) => bytes.slice(0, n), close() {} };
    }
  }
  class Directory {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = `${join(parts)}/`;
    }
    get exists() {
      return true;
    }
    list() {
      return [];
    }
    create() {}
    delete() {
      removedDirs.push(this.uri);
      for (const key of [...disk.keys()]) if (key.startsWith(this.uri)) disk.delete(key);
    }
  }
  return { __disk: disk, __removedDirs: removedDirs, File, Directory, Paths: { cache: 'file:///cache', document: 'file:///docs' } };
});
const fs = jest.requireMock<{ __disk: Map<string, Uint8Array>; __removedDirs: string[] }>('expo-file-system');
jest.mock('@/features/chat/attachments/pickers', () => ({
  PickerPermissionError: class extends Error {},
  pickFiles: jest.fn(async () => []),
  pickPhotos: jest.fn(async () => []),
  takePhoto: jest.fn(async () => []),
  discardPickerCopy: (uri: string) => jest.requireMock<{ __disk: Map<string, Uint8Array> }>('expo-file-system').__disk.delete(uri),
}));

// The DocumentScanner inline module: three pages from the document camera, a PDF with the
// recognised text, and the text alone.
const SCAN_DIR = 'file:///cache/DocumentScan/3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b/';
// VisionKit's own scratch directory, emptied after every scan.
const VISIONKIT_DIR = 'file:///cache/com.apple.DocumentCamera/';
const PAGES = [1, 2, 3].map((n) => `${SCAN_DIR}page-${n}.jpg`);
const TEXT = ['Pump Station 7 - Field Inspection\nSerial number: ZF-4471', 'Findings\n3. Vibration: 4.2 mm/s', 'Costs and sign-off\nTotal: 1,284.50 CAD'];
const LAYOUT = '[[{"text":"Pump Station 7"}]]';
const mockScanner = {
  isSupported: true,
  scan: jest.fn(async (dir: string) => {
    const disk = jest.requireMock<{ __disk: Map<string, Uint8Array> }>('expo-file-system').__disk;
    const pages = [1, 2, 3].map((n) => `${dir}page-${n}.jpg`);
    for (const page of pages) disk.set(page, new Uint8Array([0xff, 0xd8, 0xff, 0xe0]));
    return pages;
  }),
  recognizeLayout: jest.fn(async () => ({ layout: LAYOUT, text: TEXT })),
  makePdf: jest.fn(async (_pages: string[], output: string) => {
    jest.requireMock<{ __disk: Map<string, Uint8Array> }>('expo-file-system').__disk.set(output, new TextEncoder().encode('%PDF-1.3 scan'));
    return { uri: output, size: 13 };
  }),
  recognizeText: jest.fn(async () => TEXT),
  // The text file is written natively (Complete file protection).
  writeText: jest.fn(async (output: string, text: string) => {
    const bytes = new TextEncoder().encode(text);
    jest.requireMock<{ __disk: Map<string, Uint8Array> }>('expo-file-system').__disk.set(output, bytes);
    return bytes.byteLength;
  }),
};
jest.mock('@/features/chat/attachments/scan/scanner', () => ({
  ...jest.requireActual('@/features/chat/attachments/scan/scanner'),
  documentScanner: () => mockScanner,
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

/** The attach menu answers "Scan document"; the format sheet answers `format`. */
function answerSheets(format: 'PDF (3 pages)' | 'Text only' | 'Cancel') {
  const sheets: ActionSheetIOSOptions[] = [];
  jest.spyOn(ActionSheetIOS, 'showActionSheetWithOptions').mockImplementation((options, callback) => {
    sheets.push(options);
    const wanted = sheets.length === 1 ? 'Scan document' : format;
    callback(options.options.indexOf(wanted));
  });
  return sheets;
}

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  fs.__disk.clear();
  fs.__removedDirs.splice(0);
  jest.clearAllMocks();
  mockParams = { id: '3' };
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

const SCAN_NAME = /^Scan \d{4}-\d{2}-\d{2} \d{2}\.\d{2}\.(pdf|txt)$/;

describe('document scan to attachment (#10)', () => {
  it('a three-page scan attaches as one PDF, uploaded before the message is sent', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/Scan-8000.pdf`, file_size: 13 }] });
    const sheets = answerSheets('PDF (3 pages)');
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/^Scan .*\.pdf, ready/)).toBeTruthy());
    // The menu offered the scan; the format sheet offered both, the text recognised on the device.
    expect(sheets[0].options).toEqual(['Photo library', 'Camera', 'Scan document', 'Files', 'Cancel']);
    expect(sheets[1]).toMatchObject({ title: 'Attach the scan as', options: ['PDF (3 pages)', 'Text only', 'Cancel'] });
    expect(mockScanner.scan).toHaveBeenCalledWith(SCAN_DIR);
    expect(mockScanner.recognizeLayout).toHaveBeenCalledWith(PAGES);
    expect(mockScanner.makePdf).toHaveBeenCalledWith(PAGES, `${SCAN_DIR}scan.pdf`, 2048, 0.7, LAYOUT);
    expect(fs.__removedDirs).toEqual([VISIONKIT_DIR]);
    // Only the PDF is left of the scan.
    expect([...fs.__disk.keys()]).toEqual([`${SCAN_DIR}scan.pdf`]);

    await fireEvent.changeText(screen.getByLabelText('Message'), 'What is the total on the last page?');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST' && r.url.pathname.endsWith(SEND))).toBe(true));
    const posts = server.requests.filter((r) => r.method === 'POST').map((r) => r.url.pathname.replace('/api/v2', ''));
    expect(posts).toEqual([UPLOAD, SEND]);
    const send = JSON.parse(server.requests.find((r) => r.url.pathname.endsWith(SEND))!.body!);
    expect(send.payload.user_input).toBe('What is the total on the last page?');
    expect(send.payload.attachments).toEqual([{ filepath: `/chat-attachments/${UUID}/Scan-8000.pdf`, name: expect.stringMatching(SCAN_NAME) }]);
    expect(send.payload.attachments[0].name).toMatch(/\.pdf$/);
  });

  it('OCR mode attaches the recognised text as a .txt file, never the images', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/Scan-8000.txt`, file_size: 100 }] });
    answerSheets('Text only');
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText(/^Scan .*\.txt, ready/)).toBeTruthy());
    expect(mockScanner.recognizeText).toHaveBeenCalledWith(PAGES);
    expect(mockScanner.makePdf).not.toHaveBeenCalled();
    expect(mockScanner.writeText).toHaveBeenCalledWith(`${SCAN_DIR}scan.txt`, expect.stringContaining('--- Page 1 of 3 ---'));
    expect([...fs.__disk.keys()]).toEqual([`${SCAN_DIR}scan.txt`]);
    const text = new TextDecoder().decode(fs.__disk.get(`${SCAN_DIR}scan.txt`));
    expect(text).toContain('--- Page 3 of 3 ---\n\nCosts and sign-off\nTotal: 1,284.50 CAD');

    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(server.requests.some((r) => r.url.pathname.endsWith(SEND))).toBe(true));
    const upload = server.requests.find((r) => r.url.pathname.endsWith(UPLOAD))!;
    // The file part is the text file on disk (React Native reads it natively), named .txt.
    expect(upload.body).toMatch(/name="file_name"\s+Scan_\d{4}-\d{2}-\d{2}_\d{2}\.\d{2}-[0-9a-f]{4}\.txt/);
    expect(JSON.parse(server.requests.find((r) => r.url.pathname.endsWith(SEND))!.body!).payload.attachments[0].name).toMatch(/\.txt$/);
  });

  it('a cancelled choice attaches nothing and deletes the scanned pages', async () => {
    installServer();
    answerSheets('Cancel');
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(fs.__removedDirs).toEqual([SCAN_DIR, VISIONKIT_DIR]));
    expect(fs.__disk.size).toBe(0);
    expect(screen.queryByLabelText(/^Scan /)).toBeNull();
  });

  it('while the scan is being prepared: a placeholder in the tray, announced, and no send or second scan until it lands (F2)', async () => {
    const server = installServer();
    server.on('POST', UPLOAD, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/Scan-8000.pdf`, file_size: 13 }] });
    const sheets = answerSheets('PDF (3 pages)');
    const announce = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    let finish: () => void = () => {};
    mockScanner.makePdf.mockImplementationOnce(
      (_pages: string[], output: string) =>
        new Promise((resolve) => {
          finish = () => {
            fs.__disk.set(output, new TextEncoder().encode('%PDF-1.3 scan'));
            resolve({ uri: output, size: 13 });
          };
        }),
    );
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    expect(await screen.findByLabelText('Scan, preparing')).toBeTruthy();
    expect(announce).toHaveBeenCalledWith('Preparing the scan…');
    // A send now would leave the scan behind: it is held, the text kept.
    await fireEvent.changeText(screen.getByLabelText('Message'), 'What is the total?');
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText('The scan is still being prepared. Send once it is in the tray.')).toBeTruthy();
    expect(server.requests.some((r) => r.url.pathname.endsWith(SEND))).toBe(false);
    // A second scan does not start alongside.
    await fireEvent.press(screen.getByRole('button', { name: 'Attach files' }));
    expect(sheets).toHaveLength(2);
    expect(mockScanner.scan).toHaveBeenCalledTimes(1);

    finish();
    await waitFor(() => expect(screen.getByLabelText(/^Scan .*\.pdf, ready/)).toBeTruthy());
    expect(screen.queryByLabelText('Scan, preparing')).toBeNull();
    await fireEvent.press(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(server.requests.some((r) => r.url.pathname.endsWith(SEND))).toBe(true));
    expect(JSON.parse(server.requests.find((r) => r.url.pathname.endsWith(SEND))!.body!).payload.attachments).toHaveLength(1);
  });

  it('a full tray refuses the scan before the camera opens (F1)', async () => {
    installServer();
    const pickers = jest.requireMock<{ pickFiles: jest.Mock }>('@/features/chat/attachments/pickers');
    const notes = Array.from({ length: 10 }, (_, i) => ({ uri: `file:///cache/DocumentPicker/n${i}.txt`, name: `n${i}.txt`, size: 4, mimeType: 'text/plain' }));
    for (const n of notes) fs.__disk.set(n.uri, new TextEncoder().encode('note'));
    pickers.pickFiles.mockResolvedValueOnce(notes);
    const answers = ['Files', 'Scan document'];
    jest.spyOn(ActionSheetIOS, 'showActionSheetWithOptions').mockImplementation((options, callback) => callback(options.options.indexOf(answers.shift() ?? 'Cancel')));
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    await waitFor(() => expect(screen.getByLabelText('n9.txt, ready')).toBeTruthy());
    await fireEvent.press(screen.getByRole('button', { name: 'Attach files' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('A message can carry at most 10 files.');
    expect(mockScanner.scan).not.toHaveBeenCalled();
  });

  it('a scan that fails says so and leaves nothing behind', async () => {
    installServer();
    answerSheets('PDF (3 pages)');
    mockScanner.scan.mockRejectedValueOnce(new Error('camera failed'));
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Attach files' }));
    expect(await screen.findByRole('alert')).toHaveTextContent("The scan couldn't be saved. Try again.");
    expect(fs.__removedDirs).toEqual([SCAN_DIR, VISIONKIT_DIR]);
  });
});
