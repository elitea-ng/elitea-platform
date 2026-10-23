import { attachmentLimits, DEFAULT_ATTACHMENT_POLICY } from '@/features/chat/attachments/prepare';
import { NO_SCAN_FORMAT, runScan, type ScanFlowDeps } from '@/features/chat/attachments/scan/scanFlow';
import type { DocumentScannerNative } from '@/features/chat/attachments/scan/scanner';

const DIR = 'file:///app/Library/Caches/DocumentScan/6f1c2b9e/';
const PAGES = [`${DIR}page-1.jpg`, `${DIR}page-2.jpg`, `${DIR}page-3.jpg`];
const limits = attachmentLimits(DEFAULT_ATTACHMENT_POLICY);

function setup(overrides: Partial<ScanFlowDeps> = {}, native: Partial<DocumentScannerNative> = {}) {
  const events: string[] = [];
  const scanner: DocumentScannerNative = {
    isSupported: true,
    scan: jest.fn(async (dir: string) => {
      events.push(`scan ${dir}`);
      return PAGES;
    }),
    recognizeLayout: jest.fn(async () => ({ layout: '[]', text: ['Pump Station 7'] })),
    makePdf: jest.fn(async (_pages: string[], output: string) => ({ uri: output, size: 1234 })),
    recognizeText: jest.fn(async () => ['Pump Station 7']),
    writeText: jest.fn(async (_uri: string, text: string) => text.length),
    ...native,
  };
  const deps: ScanFlowDeps = {
    source: 'camera',
    native: scanner,
    limits,
    makeDirectory: () => ({ uri: DIR, remove: () => events.push('remove dir') }),
    chooseFormat: jest.fn(async () => 'pdf' as const),
    remove: (uri) => events.push(`remove ${uri.slice(DIR.length)}`),
    isCurrent: () => true,
    now: () => new Date(2026, 9, 8, 9, 30),
    ...overrides,
  };
  return { deps, events, scanner };
}

it('camera → choice → PDF: the PDF is kept for the tray, the pages are deleted', async () => {
  const { deps, events } = setup();
  const outcome = await runScan(deps);
  expect(outcome).toEqual({
    kind: 'files',
    files: [{ uri: `${DIR}scan.pdf`, name: 'Scan 2026-10-08 09.30.pdf', size: 1234, mimeType: 'application/pdf', display: { title: 'Scan 09.30', detail: expect.any(String) } }],
    warnings: [],
  });
  expect(deps.chooseFormat).toHaveBeenCalledWith(['pdf', 'text'], 3);
  expect(events).toEqual([`scan ${DIR}`, 'remove page-1.jpg', 'remove page-2.jpg', 'remove page-3.jpg']);
});

it('the development sample pages take the same pipeline', async () => {
  const samplePages = jest.fn(async () => PAGES);
  const { deps, scanner } = setup({ source: 'samples', samplePages, chooseFormat: jest.fn(async () => 'text' as const) });
  const outcome = await runScan(deps);
  expect(samplePages).toHaveBeenCalledWith(DIR);
  expect(scanner.scan).not.toHaveBeenCalled();
  expect(scanner.recognizeText).toHaveBeenCalledWith(PAGES);
  expect(outcome).toMatchObject({ kind: 'files', files: [{ name: 'Scan 2026-10-08 09.30.txt', mimeType: 'text/plain' }] });
});

it('asks nothing when the workspace takes one of the two', async () => {
  const { deps } = setup({ limits: { ...limits, acceptedExtensions: ['.txt'] } });
  expect(await runScan(deps)).toMatchObject({ kind: 'files', files: [{ mimeType: 'text/plain' }] });
  expect(deps.chooseFormat).not.toHaveBeenCalled();
});

it('refuses before opening the camera when the workspace takes neither', async () => {
  const { deps, scanner, events } = setup({ limits: { ...limits, acceptedExtensions: ['.png'] } });
  expect(await runScan(deps)).toEqual({ kind: 'refused', message: NO_SCAN_FORMAT });
  expect(scanner.scan).not.toHaveBeenCalled();
  expect(events).toEqual([]);
});

it('a cancelled scan or a cancelled choice deletes the whole scan', async () => {
  const cancelledScan = setup({}, { scan: jest.fn(async () => []) });
  expect(await runScan(cancelledScan.deps)).toEqual({ kind: 'cancelled' });
  expect(cancelledScan.events).toEqual(['remove dir']);

  const cancelledChoice = setup({ chooseFormat: jest.fn(async () => null) });
  expect(await runScan(cancelledChoice.deps)).toEqual({ kind: 'cancelled' });
  expect(cancelledChoice.events).toEqual([`scan ${DIR}`, 'remove dir']);
});

it('a scan that finishes after its screen went is thrown away', async () => {
  let current = true;
  const { deps, events } = setup({
    isCurrent: () => current,
    chooseFormat: jest.fn(async () => {
      current = false;
      return 'pdf' as const;
    }),
  });
  expect(await runScan(deps)).toEqual({ kind: 'cancelled' });
  expect(events).toEqual([`scan ${DIR}`, 'remove dir']);
});

it('a pipeline refusal is said, and the scan is deleted', async () => {
  const { deps, events } = setup({}, { makePdf: jest.fn(async (_p: string[], output: string) => ({ uri: output, size: 999 * 1024 * 1024 })) });
  const outcome = await runScan(deps);
  expect(outcome).toEqual({ kind: 'refused', message: 'The scan is larger than 50 MB. Scan fewer pages.' });
  expect(events.at(-1)).toBe('remove dir');
});

it('a native failure propagates (the screen says the scan could not be saved), the scan deleted', async () => {
  const { deps, events } = setup({}, { scan: jest.fn(async () => Promise.reject(new Error('camera failed'))) });
  await expect(runScan(deps)).rejects.toThrow('camera failed');
  expect(events).toEqual(['remove dir']);
});

it('refuses before opening the camera when the message already carries the most files (F1)', async () => {
  const { deps, scanner, events } = setup({ held: () => ({ count: limits.maxFiles, totalBytes: 0 }) });
  expect(await runScan(deps)).toEqual({ kind: 'refused', message: `A message can carry at most ${limits.maxFiles} files.` });
  expect(scanner.scan).not.toHaveBeenCalled();
  expect(events).toEqual([]);
});

it('refuses before opening the camera when the message has no room left', async () => {
  const { deps, scanner } = setup({ held: () => ({ count: 1, totalBytes: limits.maxTotalBytes }) });
  expect(await runScan(deps)).toMatchObject({ kind: 'refused', message: expect.stringContaining('together') });
  expect(scanner.scan).not.toHaveBeenCalled();
});

it('fits the PDF to what the tray holds when the scan is built, not when it was opened', async () => {
  let held = { count: 0, totalBytes: 0 };
  const sizes = [4000, 1500];
  const makePdf = jest.fn(async (_p: string[], output: string) => ({ uri: output, size: sizes.shift() ?? 1500 }));
  const { deps } = setup(
    {
      limits: { ...limits, maxTotalBytes: 5000 },
      held: () => held,
      chooseFormat: jest.fn(async () => {
        held = { count: 1, totalBytes: 2000 };
        return 'pdf' as const;
      }),
    },
    { makePdf },
  );
  expect(await runScan(deps)).toMatchObject({ kind: 'files', files: [{ size: 1500 }] });
  expect(makePdf).toHaveBeenCalledTimes(2);
});

it('says when the slow part starts: after the camera and the format choice, before the file is built (F2)', async () => {
  const onPreparing = jest.fn();
  const { deps, scanner } = setup({ onPreparing });
  (scanner.makePdf as jest.Mock).mockImplementationOnce(async (_p: string[], output: string) => {
    expect(onPreparing).toHaveBeenCalledTimes(1);
    return { uri: output, size: 10 };
  });
  await runScan(deps);
  expect(deps.chooseFormat).toHaveBeenCalled();
  expect(onPreparing).toHaveBeenCalledTimes(1);

  const cancelled = setup({ onPreparing: jest.fn(), chooseFormat: jest.fn(async () => null) });
  await runScan(cancelled.deps);
  expect(cancelled.deps.onPreparing).not.toHaveBeenCalled();
});
