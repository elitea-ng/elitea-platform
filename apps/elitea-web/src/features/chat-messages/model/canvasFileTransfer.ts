/**
 * model/canvasFileTransfer.ts — the network half of issue #878: opening a
 * previously-stored artifact file in the canvas editor, and saving a canvas
 * document back into the artifact store.
 *
 * Both directions go through the SAME routes the artifacts browser already
 * uses (`shared/api/artifacts.ts`'s `fetchArtifactBlob`/`uploadArtifactObject`/
 * `listBuckets`/`listArtifacts`, all `internal/api/v2/artifacts` on the Go
 * side) — no new backend route, no OpenAPI change: the gap this closes was
 * entirely that nothing on the canvas side called them.
 *
 * Plain async functions, not a hook: every caller already has its own
 * `projectId`/base-URL resolution (the chat composition root, the Artifacts
 * page), and a hook here would just be a second place to get that wrong.
 */
import { fetchArtifactBlob, listArtifacts, listBuckets, uploadArtifactObject } from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';
import { isSystemBucket, normaliseBuckets, sortBucketsPinnedFirst } from '@/entities/bucket';
import type { BucketWire } from '@/entities/bucket';

import { detectCanvasFileOpenKind, isCanvasFileOpenSizeOk } from '../lib/canvasFileSource';
import type { CanvasFileOpenKind, CanvasFileSource } from '../lib/canvasFileSource';

/** The opened document, ready to hand to `CanvasEditor`'s `selectedCodeBlockInfo`. */
export interface OpenedCanvasFile {
  readonly codeBlock: string;
  readonly language: string;
  readonly type: CanvasFileOpenKind;
  readonly source: CanvasFileSource;
}

export type OpenArtifactFileResult =
  | { readonly ok: true; readonly file: OpenedCanvasFile }
  | { readonly ok: false; readonly reason: 'unsupported-kind' | 'too-large' | 'config-unavailable' | 'fetch-failed' };

export interface OpenArtifactFileParams {
  readonly projectId: string;
  readonly bucket: string;
  readonly name: string;
  /** Size in bytes, when the caller already knows it (a bucket listing row, an attachment's own metadata) — checked BEFORE fetching, so an oversized file is refused without downloading it. */
  readonly size?: number;
  readonly etag?: string;
  readonly signal?: AbortSignal;
}

/** Opens a stored artifact object as a canvas document, or explains why it can't. */
export async function openArtifactFileInCanvas(params: OpenArtifactFileParams): Promise<OpenArtifactFileResult> {
  const plan = detectCanvasFileOpenKind(params.name);
  if (plan === undefined) return { ok: false, reason: 'unsupported-kind' };
  if (!isCanvasFileOpenSizeOk(params.size)) return { ok: false, reason: 'too-large' };

  const config = getConfig();
  if (config.status !== 'ok') return { ok: false, reason: 'config-unavailable' };

  const result = await fetchArtifactBlob({
    baseUrl: config.config.vite_server_url,
    projectId: params.projectId,
    bucket: params.bucket,
    filePath: params.name,
    ...(params.signal ? { signal: params.signal } : {}),
  });
  if (!result.ok) return { ok: false, reason: 'fetch-failed' };

  // A second cap on the bytes actually received: a bucket listing's `size`
  // can be stale (the object was replaced after the list was read), and an
  // attachment's own metadata does not always carry a size at all.
  if (!isCanvasFileOpenSizeOk(result.data.size)) return { ok: false, reason: 'too-large' };

  const text = await result.data.text();
  const etagHeader = result.headers.get('etag') ?? undefined;
  return {
    ok: true,
    file: {
      codeBlock: text,
      language: plan.language,
      type: plan.type,
      source: {
        bucket: params.bucket,
        name: params.name,
        ...(etagHeader !== undefined ? { etag: etagHeader } : params.etag !== undefined ? { etag: params.etag } : {}),
      },
    },
  };
}

export interface SaveCanvasToArtifactParams {
  readonly projectId: string;
  readonly bucket: string;
  readonly name: string;
  readonly content: string;
}

export type SaveCanvasToArtifactResult = { readonly ok: true } | { readonly ok: false; readonly reason: 'config-unavailable' | 'upload-failed' };

/** Writes a canvas document into the artifact store — a create OR an overwrite of `bucket`/`name` (the same `overwrite=true` semantics every artifact upload already uses; the CALLER is responsible for confirming an overwrite before this is reached, see `SaveToArtifactsDialog`). */
export async function saveCanvasToArtifact(params: SaveCanvasToArtifactParams): Promise<SaveCanvasToArtifactResult> {
  const config = getConfig();
  if (config.status !== 'ok') return { ok: false, reason: 'config-unavailable' };
  const blob = new Blob([params.content], { type: 'text/plain' });
  const result = await uploadArtifactObject({
    baseUrl: config.config.vite_server_url,
    projectId: params.projectId,
    bucket: params.bucket,
    fileKey: params.name,
    file: blob,
  });
  return result.ok ? { ok: true } : { ok: false, reason: 'upload-failed' };
}

interface BucketListWire {
  readonly buckets: readonly BucketWire[];
}

function isBucketListWire(value: unknown): value is BucketListWire {
  return typeof value === 'object' && value !== null && Array.isArray((value as { buckets?: unknown }).buckets);
}

/** The project's non-system bucket names, pinned-first — the picker's own options list. Returns `[]` on any failure rather than throwing: an empty picker is a recoverable, visible state; a thrown fetch during a save dialog's mount is not. */
export async function listArtifactBucketNames(projectId: string, signal?: AbortSignal): Promise<readonly string[]> {
  const config = getConfig();
  if (config.status !== 'ok') return [];
  const result = await listBuckets({ baseUrl: config.config.vite_server_url, projectId, ...(signal ? { signal } : {}) });
  if (!result.ok || !isBucketListWire(result.data)) return [];
  return sortBucketsPinnedFirst(normaliseBuckets(result.data.buckets).filter((bucket) => !isSystemBucket(bucket.name))).map(
    (bucket) => bucket.name,
  );
}

interface ArtifactListWire {
  readonly objects: readonly { readonly key: string }[];
}

function isArtifactListWire(value: unknown): value is ArtifactListWire {
  return typeof value === 'object' && value !== null && Array.isArray((value as { objects?: unknown }).objects);
}

/** Whether `bucket` already holds an object named exactly `name` — the overwrite-confirmation check. `false` on a failed read: refusing to warn is safer here than blocking every save because the existence check itself failed. */
export async function artifactObjectExists(projectId: string, bucket: string, name: string, signal?: AbortSignal): Promise<boolean> {
  const config = getConfig();
  if (config.status !== 'ok') return false;
  const result = await listArtifacts({ baseUrl: config.config.vite_server_url, projectId, bucket, ...(signal ? { signal } : {}) });
  if (!result.ok || !isArtifactListWire(result.data)) return false;
  return result.data.objects.some((entry) => entry.key === name);
}
