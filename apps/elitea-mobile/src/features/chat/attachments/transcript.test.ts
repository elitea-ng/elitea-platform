import { MAX_DOWNLOAD_BYTES, downloadAttachment, downloadErrorText, isSafeStoredName } from '@/api/attachments/download';
import { createApiClient } from '@/api/client';
import { CONV, PNG_DATA_URL, detailsGroup, documentItem, imageItem, transcriptRow } from '@/features/chat/__fixtures__/attachmentGroups';
import { viewerFor } from '@/features/chat/attachments/opener';
import { attachmentOf, groupText, toChatMessages } from '@/features/chat/messageModel';
import { toMessageGroup } from '@/sync/offlineReads';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: 'file:///cache' } }));

describe('attachment items in the transcript', () => {
  it('reads files from a details group: text first, then an image with its thumbnail and a document', () => {
    const { text, unsupported, attachments } = groupText(detailsGroup([imageItem(), documentItem()]));
    expect(text).toBe('what is in these?');
    // Files are no longer "only in the web app".
    expect(unsupported).toBe(false);
    expect(attachments).toEqual([
      {
        key: '501',
        name: 'photo-a1b2.jpg',
        filepath: `/chat-attachments/${CONV}/photo-a1b2.jpg`,
        storedName: 'photo-a1b2.jpg',
        conversationUuid: CONV,
        kind: 'image',
        inlineDataUrl: PNG_DATA_URL,
      },
      expect.objectContaining({ key: '502', name: 'report-c3d4.pdf', kind: 'document', inlineDataUrl: null }),
    ]);
  });

  it('reads the same files from a transcript row (offline copy path)', () => {
    const group = toMessageGroup(transcriptRow([imageItem(), documentItem()]) as never);
    const [message] = toChatMessages({ participants: [{ id: 1, entity_name: 'user', entity_meta: { id: 9 } }], message_groups: [group as never] });
    expect(message.text).toBe('what is in these?');
    expect(message.attachments.map((a) => a.storedName)).toEqual(['photo-a1b2.jpg', 'report-c3d4.pdf']);
  });

  it('keeps only small raster data URLs as thumbnails (never SVG or anything else)', () => {
    const svg = imageItem();
    (svg.item_details.content[1] as { image_url: { url: string } }).image_url.url = 'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=';
    expect(attachmentOf(svg)?.inlineDataUrl).toBeNull();
    const remote = imageItem();
    (remote.item_details.content[1] as { image_url: { url: string } }).image_url.url = 'https://tracker.example/pixel.png';
    expect(attachmentOf(remote)?.inlineDataUrl).toBeNull();
    // Still an image by its declared type: a chip, opened by download.
    expect(attachmentOf(remote)?.kind).toBe('image');
  });

  it('skips an item without a usable filepath, and flags it', () => {
    expect(attachmentOf({ id: 1, item_type: 'attachment_message', item_details: { filepath: `/b/${CONV}/..` } })).toBeNull();
    expect(groupText(detailsGroup([{ id: 1, item_type: 'attachment_message', order_index: 1, item_details: {} }])).unsupported).toBe(true);
  });

  it('shows the group content for a message that carries only files', () => {
    const group = { id: 41, uuid: 'x', content: '(attachment)', message_items: [documentItem(9, 0)] };
    expect(groupText(group)).toMatchObject({ text: '(attachment)', attachments: [expect.objectContaining({ storedName: 'report-c3d4.pdf' })] });
  });
});

describe('downloadAttachment', () => {
  beforeEach(() => {
    __resetSessionForTests();
    setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
    setCredential({ kind: 'pat', token: 't' });
  });
  const route = `/elitea_core/attachments/prompt_lib/7/${CONV}/report-c3d4.pdf`;

  it('fetches the bytes from the conversation-scoped route', async () => {
    const server = fakeServer();
    const pdf = new TextEncoder().encode('%PDF-1.7 tiny');
    server.on('GET', route, {
      status: 200,
      raw: pdf,
      headers: {
        'Content-Type': 'application/pdf',
        'Content-Length': String(pdf.byteLength),
        'Content-Disposition': "attachment; filename*=UTF-8''report-c3d4.pdf",
        'X-Content-Type-Options': 'nosniff',
        'Cache-Control': 'private, no-store',
      },
    });
    const file = await downloadAttachment(7, CONV, 'report-c3d4.pdf', server.client);
    expect(new TextDecoder().decode(file.bytes)).toBe('%PDF-1.7 tiny');
    expect(file.contentType).toBe('application/pdf');
    expect(server.requests[0].url.pathname).toBe(`/api/v2${route}`);
  });

  it.each([
    [403, "You don't have access to download this file."],
    [404, 'This file is no longer available.'],
    [501, 'This workspace does not offer file downloads to the app yet.'],
  ])('maps %s to its message', async (status, text) => {
    const server = fakeServer();
    server.on('GET', route, { status, body: { error: 'x' } });
    const error = await downloadAttachment(7, CONV, 'report-c3d4.pdf', server.client).catch((e: unknown) => e);
    expect(downloadErrorText(error)).toBe(text);
  });

  it('refuses a file larger than the device limit before reading it', async () => {
    const server = fakeServer();
    server.on('GET', route, { status: 200, raw: new Uint8Array(4), headers: { 'Content-Length': String(60 * 1024 * 1024) } });
    await expect(downloadAttachment(7, CONV, 'report-c3d4.pdf', server.client)).rejects.toMatchObject({ reason: 'too_large' });
  });

  it('stops reading a body with no Content-Length once it passes the limit (never buffers it whole)', async () => {
    const MIB = 1024 * 1024;
    const chunk = new Uint8Array(MIB);
    let pulled = 0;
    let cancelled = false;
    const total = MAX_DOWNLOAD_BYTES / MIB + 30;
    const client = createApiClient({
      origin: FAKE_ORIGIN,
      auth: { kind: 'session' },
      fetch: async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            pull(controller) {
              if (pulled >= total) return controller.close();
              pulled++;
              controller.enqueue(chunk);
            },
            cancel() {
              cancelled = true;
            },
          }),
          { status: 200, headers: { 'Content-Type': 'application/octet-stream' } },
        ),
    });
    await expect(downloadAttachment(7, CONV, 'report-c3d4.pdf', client)).rejects.toMatchObject({ reason: 'too_large' });
    expect(pulled).toBeLessThanOrEqual(MAX_DOWNLOAD_BYTES / MIB + 2);
    expect(cancelled).toBe(true);
  });

  it('reads a body with no Content-Length in pieces into one file', async () => {
    const parts = ['%PDF-1.7 ', 'in ', 'pieces'].map((p) => new TextEncoder().encode(p));
    const client = createApiClient({
      origin: FAKE_ORIGIN,
      auth: { kind: 'session' },
      fetch: async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              for (const part of parts) controller.enqueue(part);
              controller.close();
            },
          }),
          { status: 200, headers: { 'Content-Type': 'application/pdf' } },
        ),
    });
    const file = await downloadAttachment(7, CONV, 'report-c3d4.pdf', client);
    expect(new TextDecoder().decode(file.bytes)).toBe('%PDF-1.7 in pieces');
  });

  it('never asks for an unsafe name or a numeric conversation', async () => {
    const server = fakeServer();
    for (const name of ['../secret', 'a/b', 'a\\b', '..', '', 'x\u0000y']) expect(isSafeStoredName(name)).toBe(false);
    await expect(downloadAttachment(7, CONV, '../secret', server.client)).rejects.toMatchObject({ reason: 'not_found' });
    await expect(downloadAttachment(7, '42', 'a.pdf', server.client)).rejects.toMatchObject({ reason: 'not_found' });
    expect(server.requests).toEqual([]);
  });
});

describe('viewerFor', () => {
  it('shows only real raster images in the app; HTML, SVG and PDFs go to the system preview', () => {
    expect(viewerFor(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))).toBe('image');
    expect(viewerFor(new TextEncoder().encode('<svg xmlns="http://www.w3.org/2000/svg"/>'))).toBe('preview');
    expect(viewerFor(new TextEncoder().encode('<html><script>'))).toBe('preview');
    expect(viewerFor(new TextEncoder().encode('%PDF-1.7'))).toBe('preview');
  });
});
