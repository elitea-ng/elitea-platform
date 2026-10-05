import { UploadError, uploadAttachment, uploadErrorText } from '@/api/attachments/upload';
import { memoryDisk, readMultipart } from '@/api/attachments/uploadHarness';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: 'file:///cache' } }));

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const ROUTE = `/elitea_core/attachments/prompt_lib/7/${UUID}`;
const text = (s: string) => new TextEncoder().encode(s);
const source = (uri: string, size: number) => ({ uri, uploadName: 'report-8000.pdf', size, mimeType: 'application/pdf' });

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

describe('single-shot upload', () => {
  it('sends the file and its name, and answers the filepath (201)', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///a.pdf': text('%PDF-1.7 small') });
    server.on('POST', ROUTE, { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/report-8000.pdf`, file_size: 14 }] });
    const progress: (number | null)[] = [];
    const uploaded = await uploadAttachment(7, UUID, source('file:///a.pdf', 14), 100, {
      client: server.client,
      ...disk.deps,
      onProgress: (p) => progress.push(p),
    });
    expect(uploaded).toEqual({ filepath: `/chat-attachments/${UUID}/report-8000.pdf`, size: 14 });
    const multipart = readMultipart(server.requests[0].body!);
    expect(multipart.fields).toEqual({ file_name: 'report-8000.pdf' });
    expect(multipart.file).toBe('%PDF-1.7 small');
    expect(server.requests[0].url.search).toBe('');
    expect(progress).toEqual([null, 1]);
  });

  it('maps a 400 over the limit to the server message', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///a.pdf': text('x') });
    server.on('POST', ROUTE, { status: 400, body: { error: 'File size exceeds limit of 1 MB' } });
    const error = await uploadAttachment(7, UUID, source('file:///a.pdf', 1), 10, { client: server.client, ...disk.deps }).catch((e: unknown) => e);
    expect(error).toMatchObject({ reason: 'too_large' });
    expect(uploadErrorText(error, 'report.pdf')).toBe('report.pdf: File size exceeds limit of 1 MB');
  });

  it('refuses a numeric conversation id before sending anything', async () => {
    const server = fakeServer();
    await expect(uploadAttachment(7, '42', source('file:///a.pdf', 1), 10, { client: server.client })).rejects.toBeInstanceOf(UploadError);
    expect(server.requests).toEqual([]);
  });

  it('says to update on a 426', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///a.pdf': text('x') });
    server.on('POST', ROUTE, { status: 426, body: { error: 'client_upgrade_required', min_client_version: '9.0.0' } });
    const error = await uploadAttachment(7, UUID, source('file:///a.pdf', 1), 10, { client: server.client, ...disk.deps }).catch((e: unknown) => e);
    expect(uploadErrorText(error, 'x')).toBe('Update Agent Zefir to attach files.');
  });
});

describe('chunked upload', () => {
  const big = text('AAAAABBBBBCCC'); // 13 bytes in chunks of 5: 3 chunks

  it('sends offset chunks with file_id, chunk_index and total_chunks: 202, 202, then 201', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///big.pdf': big });
    server.on(
      'POST',
      ROUTE,
      { status: 202, body: { status: 'chunk_received', chunk_index: 0, total_chunks: 3, file_id: 'f1', message: 'ok' } },
      { status: 202, body: { status: 'chunk_received', chunk_index: 1, total_chunks: 3, file_id: 'f1', message: 'ok' } },
      { status: 201, body: [{ filepath: `/chat-attachments/${UUID}/report-8000.pdf`, file_size: 13 }] },
    );
    const progress: (number | null)[] = [];
    const uploaded = await uploadAttachment(7, UUID, source('file:///big.pdf', 13), 5, {
      client: server.client,
      ...disk.deps,
      newFileId: () => 'f1',
      onProgress: (p) => progress.push(p),
    });
    expect(uploaded.filepath).toBe(`/chat-attachments/${UUID}/report-8000.pdf`);
    const parts = server.requests.map((r) => readMultipart(r.body!));
    expect(parts.map((p) => [p.fields.chunk_index, p.fields.total_chunks, p.fields.file_id, p.fields.file_name, p.file])).toEqual([
      ['0', '3', 'f1', 'report-8000.pdf', 'AAAAA'],
      ['1', '3', 'f1', 'report-8000.pdf', 'BBBBB'],
      ['2', '3', 'f1', 'report-8000.pdf', 'CCC'],
    ]);
    expect(progress).toEqual([0, 1 / 3, 2 / 3, 1]);
    // Every staged chunk is gone.
    expect(disk.temps.size).toBe(0);
    expect(disk.deleted).toHaveLength(3);
  });

  it('retries a chunk the workspace did not take (up to 3 times), then goes on', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///big.pdf': big });
    server.on(
      'POST',
      ROUTE,
      { status: 202, body: { status: 'chunk_received' } },
      { status: 503, body: { error: 'busy' } },
      'network_error',
      { status: 202, body: { status: 'chunk_received' } },
      { status: 201, body: [{ filepath: `/b/${UUID}/report-8000.pdf`, file_size: 13 }] },
    );
    const sleeps: number[] = [];
    await uploadAttachment(7, UUID, source('file:///big.pdf', 13), 5, {
      client: server.client,
      ...disk.deps,
      newFileId: () => 'f1',
      sleep: async (ms) => void sleeps.push(ms),
    });
    expect(server.requests.map((r) => readMultipart(r.body!).fields.chunk_index)).toEqual(['0', '1', '1', '1', '2']);
    expect(sleeps).toEqual([500, 1000]);
  });

  it('gives up after 3 attempts, keeps nothing on disk, and says why', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///big.pdf': big });
    server.on('POST', ROUTE, { status: 202, body: { status: 'chunk_received' } }, { status: 502, body: {} });
    const error = await uploadAttachment(7, UUID, source('file:///big.pdf', 13), 5, {
      client: server.client,
      ...disk.deps,
      newFileId: () => 'f1',
      sleep: async () => {},
    }).catch((e: unknown) => e);
    expect(error).toMatchObject({ reason: 'unreachable' });
    expect(server.requests).toHaveLength(4);
    expect(disk.temps.size).toBe(0);
  });

  it('refuses a 201 before the last chunk as a bad answer', async () => {
    const server = fakeServer();
    const disk = memoryDisk({ 'file:///big.pdf': big });
    server.on('POST', ROUTE, { status: 201, body: [{ filepath: `/b/${UUID}/x`, file_size: 1 }] });
    await expect(
      uploadAttachment(7, UUID, source('file:///big.pdf', 13), 5, { client: server.client, ...disk.deps, newFileId: () => 'f1' }),
    ).rejects.toMatchObject({ reason: 'bad_response' });
  });
});
