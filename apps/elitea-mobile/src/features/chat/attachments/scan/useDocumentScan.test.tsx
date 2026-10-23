import { act, renderHook } from '@testing-library/react-native';

import { attachmentLimits, DEFAULT_ATTACHMENT_POLICY } from '@/features/chat/attachments/prepare';
import type { ScanOutcome } from '@/features/chat/attachments/scan/scanFlow';
import { useDocumentScan } from '@/features/chat/attachments/scan/useDocumentScan';

jest.mock('expo-crypto', () => {
  let n = 0;
  return { randomUUID: () => `scan-${++n}` };
});
// Directories deleted, in order; files are not tracked here.
jest.mock('expo-file-system', () => {
  const removedDirs: string[] = [];
  const join = (parts: unknown[]) => parts.map((p) => (typeof p === 'string' ? p : (p as { uri: string }).uri).replace(/\/$/, '')).join('/');
  class File {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = join(parts);
    }
    exists = false;
    size = 0;
    write() {}
    delete() {}
  }
  class Directory {
    uri: string;
    constructor(...parts: unknown[]) {
      this.uri = `${join(parts)}/`;
    }
    exists = true;
    create() {}
    delete() {
      removedDirs.push(this.uri);
    }
  }
  return { __removedDirs: removedDirs, File, Directory, Paths: { cache: 'file:///cache' } };
});
const fs = jest.requireMock<{ __removedDirs: string[] }>('expo-file-system');
jest.mock('expo-image-picker', () => ({}));
jest.mock('expo-document-picker', () => ({}));

const VISIONKIT_DIR = 'file:///cache/com.apple.DocumentCamera/';
const gates: (() => void)[] = [];
const mockScanner = {
  isSupported: true,
  scan: jest.fn(async (dir: string) => [`${dir}page-1.jpg`]),
  // Each PDF waits until the test lets it finish.
  makePdf: jest.fn(
    (_pages: string[], output: string) =>
      new Promise<{ uri: string; size: number }>((resolve) => gates.push(() => resolve({ uri: output, size: 10 }))),
  ),
  recognizeLayout: jest.fn(async () => ({ layout: '[]', text: ['x'] })),
  recognizeText: jest.fn(async () => ['x']),
  writeText: jest.fn(async (_uri: string, text: string) => text.length),
};
jest.mock('@/features/chat/attachments/scan/scanner', () => ({
  ...jest.requireActual('@/features/chat/attachments/scan/scanner'),
  documentScanner: () => mockScanner,
}));

const limits = attachmentLimits(DEFAULT_ATTACHMENT_POLICY);
const pdfOnly = { chooseFormat: async () => 'pdf' as const };
const flush = async () => {
  for (let i = 0; i < 5; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

beforeEach(() => {
  fs.__removedDirs.splice(0);
  gates.splice(0);
  jest.clearAllMocks();
});

it('one scan at a time: a second scan while the first is being prepared is refused, and the camera scratch is kept until it ends (F3)', async () => {
  const { result } = await renderHook(() => useDocumentScan(limits, pdfOnly));
  let first: Promise<ScanOutcome> = Promise.resolve({ kind: 'cancelled' });
  await act(async () => {
    first = result.current.scan();
    await flush();
  });
  expect(mockScanner.makePdf).toHaveBeenCalledTimes(1);
  let second: ScanOutcome | null = null;
  await act(async () => {
    second = await result.current.scan();
  });
  expect(second).toEqual({ kind: 'refused', message: 'A scan is already being prepared.' });
  expect(mockScanner.scan).toHaveBeenCalledTimes(1);
  // Nothing under a camera that might be live was deleted.
  expect(fs.__removedDirs).not.toContain(VISIONKIT_DIR);

  await act(async () => {
    gates[0]();
    await first;
  });
  expect(await first).toMatchObject({ kind: 'files' });
  expect(fs.__removedDirs).toEqual([VISIONKIT_DIR]);

  // Free again once it ended.
  let third: Promise<ScanOutcome> = Promise.resolve({ kind: 'cancelled' });
  await act(async () => {
    third = result.current.scan();
    await flush();
  });
  expect(mockScanner.scan).toHaveBeenCalledTimes(2);
  await act(async () => {
    gates[1]();
    await third;
  });
});

it('the guard is shared by every screen: a scan from another thread waits its turn too', async () => {
  const a = await renderHook(() => useDocumentScan(limits, pdfOnly));
  const b = await renderHook(() => useDocumentScan(limits, pdfOnly));
  let first: Promise<ScanOutcome> = Promise.resolve({ kind: 'cancelled' });
  await act(async () => {
    first = a.result.current.scan();
    await flush();
  });
  let other: ScanOutcome | null = null;
  await act(async () => {
    other = await b.result.current.scan();
  });
  expect(other).toEqual({ kind: 'refused', message: 'A scan is already being prepared.' });
  await act(async () => {
    gates[0]();
    await first;
  });
});
