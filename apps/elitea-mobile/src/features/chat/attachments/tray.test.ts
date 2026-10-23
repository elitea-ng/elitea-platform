import { attachmentGate } from '@/features/chat/attachments/gate';
import type { ReadyFile } from '@/features/chat/attachments/imagePipeline';
import { attachmentLimits, type CheckedFile } from '@/features/chat/attachments/prepare';
import { AttachmentTray, type TrayDeps } from '@/features/chat/attachments/tray';

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const picked = (name: string, size = 100) => ({ uri: `file:///p/${name}`, name, size, mimeType: null });
const flush = async () => {
  for (let i = 0; i < 5; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

function makeTray(over: Partial<TrayDeps> = {}) {
  const uploads: string[] = [];
  const discarded: string[] = [];
  const fitInline: boolean[] = [];
  let id = 0;
  const deps: TrayDeps = {
    limits: attachmentLimits(null),
    prepare: async (file: CheckedFile, options) => {
      fitInline.push(options.fitInline);
      const temporary = file.kind === 'image';
      return {
        uri: temporary ? `file:///cache/${file.name}.jpg` : file.uri,
        uploadName: `${file.name}-0000`,
        displayName: file.name,
        size: file.size ?? 0,
        mimeType: 'application/octet-stream',
        kind: file.kind,
        temporary,
      } satisfies ReadyFile;
    },
    upload: async (file, onProgress, target) => {
      uploads.push(file.uploadName);
      onProgress(1);
      return { filepath: `/chat-attachments/${target.conversationUuid}/${file.uploadName}`, size: file.size };
    },
    discard: (uri) => discarded.push(uri),
    describe: (error, name) => `${name}: ${error instanceof Error ? error.message : 'failed'}`,
    newId: () => `t${++id}`,
    ...over,
  };
  const tray = new AttachmentTray(deps);
  tray.setTarget(7, UUID);
  return { tray, uploads, discarded, fitInline };
}

describe('AttachmentTray', () => {
  it('prepares picked files, then uploads them in order on send', async () => {
    const { tray, uploads, fitInline } = makeTray();
    const { refused } = tray.add([picked('a.pdf'), picked('b.jpg'), picked('c.png')]);
    expect(refused).toEqual([]);
    expect(tray.getSnapshot().map((i) => i.state)).toEqual(['preparing', 'preparing', 'preparing']);
    await flush();
    expect(tray.getSnapshot().map((i) => i.state)).toEqual(['ready', 'ready', 'ready']);
    // Only the first image is fitted for the model.
    expect(fitInline).toEqual([false, true, false]);
    const result = await tray.uploadAll();
    expect(uploads).toEqual(['a.pdf-0000', 'b.jpg-0000', 'c.png-0000']);
    expect(result).toEqual({
      ok: true,
      attachments: [
        { filepath: `/chat-attachments/${UUID}/a.pdf-0000`, name: 'a.pdf' },
        { filepath: `/chat-attachments/${UUID}/b.jpg-0000`, name: 'b.jpg' },
        { filepath: `/chat-attachments/${UUID}/c.png-0000`, name: 'c.png' },
      ],
      ids: ['t1', 't2', 't3'],
    });
  });

  it('stops at a failed upload, keeps the others, and does not re-upload what is stored', async () => {
    let fail = true;
    const { tray, uploads } = makeTray({
      upload: async (file, _p, target) => {
        uploads.push(file.uploadName);
        if (file.uploadName.startsWith('b') && fail) throw new Error('refused');
        return { filepath: `/x/${target.conversationUuid}/${file.uploadName}`, size: 1 };
      },
    });
    tray.add([picked('a.pdf'), picked('b.pdf'), picked('c.pdf')]);
    await flush();
    expect(await tray.uploadAll()).toEqual({ ok: false, error: 'b.pdf: refused' });
    expect(tray.getSnapshot().map((i) => i.state)).toEqual(['uploaded', 'failed', 'ready']);
    fail = false;
    tray.retry('t2');
    expect((await tray.uploadAll()).ok).toBe(true);
    expect(uploads).toEqual(['a.pdf-0000', 'b.pdf-0000', 'b.pdf-0000', 'c.pdf-0000']);
  });

  it('refuses beyond the file count, counting what it holds', async () => {
    const { tray } = makeTray();
    tray.add(Array.from({ length: 9 }, (_, i) => picked(`f${i}.txt`)));
    const { refused } = tray.add([picked('x.txt'), picked('y.txt')]);
    expect(refused.map((r) => [r.name, r.reason])).toEqual([['y.txt', 'count']]);
  });

  it('answers what it holds (count and bytes, failed files left out), the room a scan is fitted to', async () => {
    const { tray } = makeTray({ prepare: async (file) => (file.name === 'bad.pdf' ? Promise.reject(new Error('no')) : { uri: file.uri, uploadName: file.name, displayName: file.name, size: file.size ?? 0, mimeType: 'application/pdf', kind: file.kind, temporary: false }) });
    expect(tray.usage()).toEqual({ count: 0, totalBytes: 0 });
    tray.add([picked('a.pdf', 300), picked('bad.pdf', 50), picked('b.pdf', 200)]);
    await flush();
    expect(tray.usage()).toEqual({ count: 2, totalBytes: 500 });
  });

  it("keeps a picked file's short display (a scan's title and date) on its item", async () => {
    const { tray } = makeTray();
    tray.add([{ ...picked('Scan 2026-10-08 14.05.pdf'), display: { title: 'Scan 14.05', detail: 'Oct 8' } }, picked('a.pdf')]);
    expect(tray.getSnapshot().map((i) => i.display ?? null)).toEqual([{ title: 'Scan 14.05', detail: 'Oct 8' }, null]);
  });

  it('marks a file that cannot be prepared as failed, and will not send until it is removed', async () => {
    const { tray } = makeTray({ prepare: async () => Promise.reject(new Error('not an image')) });
    tray.add([picked('cat.png')]);
    await flush();
    expect(tray.getSnapshot()[0]).toMatchObject({ state: 'failed', error: 'cat.png: not an image' });
    expect(await tray.uploadAll()).toEqual({ ok: false, error: 'cat.png: not an image' });
    tray.remove('t1');
    expect(tray.getSnapshot()).toEqual([]);
  });

  it('deletes temporary copies when a file is removed and when the message went', async () => {
    const { tray, discarded } = makeTray();
    tray.add([picked('a.jpg'), picked('b.jpg'), picked('c.pdf')]);
    await flush();
    tray.remove('t1');
    expect(discarded).toEqual(['file:///cache/a.jpg.jpg']);
    tray.clear();
    expect(discarded).toEqual(['file:///cache/a.jpg.jpg', 'file:///cache/b.jpg.jpg']);
    expect(tray.getSnapshot()).toEqual([]);
  });

  it("deletes the picker's own copies too: removed, sent, failed or dropped while preparing", async () => {
    // The image library, the camera and the document picker all hand over a
    // COPY in the app's cache (with the photo's original metadata); nothing
    // else ever deletes it while the session lasts.
    const picks: string[] = [];
    const { tray, discarded } = makeTray({ discardPicked: (uri) => picks.push(uri) });
    tray.add([picked('a.jpg'), picked('b.jpg'), picked('c.pdf')]);
    await flush();
    tray.remove('t1');
    expect(picks).toEqual(['file:///p/a.jpg']);
    tray.clear();
    expect(picks).toEqual(['file:///p/a.jpg', 'file:///p/b.jpg', 'file:///p/c.pdf']);
    // The re-encoded copies go as before; a pdf's ready file IS the picked copy, deleted once.
    expect(discarded).toEqual(['file:///cache/a.jpg.jpg', 'file:///cache/b.jpg.jpg']);

    const failed = makeTray({ prepare: async () => Promise.reject(new Error('not an image')), discardPicked: (uri) => picks.push(uri) });
    failed.tray.add([picked('bad.png')]);
    await flush();
    failed.tray.remove('t1');
    expect(picks.at(-1)).toBe('file:///p/bad.png');
  });

  it('a file picked while the others upload is not dropped from the message or deleted with it', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const picks: string[] = [];
    const { tray, uploads } = makeTray({
      discardPicked: (uri) => picks.push(uri),
      upload: async (file, _p, target) => {
        uploads.push(file.uploadName);
        if (file.uploadName.startsWith('a')) await gate;
        return { filepath: `/x/${target.conversationUuid}/${file.uploadName}`, size: 1 };
      },
    });
    tray.add([picked('a.pdf')]);
    await flush();
    const sending = tray.uploadAll();
    await flush();
    tray.add([picked('b.pdf')]);
    await flush();
    release();
    // b was never uploaded: this send must not go out without it.
    expect(await sending).toEqual({ ok: false, error: 'Wait until the files are ready.' });
    const again = await tray.uploadAll();
    expect(again).toMatchObject({ ok: true, attachments: [{ name: 'a.pdf' }, { name: 'b.pdf' }] });
    expect(uploads).toEqual(['a.pdf-0000', 'b.pdf-0000']);
  });

  it('a file removed while the others upload is not uploaded and does not fail the send', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const { tray, uploads } = makeTray({
      upload: async (file, _p, target) => {
        uploads.push(file.uploadName);
        if (file.uploadName.startsWith('a')) await gate;
        return { filepath: `/x/${target.conversationUuid}/${file.uploadName}`, size: 1 };
      },
    });
    tray.add([picked('a.pdf'), picked('b.pdf')]);
    await flush();
    const sending = tray.uploadAll();
    await flush();
    tray.remove('t2');
    release();
    expect(await sending).toMatchObject({ ok: true, attachments: [{ name: 'a.pdf' }], ids: ['t1'] });
    expect(uploads).toEqual(['a.pdf-0000']);
  });

  it('removing a file while it uploads cancels that upload; the send goes on with the rest', async () => {
    const signals: AbortSignal[] = [];
    const picks: string[] = [];
    const { tray, uploads } = makeTray({
      discardPicked: (uri) => picks.push(uri),
      upload: (file, _p, target, _l, signal) => {
        uploads.push(file.uploadName);
        signals.push(signal);
        if (!file.uploadName.startsWith('a')) return Promise.resolve({ filepath: `/x/${target.conversationUuid}/${file.uploadName}`, size: 1 });
        // a.pdf hangs until it is aborted, as a slow upload would.
        return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(new Error('aborted'))));
      },
    });
    tray.add([picked('a.pdf'), picked('b.pdf')]);
    await flush();
    const sending = tray.uploadAll();
    await flush();
    expect(tray.getSnapshot().map((i) => i.state)).toEqual(['uploading', 'ready']);
    tray.remove('t1');
    expect(signals[0].aborted).toBe(true);
    expect(tray.getSnapshot().map((i) => i.name)).toEqual(['b.pdf']);
    expect(await sending).toMatchObject({ ok: true, attachments: [{ name: 'b.pdf' }], ids: ['t2'] });
    expect(uploads).toEqual(['a.pdf-0000', 'b.pdf-0000']);
    // The cancelled file's copies are deleted once its upload let go.
    expect(picks).toContain('file:///p/a.pdf');
  });

  it('the thread going away aborts an upload in flight', async () => {
    const signals: AbortSignal[] = [];
    const { tray } = makeTray({
      upload: (_file, _p, _t, _l, signal) => {
        signals.push(signal);
        return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(new Error('aborted'))));
      },
    });
    tray.add([picked('a.pdf')]);
    await flush();
    const sending = tray.uploadAll();
    await flush();
    tray.clear();
    expect(signals[0].aborted).toBe(true);
    await sending;
  });

  it('clearing after a send forgets only the files that were sent', async () => {
    const picks: string[] = [];
    const { tray } = makeTray({ discardPicked: (uri) => picks.push(uri) });
    tray.add([picked('a.pdf')]);
    await flush();
    const sent = await tray.uploadAll();
    if (!sent.ok) throw new Error('expected an upload');
    // Picked while the message itself was being sent.
    tray.add([picked('b.pdf')]);
    await flush();
    tray.clear(sent.ids);
    expect(tray.getSnapshot().map((i) => [i.name, i.state])).toEqual([['b.pdf', 'ready']]);
    expect(picks).toEqual(['file:///p/a.pdf']);
  });

  it('follows the workspace limits that arrive after it was made (discovery after a cold-start deep link)', async () => {
    const seen: { prepare: number[]; upload: number[] } = { prepare: [], upload: [] };
    const { tray } = makeTray({
      prepare: async (file, options) => {
        seen.prepare.push(options.limits.chunkBytes);
        return { uri: file.uri, uploadName: `${file.name}-0000`, displayName: file.name, size: 1, mimeType: 'text/plain', kind: file.kind, temporary: false };
      },
      upload: async (file, _p, target, limits) => {
        seen.upload.push(limits.chunkBytes);
        return { filepath: `/x/${target.conversationUuid}/${file.uploadName}`, size: 1 };
      },
    });
    const live = { ...attachmentLimits(null), maxFiles: 1, chunkBytes: 4096 };
    tray.setLimits(live);
    const { refused } = tray.add([picked('a.txt'), picked('b.txt')]);
    expect(refused.map((r) => [r.name, r.reason])).toEqual([['b.txt', 'count']]);
    await flush();
    expect((await tray.uploadAll()).ok).toBe(true);
    expect(seen).toEqual({ prepare: [4096], upload: [4096] });
  });

  it('will not upload without a conversation', async () => {
    const { tray } = makeTray();
    tray.setTarget(7, null);
    tray.add([picked('a.pdf')]);
    await flush();
    expect(await tray.uploadAll()).toMatchObject({ ok: false });
  });
});

describe('attachmentGate', () => {
  const base = { online: true, conversationUuid: UUID };
  it.each([
    ['offline', { ...base, online: false, target: { kind: 'model' as const } }, 'Attachments need a connection.'],
    ['no conversation uuid', { ...base, conversationUuid: null, target: { kind: 'model' as const } }, 'Files cannot be attached to this conversation from mobile.'],
    ['a model chat', { ...base, target: { kind: 'model' as const } }, null],
    ['an agent with attachments on', { ...base, target: { kind: 'agent' as const, versionStatus: 'ready' as const, internalTools: ['ask_user', 'attachments'] } }, null],
    ['an agent without them', { ...base, target: { kind: 'agent' as const, versionStatus: 'ready' as const, internalTools: ['ask_user'] } }, "This agent doesn't take files."],
    ['an agent still being checked', { ...base, target: { kind: 'agent' as const, versionStatus: 'loading' as const, internalTools: [] } }, 'Checking whether this agent takes files…'],
    ['an agent that could not be checked (fail closed)', { ...base, target: { kind: 'agent' as const, versionStatus: 'error' as const, internalTools: [] } }, "Couldn't check whether this agent takes files."],
    ['nothing addressable', { ...base, target: { kind: 'none' as const } }, 'Files cannot be attached to this conversation from mobile.'],
  ])('%s', (_label, input, expected) => {
    expect(attachmentGate(input)).toBe(expected);
  });
});

describe('AttachmentTray: the sent files of a stopped question (#29)', () => {
  async function sent(over: Partial<TrayDeps> = {}) {
    const picks: string[] = [];
    const made = makeTray({ discardPicked: (uri) => picks.push(uri), ...over });
    made.tray.add([picked('report.pdf'), picked('chart.png')]);
    await flush();
    const result = await made.tray.uploadAll();
    if (!result.ok) throw new Error(result.error);
    made.tray.holdSent(result.ids, 'q1');
    return { ...made, picks };
  }

  it('holds the sent files out of the tray without deleting their copies', async () => {
    const { tray, discarded, picks } = await sent();
    expect(tray.getSnapshot()).toEqual([]);
    expect(discarded).toEqual([]);
    expect(picks).toEqual([]);
  });

  it('puts them back ready to upload again when the question was removed', async () => {
    const { tray, uploads, discarded } = await sent();
    expect(tray.restoreSent()).toEqual({ restored: ['report.pdf', 'chart.png'], lost: [] });
    expect(tray.getSnapshot().map((i) => [i.name, i.state, i.filepath])).toEqual([
      ['report.pdf', 'ready', null],
      ['chart.png', 'ready', null],
    ]);
    expect(discarded).toEqual([]);
    // The resend uploads them again (the removed message's upload is not reused).
    const again = await tray.uploadAll();
    expect(again.ok).toBe(true);
    expect(uploads).toEqual(['report.pdf-0000', 'chart.png-0000', 'report.pdf-0000', 'chart.png-0000']);
    // Nothing is held any more: a second restore puts nothing back.
    tray.clear((again as { ids: string[] }).ids);
    expect(tray.restoreSent()).toEqual({ restored: [], lost: [] });
  });

  it('deletes the copies once the answer settled with the question kept', async () => {
    const { tray, discarded, picks } = await sent();
    tray.releaseSent();
    expect(discarded).toEqual(['file:///cache/chart.png.jpg']);
    expect(picks).toEqual(['file:///p/report.pdf', 'file:///p/chart.png']);
    expect(tray.restoreSent()).toEqual({ restored: [], lost: [] });
  });

  it('reports a file whose local copy was swept as lost', async () => {
    const { tray } = await sent({ exists: (uri) => !uri.includes('chart') });
    expect(tray.restoreSent()).toEqual({ restored: ['report.pdf'], lost: ['chart.png'] });
    expect(tray.getSnapshot().map((i) => i.name)).toEqual(['report.pdf']);
  });

  it('keeps files picked meanwhile first and restores only what fits the limit', async () => {
    const { tray, picks } = await sent({ limits: { ...attachmentLimits(null), maxFiles: 2 } });
    tray.add([picked('new.txt')]);
    await flush();
    expect(tray.restoreSent()).toEqual({ restored: ['report.pdf'], lost: ['chart.png'] });
    expect(tray.getSnapshot().map((i) => i.name)).toEqual(['new.txt', 'report.pdf']);
    expect(picks).toEqual(['file:///p/chart.png']);
  });

  it('restores only what fits the total size limit next to files picked meanwhile (F2)', async () => {
    const limits = { ...attachmentLimits(null), maxTotalBytes: 1000 };
    const picks: string[] = [];
    const made = makeTray({ limits, discardPicked: (uri) => picks.push(uri) });
    const { tray } = made;
    tray.add([picked('big.pdf', 600), picked('small.pdf', 100)]);
    await flush();
    const result = await tray.uploadAll();
    if (!result.ok) throw new Error(result.error);
    tray.holdSent(result.ids, 'q1');
    // Picked while the answer streamed: checked against an empty tray, so accepted.
    expect(tray.add([picked('new.pdf', 500)]).refused).toEqual([]);
    await flush();
    expect(tray.restoreSent()).toEqual({ restored: ['small.pdf'], lost: ['big.pdf'] });
    expect(tray.getSnapshot().map((i) => i.name)).toEqual(['new.pdf', 'small.pdf']);
    expect(tray.getSnapshot().reduce((n, i) => n + (i.size ?? 0), 0)).toBeLessThanOrEqual(1000);
    expect(picks).toEqual(['file:///p/big.pdf']);
  });

  it('lets go of held files when the next message is sent or the thread goes', async () => {
    const { tray, picks } = await sent();
    tray.add([picked('next.pdf')]);
    await flush();
    const next = await tray.uploadAll();
    if (!next.ok) throw new Error(next.error);
    tray.holdSent(next.ids, 'q2');
    expect(picks).toEqual(['file:///p/report.pdf', 'file:///p/chart.png']);
    tray.clear();
    expect(picks).toEqual(['file:///p/report.pdf', 'file:///p/chart.png', 'file:///p/next.pdf']);
    expect(tray.restoreSent()).toEqual({ restored: [], lost: [] });
  });
});
