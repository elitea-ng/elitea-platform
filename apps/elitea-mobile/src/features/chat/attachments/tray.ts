import type { Uploaded } from '@/api/attachments/upload';
import type { TurnAttachment } from '@/api/queries/sendMessage';
import type { ReadyFile } from '@/features/chat/attachments/imagePipeline';
import { checkFiles, type AttachmentLimits, type CheckedFile, type PickedFile, type Refusal } from '@/features/chat/attachments/prepare';

/**
 * The composer's attachments, one chip each:
 *
 *   preparing → ready → uploading → uploaded
 *        ↘ failed          ↘ failed (retry: ready again)
 *
 * Files are prepared (checked, converted, resized) when picked and uploaded
 * when the message is sent, one at a time. A file already uploaded is not
 * sent again when a later one fails and the user retries. Nothing here
 * touches the network or the disk itself: both come in as functions.
 */

export type TrayItemState = 'preparing' | 'ready' | 'uploading' | 'uploaded' | 'failed';

export interface TrayItem {
  id: string;
  name: string;
  size: number | null;
  kind: CheckedFile['kind'];
  /** The local file for a thumbnail (images only). */
  previewUri: string | null;
  state: TrayItemState;
  /** 0..1 while uploading in chunks; null when it cannot be told. */
  progress: number | null;
  error: string | null;
  ready: ReadyFile | null;
  filepath: string | null;
  /** The picker's copy of the file (in the app's cache), deleted with the item. */
  pickedUri: string;
}

export interface TrayDeps {
  /** The limits at creation; `setLimits` replaces them when the workspace's arrive. */
  limits: AttachmentLimits;
  prepare: (file: CheckedFile, options: { fitInline: boolean; limits: AttachmentLimits }) => Promise<ReadyFile>;
  upload: (
    file: ReadyFile,
    onProgress: (fraction: number | null) => void,
    target: { projectId: number; conversationUuid: string },
    limits: AttachmentLimits,
  ) => Promise<Uploaded>;
  /** Deletes a temporary (re-encoded) file. */
  discard: (uri: string) => void;
  /**
   * Deletes the picker's copy of a picked file when the app owns it (the
   * image library, the camera and the document picker copy into the app's
   * cache, the photo with its original metadata). Nothing else deletes it
   * while the session lasts.
   */
  discardPicked?: (uri: string) => void;
  /** A user-facing message for an upload or preparation error. */
  describe: (error: unknown, name: string) => string;
  newId?: () => string;
}

export type UploadAllResult =
  /** `ids`: the tray items the attachments came from (clear only those once the message is sent). */
  | { ok: true; attachments: TurnAttachment[]; ids: string[] }
  | { ok: false; error: string };

let counter = 0;

export class AttachmentTray {
  private items: TrayItem[] = [];
  private readonly listeners = new Set<() => void>();
  private generation = 0;
  private target: { projectId: number; conversationUuid: string } | null = null;
  private limits: AttachmentLimits;

  constructor(private readonly deps: TrayDeps) {
    this.limits = deps.limits;
  }

  /**
   * The workspace's current limits (discovery may arrive, or change, after
   * the thread opened): checks, preparation and chunking all read these.
   */
  setLimits(limits: AttachmentLimits): void {
    this.limits = limits;
  }

  /** Where uploads go; known once the conversation is read. */
  setTarget(projectId: number | null, conversationUuid: string | null): void {
    this.target = projectId !== null && conversationUuid !== null ? { projectId, conversationUuid } : null;
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): readonly TrayItem[] => this.items;

  private set(items: TrayItem[]) {
    this.items = items;
    for (const listener of [...this.listeners]) listener();
  }

  private patch(id: string, patch: Partial<TrayItem>) {
    if (!this.items.some((i) => i.id === id)) return;
    this.set(this.items.map((i) => (i.id === id ? { ...i, ...patch } : i)));
  }

  get busy(): boolean {
    return this.items.some((i) => i.state === 'preparing' || i.state === 'uploading');
  }

  /** Files that will be sent (everything not failed). */
  get sendable(): number {
    return this.items.filter((i) => i.state !== 'failed').length;
  }

  /** Checks and prepares picked files; answers what was refused and what is worth a warning. */
  add(picked: readonly PickedFile[]): { refused: Refusal[]; warnings: string[] } {
    const held = this.items.filter((i) => i.state !== 'failed');
    const totalBytes = held.reduce((n, i) => n + (i.ready?.size ?? i.size ?? 0), 0);
    const { accepted, refused, warnings } = checkFiles(picked, this.limits, { count: held.length, totalBytes });
    for (const file of picked) if (!accepted.some((a) => a.uri === file.uri)) this.deps.discardPicked?.(file.uri);
    const added: { item: TrayItem; file: CheckedFile }[] = accepted.map((file) => ({
      file,
      item: {
        id: this.deps.newId?.() ?? `att-${++counter}`,
        name: file.name,
        size: file.size,
        kind: file.kind,
        previewUri: file.kind === 'image' ? file.uri : null,
        state: 'preparing',
        progress: null,
        error: null,
        ready: null,
        filepath: null,
        pickedUri: file.uri,
      },
    }));
    if (added.length === 0) return { refused, warnings };
    this.set([...this.items, ...added.map((a) => a.item)]);
    const generation = this.generation;
    void (async () => {
      for (const { item, file } of added) {
        if (generation !== this.generation) return;
        // The turn's first image is fitted for the model when the server does not do it.
        const firstImage = file.kind === 'image' && this.items.find((i) => i.kind === 'image' && i.state !== 'failed')?.id === item.id;
        try {
          const ready = await this.deps.prepare(file, { fitInline: firstImage, limits: this.limits });
          if (generation !== this.generation || !this.items.some((i) => i.id === item.id)) {
            if (ready.temporary) this.deps.discard(ready.uri);
            continue;
          }
          this.patch(item.id, { state: 'ready', ready, size: ready.size, previewUri: item.kind === 'image' ? ready.uri : null });
        } catch (error) {
          this.patch(item.id, { state: 'failed', error: this.deps.describe(error, file.name) });
        }
      }
    })();
    return { refused, warnings };
  }

  /** Deletes an item's files: its re-encoded copy and the picker's copy. */
  private forget(item: TrayItem): void {
    if (item.ready?.temporary) this.deps.discard(item.ready.uri);
    this.deps.discardPicked?.(item.pickedUri);
  }

  remove(id: string): void {
    const item = this.items.find((i) => i.id === id);
    if (!item || item.state === 'uploading') return;
    this.forget(item);
    this.set(this.items.filter((i) => i.id !== id));
  }

  /** A failed upload is tried again on the next send. */
  retry(id: string): void {
    const item = this.items.find((i) => i.id === id);
    if (item?.state === 'failed' && item.ready) this.patch(id, { state: 'ready', error: null, progress: null });
  }

  /**
   * Uploads every file not uploaded yet, one at a time. Stops at the first
   * failure (that chip turns red, the others keep their state) so the
   * message is not sent without it.
   */
  async uploadAll(): Promise<UploadAllResult> {
    if (this.items.some((i) => i.state === 'preparing')) return { ok: false, error: 'Wait until the files are ready.' };
    const failed = this.items.find((i) => i.state === 'failed');
    if (failed) return { ok: false, error: failed.error ?? `${failed.name} could not be attached. Remove it or try again.` };
    const target = this.target;
    if (target === null) return { ok: false, error: 'Files cannot be attached to this conversation from mobile.' };
    for (const { id } of this.items) {
      // Read the item afresh: one removed while an earlier file uploaded (its
      // copies already deleted) is skipped, not uploaded from a stale entry.
      const item = this.items.find((i) => i.id === id);
      if (!item || item.state !== 'ready' || !item.ready) continue;
      this.patch(item.id, { state: 'uploading', progress: null, error: null });
      try {
        const uploaded = await this.deps.upload(item.ready, (progress) => this.patch(item.id, { progress }), target, this.limits);
        this.patch(item.id, { state: 'uploaded', progress: 1, filepath: uploaded.filepath });
      } catch (error) {
        const message = this.deps.describe(error, item.name);
        this.patch(item.id, { state: 'failed', progress: null, error: message });
        return { ok: false, error: message };
      }
    }
    // A file picked while the others uploaded was not part of this pass: the
    // message must not go out without it (the next send uploads only it).
    if (this.items.some((i) => i.state !== 'uploaded' || i.filepath === null)) {
      return { ok: false, error: 'Wait until the files are ready.' };
    }
    return {
      ok: true,
      attachments: this.items.map((i) => ({ filepath: i.filepath as string, name: i.name })),
      ids: this.items.map((i) => i.id),
    };
  }

  /**
   * Forgets files and deletes their temporary copies: after the message is
   * accepted only the sent ones (`ids`), so a file picked meanwhile stays;
   * with no ids (the thread went) everything.
   */
  clear(ids?: readonly string[]): void {
    if (ids === undefined) {
      this.generation++;
      for (const item of this.items) this.forget(item);
      this.set([]);
      return;
    }
    const sent = new Set(ids);
    for (const item of this.items) if (sent.has(item.id)) this.forget(item);
    this.set(this.items.filter((i) => !sent.has(i.id)));
  }
}
