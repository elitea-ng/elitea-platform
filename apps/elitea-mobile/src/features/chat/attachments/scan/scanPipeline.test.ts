import { PICKER_CACHE_DIRS } from '@/features/chat/attachments/pickers';
import { attachmentLimits, DEFAULT_ATTACHMENT_POLICY, MIB } from '@/features/chat/attachments/prepare';
import {
  PDF_STEPS,
  SCAN_CACHE_DIR,
  VISIONKIT_CACHE_DIR,
  ScanError,
  scanFormats,
  scanName,
  scanText,
  scanToFiles,
  type ScanPipelineDeps,
} from '@/features/chat/attachments/scan/scanPipeline';

jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: { uri: 'file:///app/Library/Caches/' } } }));
jest.mock('expo-image-picker', () => ({}));
jest.mock('expo-document-picker', () => ({}));

const DIR = 'file:///app/Library/Caches/DocumentScan/6f1c2b9e/';
const PAGES = [`${DIR}page-1.jpg`, `${DIR}page-2.jpg`, `${DIR}page-3.jpg`];
const NOW = new Date(2026, 9, 8, 14, 5);
// The tray's second line: the day in the device's locale ("Oct 8" in en-US).
const DAY = NOW.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
const limits = attachmentLimits(DEFAULT_ATTACHMENT_POLICY);
// The recognised lines and boxes, opaque to JS (native-modules/DocumentScanKit.swift).
const LAYOUT = '[[{"text":"Pump Station 7"}]]';

function deps(overrides: Partial<ScanPipelineDeps['native']> = {}, sizes: number[] = [400_000]) {
  const removed: string[] = [];
  const written: { uri: string; text: string }[] = [];
  const pdfCalls: { edge: number; quality: number; layout: string | null }[] = [];
  const d: ScanPipelineDeps = {
    native: {
      recognizeLayout: jest.fn(async () => ({ layout: LAYOUT, text: ['Pump Station 7', 'Findings', 'Total: 1,284.50 CAD'] })),
      makePdf: jest.fn(async (pages: string[], output: string, edge: number, quality: number, layout: string | null) => {
        pdfCalls.push({ edge, quality, layout });
        expect(pages).toEqual(PAGES);
        return { uri: output, size: sizes[Math.min(pdfCalls.length - 1, sizes.length - 1)] };
      }),
      recognizeText: jest.fn(async () => ['Pump Station 7', 'Findings', 'Total: 1,284.50 CAD']),
      // Native, so the text gets the Complete protection the pages and the PDF get (F5).
      writeText: jest.fn(async (uri: string, text: string) => {
        written.push({ uri, text });
        return new TextEncoder().encode(text).byteLength;
      }),
      ...overrides,
    },
    remove: (uri) => removed.push(uri),
    now: () => NOW,
  };
  return { d, removed, written, pdfCalls };
}

describe('scanFormats', () => {
  it('offers PDF and text where the workspace takes both, PDF first', () => {
    expect(scanFormats(limits)).toEqual(['pdf', 'text']);
  });

  it('offers only what the workspace accepts', () => {
    expect(scanFormats({ ...limits, acceptedExtensions: ['.txt', '.md'] })).toEqual(['text']);
    expect(scanFormats({ ...limits, acceptedExtensions: ['.pdf'] })).toEqual(['pdf']);
    expect(scanFormats({ ...limits, acceptedExtensions: ['.png'] })).toEqual([]);
  });
});

describe('scanName and scanText', () => {
  it('names a scan by its local date and time', () => {
    expect(scanName(NOW, '.pdf')).toBe('Scan 2026-10-08 14.05.pdf');
    expect(scanName(new Date(2027, 0, 2, 3, 4), '.txt')).toBe('Scan 2027-01-02 03.04.txt');
  });

  it('heads each page when there are several', () => {
    expect(scanText(['one'])).toBe('one\n');
    expect(scanText([' one ', 'two'])).toBe('--- Page 1 of 2 ---\n\none\n\n--- Page 2 of 2 ---\n\ntwo\n');
  });
});

describe('scanToFiles', () => {
  it('pdf: one PDF of every page with the on-device text layer, and the page images deleted', async () => {
    const { d, removed, pdfCalls } = deps();
    const result = await scanToFiles(PAGES, 'pdf', DIR, limits, d);
    expect(result).toEqual({
      files: [{ uri: `${DIR}scan.pdf`, name: 'Scan 2026-10-08 14.05.pdf', size: 400_000, mimeType: 'application/pdf', display: { title: 'Scan 14.05', detail: DAY } }],
      warnings: [],
    });
    expect(pdfCalls).toEqual([{ ...PDF_STEPS[0], layout: LAYOUT }]);
    expect(removed).toEqual(PAGES);
  });

  it('pdf: steps the page size and quality down until the PDF fits the per-file limit', async () => {
    const small = { ...limits, maxFileBytes: 1 * MIB };
    const { d, removed, pdfCalls } = deps({}, [3 * MIB, 2 * MIB, 900_000]);
    const result = await scanToFiles(PAGES, 'pdf', DIR, small, d);
    expect(result.files[0].size).toBe(900_000);
    expect(pdfCalls.map(({ edge, quality }) => ({ edge, quality }))).toEqual(PDF_STEPS.slice(0, 3));
    // The pages are recognised once; every size step reuses that text layer (F4).
    expect(d.native.recognizeLayout).toHaveBeenCalledTimes(1);
    expect(d.native.recognizeLayout).toHaveBeenCalledWith(PAGES);
    expect(pdfCalls.map((c) => c.layout)).toEqual([LAYOUT, LAYOUT, LAYOUT]);
    // Each PDF too large was deleted before the next try; the kept one is not.
    expect(removed).toEqual([`${DIR}scan.pdf`, `${DIR}scan.pdf`, ...PAGES]);
  });

  it('pdf: refuses a scan that cannot fit, and deletes everything', async () => {
    const { d, removed } = deps({}, [9 * MIB]);
    await expect(scanToFiles(PAGES, 'pdf', DIR, { ...limits, maxFileBytes: 1 * MIB }, d)).rejects.toEqual(
      new ScanError('The scan is larger than 1.0 MB. Scan fewer pages.'),
    );
    expect(removed).toEqual([...PDF_STEPS.map(() => `${DIR}scan.pdf`), ...PAGES]);
  });

  it("pdf: fitted to the room left in the message, not only the per-file limit (F1)", async () => {
    // 10 MB a file, 25 MB together, 20 MB already in the tray: 5 MB is left.
    const tight = { ...limits, maxFileBytes: 10 * MIB, maxTotalBytes: 25 * MIB };
    const { d, pdfCalls } = deps({}, [8 * MIB, 6 * MIB, 4 * MIB]);
    const result = await scanToFiles(PAGES, 'pdf', DIR, tight, d, 5 * MIB);
    expect(result.files[0].size).toBe(4 * MIB);
    expect(pdfCalls).toHaveLength(3);
  });

  it('pdf: a scan that cannot fit the room left says so', async () => {
    const { d } = deps({}, [9 * MIB]);
    await expect(scanToFiles(PAGES, 'pdf', DIR, { ...limits, maxFileBytes: 10 * MIB }, d, 2 * MIB)).rejects.toEqual(
      new ScanError('The scan is larger than the 2.0 MB left in this message. Scan fewer pages or remove a file.'),
    );
  });

  it('text: refuses text over the room left in the message', async () => {
    const { d, removed } = deps();
    await expect(scanToFiles(PAGES, 'text', DIR, limits, d, 20)).rejects.toEqual(
      new ScanError('The scanned text is larger than the 20 B left in this message. Scan fewer pages or remove a file.'),
    );
    expect(removed).toEqual([`${DIR}scan.txt`, ...PAGES]);
  });

  it('pdf: pages with no recognisable text are attached with a warning', async () => {
    const { d } = deps({ recognizeLayout: jest.fn(async () => ({ layout: '[[],[]]', text: ['', ' \n'] })) });
    const result = await scanToFiles(PAGES, 'pdf', DIR, limits, d);
    expect(result.files).toHaveLength(1);
    expect(result.warnings).toEqual(['No text was found on the scanned pages: the model may not be able to read them.']);
  });

  it('text: the recognised text as a .txt file, the images deleted and never uploaded', async () => {
    const { d, removed, written } = deps();
    const result = await scanToFiles(PAGES, 'text', DIR, limits, d);
    const text = '--- Page 1 of 3 ---\n\nPump Station 7\n\n--- Page 2 of 3 ---\n\nFindings\n\n--- Page 3 of 3 ---\n\nTotal: 1,284.50 CAD\n';
    expect(written).toEqual([{ uri: `${DIR}scan.txt`, text }]);
    expect(result).toEqual({
      files: [{ uri: `${DIR}scan.txt`, name: 'Scan 2026-10-08 14.05.txt', size: text.length, mimeType: 'text/plain', display: { title: 'Scan 14.05', detail: DAY } }],
      warnings: [],
    });
    expect(d.native.makePdf).not.toHaveBeenCalled();
    expect(d.native.recognizeLayout).not.toHaveBeenCalled();
    expect(removed).toEqual(PAGES);
  });

  it('text: no text found is a refusal, not an empty file', async () => {
    const { d, removed, written } = deps({ recognizeText: jest.fn(async () => ['', '   ']) });
    await expect(scanToFiles(PAGES, 'text', DIR, limits, d)).rejects.toEqual(new ScanError('No text was found on the scanned pages.'));
    expect(written).toEqual([]);
    expect(removed).toEqual(PAGES);
  });

  it('text: refuses text over the per-file limit', async () => {
    const { d, removed } = deps();
    await expect(scanToFiles(PAGES, 'text', DIR, { ...limits, maxFileBytes: 20 }, d)).rejects.toBeInstanceOf(ScanError);
    expect(removed).toEqual([`${DIR}scan.txt`, ...PAGES]);
  });

  it('deletes the page images when the native side fails', async () => {
    const { d, removed } = deps({ recognizeText: jest.fn(async () => Promise.reject(new Error('vision failed'))) });
    await expect(scanToFiles(PAGES, 'text', DIR, limits, d)).rejects.toThrow('vision failed');
    expect(removed).toEqual(PAGES);
  });

  it('no pages: nothing to attach', async () => {
    const { d } = deps();
    expect(await scanToFiles([], 'pdf', DIR, limits, d)).toEqual({ files: [], warnings: [] });
  });
});

it("keeps every scan file inside the directory swept at start and when a workspace is put aside", () => {
  expect(PICKER_CACHE_DIRS).toContain(SCAN_CACHE_DIR);
  expect(PICKER_CACHE_DIRS).toContain(VISIONKIT_CACHE_DIR);
});
