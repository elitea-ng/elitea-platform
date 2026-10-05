import { fetchArtifactBlob } from '@/shared/api/artifacts';
import type { HttpFailure } from '@/shared/api/http';
import { getConfig } from '@/shared/config';

import { parseCodeDebugArtifactReference, type CodeDebugArtifactReference } from './codeDebugArtifact';

export type CodeDebugReadResult =
  | { readonly ok: true; readonly blob: Blob }
  | { readonly ok: false; readonly reason: 'authorization' | 'unavailable' | 'changed' | 'verification' | 'aborted' };

/** Read through the current artifact ACL. Verify raw bytes before delivery. */
export async function readCodeDebugArtifact(reference: CodeDebugArtifactReference, projectId: string, signal: AbortSignal): Promise<CodeDebugReadResult> {
  if (signal.aborted) return { ok: false, reason: 'aborted' };
  if (!parseCodeDebugArtifactReference(reference)) return { ok: false, reason: 'changed' };
  if (projectId !== String(reference.project_id)) return { ok: false, reason: 'authorization' };
  const config = getConfig();
  if (config.status !== 'ok') return { ok: false, reason: 'unavailable' };
  try {
    const result = await fetchArtifactBlob({
      baseUrl: config.config.vite_server_url, projectId, bucket: reference.bucket, filePath: reference.name, signal,
      maximumBytes: reference.byte_length,
    });
    if (signal.aborted) return { ok: false, reason: 'aborted' };
    if (!result.ok) return codeDebugReadFailure(result.error);
    return await verifyCodeDebugArtifact(result.data, reference, signal);
  } catch {
    return { ok: false, reason: signal.aborted ? 'aborted' : 'unavailable' };
  }
}

function codeDebugReadFailure(error: HttpFailure): CodeDebugReadResult {
  const authorization = (error.kind === 'http' || error.kind === 'auth') && (error.status === 401 || error.status === 403);
  return { ok: false, reason: authorization ? 'authorization' : 'unavailable' };
}

/** Verify the bounded raw response before download. */
export async function verifyCodeDebugArtifact(blob: Blob, reference: CodeDebugArtifactReference, signal: AbortSignal): Promise<CodeDebugReadResult> {
  if (signal.aborted) return { ok: false, reason: 'aborted' };
  if (!parseCodeDebugArtifactReference(reference) || blob.size !== reference.byte_length) return { ok: false, reason: 'changed' };
  if (!globalThis.crypto?.subtle) return { ok: false, reason: 'verification' };
  const bytes = await blob.arrayBuffer();
  if (signal.aborted) return { ok: false, reason: 'aborted' };
  const hash = await globalThis.crypto.subtle.digest('SHA-256', bytes);
  if (signal.aborted) return { ok: false, reason: 'aborted' };
  const actual = Array.from(new Uint8Array(hash), byte => byte.toString(16).padStart(2, '0')).join('');
  return actual === reference.sha256
    ? { ok: true, blob: new Blob([bytes], { type: reference.media_type }) }
    : { ok: false, reason: 'changed' };
}
