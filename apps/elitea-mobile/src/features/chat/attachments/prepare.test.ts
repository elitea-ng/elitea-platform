import { attachmentPolicy } from '@/api/__fixtures__/nativeClient';
import { readyFile, type PipelineDeps } from '@/features/chat/attachments/imagePipeline';
import {
  DEFAULT_ATTACHMENT_POLICY,
  MIB,
  MOBILE_MAX_FILE_BYTES,
  attachmentLimits,
  checkFiles,
  extensionOf,
  imageMismatch,
  sniff,
  uploadName,
  type CheckedFile,
} from '@/features/chat/attachments/prepare';

jest.mock('expo-image-manipulator', () => ({ ImageManipulator: { manipulate: jest.fn() }, SaveFormat: { JPEG: 'jpeg' } }));
jest.mock('expo-file-system', () => ({ File: jest.fn() }));

const file = (name: string, size: number | null = 1000) => ({ uri: `file:///picked/${name}`, name, size, mimeType: null });

describe('attachmentLimits', () => {
  it('reads the 1.1 policy, capped by the mobile ceiling', () => {
    const limits = attachmentLimits(attachmentPolicy({ max_file_bytes: 200 * MIB, max_files: 4 }) as never);
    expect(limits).toMatchObject({ maxFiles: 4, maxFileBytes: MOBILE_MAX_FILE_BYTES, maxImageBytes: 3 * MIB, chunkBytes: 5 * MIB, serverDownscales: true });
    expect(limits.acceptedExtensions).toContain('.pdf');
  });

  it('falls back to the server defaults, and fits images itself on an older server', () => {
    expect(attachmentLimits(null)).toMatchObject({ maxFiles: 10, maxFileBytes: MOBILE_MAX_FILE_BYTES, serverDownscales: false });
    expect(attachmentLimits(null).acceptedExtensions).toEqual(DEFAULT_ATTACHMENT_POLICY.accepted_extensions);
  });
});

describe('checkFiles: type, then size, then count, then total', () => {
  const limits = attachmentLimits(attachmentPolicy({ max_files: 3, max_total_bytes: 60 * MIB }) as never);

  it('refuses an unknown type before anything else, but converts HEIC', () => {
    const result = checkFiles([file('virus.exe', 999 * MIB), file('IMG_0001.HEIC', 4 * MIB)], limits);
    expect(result.refused).toEqual([{ name: 'virus.exe', reason: 'type', message: 'virus.exe: this kind of file cannot be attached.' }]);
    expect(result.accepted).toEqual([expect.objectContaining({ name: 'IMG_0001.HEIC', extension: '.heic', kind: 'image', convert: true })]);
  });

  it('holds a document to the file limit and an image (re-encoded later) to the mobile ceiling', () => {
    const result = checkFiles([file('big.pdf', 51 * MIB), file('huge.png', 80 * MIB), file('photo.jpg', 12 * MIB)], limits);
    expect(result.refused.map((r) => [r.name, r.reason])).toEqual([
      ['big.pdf', 'size'],
      ['huge.png', 'size'],
    ]);
    expect(result.accepted.map((f) => f.name)).toEqual(['photo.jpg']);
  });

  it('counts what the composer already holds', () => {
    const result = checkFiles([file('a.txt'), file('b.txt')], limits, { count: 2, totalBytes: 0 });
    expect(result.accepted.map((f) => f.name)).toEqual(['a.txt']);
    expect(result.refused).toEqual([{ name: 'b.txt', reason: 'count', message: 'A message can carry at most 3 files.' }]);
  });

  it('refuses the file that would cross the total', () => {
    const result = checkFiles([file('a.pdf', 30 * MIB), file('b.pdf', 31 * MIB)], limits);
    expect(result.refused.map((r) => r.reason)).toEqual(['total']);
  });

  it('warns about a document the model may not read', () => {
    expect(checkFiles([file('report.pdf', 30 * MIB)], limits).warnings).toEqual(['report.pdf is large: the model may not read all of it.']);
  });
});

describe('names', () => {
  it('lower-cases the extension, sanitises the stem and adds a random suffix', () => {
    expect(uploadName('My Report (final).PDF', () => 0.5)).toBe('My_Report_final-8000.pdf');
    expect(uploadName('Résumé 2026.docx', () => 0)).toBe('Resume_2026-0000.docx');
    expect(uploadName('../../etc/passwd', () => 0.25)).toBe('passwd-4000');
    expect(uploadName('...', () => 0.25)).toBe('file-4000');
    expect(uploadName('IMG_0001.HEIC', () => 0.1, '.jpg')).toBe('IMG_0001-1999.jpg');
    expect(extensionOf('archive.tar.GZ')).toBe('.gz');
    expect(extensionOf('noext')).toBe('');
  });
});

describe('magic bytes', () => {
  const bytes = (...values: (number | string)[]) =>
    new Uint8Array(values.flatMap((v) => (typeof v === 'string' ? [...v].map((c) => c.charCodeAt(0)) : [v])));

  it('recognises the formats a phone attaches', () => {
    expect(sniff(bytes(0x89, 'PNG', 0x0d, 0x0a, 0x1a, 0x0a))).toBe('png');
    expect(sniff(bytes(0xff, 0xd8, 0xff, 0xe1))).toBe('jpeg');
    expect(sniff(bytes('GIF89a'))).toBe('gif');
    expect(sniff(bytes('RIFF', 0, 0, 0, 0, 'WEBP'))).toBe('webp');
    expect(sniff(bytes(0, 0, 0, 0x18, 'ftypheic'))).toBe('heic');
    expect(sniff(bytes('%PDF-1.7'))).toBe('pdf');
    expect(sniff(bytes('PK', 3, 4))).toBe('zip');
    expect(sniff(bytes('<svg'))).toBeNull();
  });

  it('refuses a name that claims an image the bytes are not', () => {
    expect(imageMismatch('.png', 'jpeg')).toBe(true);
    expect(imageMismatch('.jpg', null)).toBe(true);
    expect(imageMismatch('.jpg', 'jpeg')).toBe(false);
    expect(imageMismatch('.pdf', null)).toBe(false);
  });
});

describe('readyFile (image pipeline with a fake encoder)', () => {
  const limits = attachmentLimits(attachmentPolicy() as never);
  const JPEG = new Uint8Array([0xff, 0xd8, 0xff, 0xe0]);
  const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  const HEIC = new Uint8Array([0, 0, 0, 0x18, ...[...'ftypheic'].map((c) => c.charCodeAt(0))]);

  function deps(
    head: Uint8Array,
    size: number,
    sizes: (edge: number, q: number) => number,
  ): PipelineDeps & { encoded: [number | null, number][]; formats: string[]; deleted: string[] } {
    const encoded: [number | null, number][] = [];
    const formats: string[] = [];
    const deleted: string[] = [];
    return {
      encoded,
      formats,
      deleted,
      random: () => 0,
      openFile: (uri) => ({ size, readHead: async () => head, delete: () => deleted.push(uri) }),
      encode: async (_uri, edge, quality, format) => {
        encoded.push([edge, quality]);
        formats.push(format ?? 'jpeg');
        return { uri: `file:///cache/enc-${encoded.length}.jpg`, size: sizes(edge ?? 0, quality), width: 1, height: 1 };
      },
    };
  }
  const checked = (name: string, extra: Partial<CheckedFile> = {}): CheckedFile => ({
    ...file(name, 1),
    extension: extensionOf(name),
    kind: 'image',
    convert: false,
    ...extra,
  });

  it('converts a HEIC photo to a JPEG named .jpg', async () => {
    const d = deps(HEIC, 4 * MIB, () => 900 * 1024);
    const ready = await readyFile(checked('IMG_0001.HEIC', { convert: true }), limits, {}, d);
    expect(ready).toMatchObject({ uploadName: 'IMG_0001-0000.jpg', mimeType: 'image/jpeg', temporary: true, size: 900 * 1024, displayName: 'IMG_0001.HEIC' });
    expect(d.encoded).toEqual([[2048, 0.8]]);
  });

  it('always re-encodes a JPEG (EXIF and GPS go), stepping quality then size down until it fits', async () => {
    const d = deps(JPEG, 2 * MIB, (edge, q) => (edge === 2048 ? 4 * MIB : q > 0.5 ? 3.5 * MIB : 2 * MIB));
    const ready = await readyFile(checked('photo.jpg'), limits, {}, d);
    expect(ready.size).toBe(2 * MIB);
    expect(d.encoded).toEqual([[2048, 0.8], [2048, 0.65], [2048, 0.5], [2048, 0.4], [1229, 0.8], [1229, 0.65], [1229, 0.5]]);
    // Every rejected attempt's file is deleted.
    expect(d.deleted).toHaveLength(6);
  });

  it('re-writes a small PNG losslessly (as PNG): eXIf/XMP metadata, GPS included, does not go up', async () => {
    const d = deps(PNG, 200 * 1024, () => 210 * 1024);
    const ready = await readyFile(checked('shot.png'), limits, {}, d);
    expect(ready).toMatchObject({ uri: 'file:///cache/enc-1.jpg', temporary: true, mimeType: 'image/png', uploadName: 'shot-0000.png' });
    expect(ready.uri).not.toBe('file:///picked/shot.png');
    expect(d.encoded).toEqual([[null, 1]]);
    expect(d.formats).toEqual(['png']);
  });

  it('re-writes a small WebP from the gallery (its EXIF chunk can carry GPS) too', async () => {
    const WEBP = new Uint8Array([...'RIFF'].map((c) => c.charCodeAt(0)).concat([0, 0, 0, 0], [...'WEBP'].map((c) => c.charCodeAt(0))));
    const d = deps(WEBP, 200 * 1024, () => 300 * 1024);
    const ready = await readyFile(checked('IMG_2.webp'), limits, {}, d);
    expect(ready).toMatchObject({ temporary: true, mimeType: 'image/png', uploadName: 'IMG_2-0000.png' });
    expect(d.formats).toEqual(['png']);
  });

  it('a lossless re-write over the limit falls back to the JPEG steps', async () => {
    const d = deps(PNG, 200 * 1024, (edge) => (edge === 0 ? 60 * MIB : 300 * 1024));
    const ready = await readyFile(checked('shot.png'), limits, {}, d);
    expect(ready).toMatchObject({ temporary: true, mimeType: 'image/jpeg', uploadName: 'shot-0000.jpg' });
    expect(d.formats).toEqual(['png', 'jpeg']);
    expect(d.deleted).toEqual(['file:///cache/enc-1.jpg']);
  });

  it('fits the first image to the inline size only when the server does not downscale', async () => {
    const old = attachmentLimits(null);
    const d = deps(PNG, 200 * 1024, (edge) => (edge <= 1229 ? 40 * 1024 : 100 * 1024));
    const ready = await readyFile(checked('shot.png'), old, { fitInline: true }, d);
    expect(ready.size).toBe(40 * 1024);
    const d2 = deps(PNG, 200 * 1024, () => 0);
    await readyFile(checked('shot.png'), limits, { fitInline: true }, d2);
    expect(d2.encoded.every(([edge]) => edge === 2048)).toBe(true);
  });

  it('refuses an "image" whose bytes are not one, and a document over the limit', async () => {
    await expect(readyFile(checked('cat.png'), limits, {}, deps(JPEG, 10, () => 0))).rejects.toMatchObject({
      userMessage: 'cat.png is not the image its name says it is.',
    });
    await expect(
      readyFile({ ...checked('big.pdf'), kind: 'document' }, limits, {}, deps(new TextEncoder().encode('%PDF-1.7'), 51 * MIB, () => 0)),
    ).rejects.toMatchObject({ userMessage: 'big.pdf is larger than 50 MB.' });
  });
});
