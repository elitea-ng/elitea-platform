/**
 * The voice transport: read-aloud and dictation over plain HTTPS to the
 * `/llm` audio routes, instead of the socket.io events the old app used.
 *
 * WHY NOT SOCKET.IO. elitea-main runs no socket.io server (removed in #126),
 * and `VITE_SOCKET_SERVER` is empty in every deployment, so the voice events
 * (`tts_start`/`tts_audio_chunk`/`asr_audio_chunk`/…) went to a no-op client
 * and voice failed with no message. The old server only relayed those events
 * to a worker that called `/v1/audio/speech` and `/v1/audio/transcriptions`.
 * The browser now calls the same two routes itself, through the
 * session-authenticated `/llm` edge (auth, project membership, the 413 and 504
 * bounds) and the gateway (model resolution, budget, rate limits, billing).
 *
 * The project is sent as `X-Project-Id`: the `/llm` edge reads that header,
 * checks membership, and bills that project (spec-llm-project-scope §6.1).
 */
import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';

export const SPEECH_PATH = '/llm/v1/audio/speech';
export const TRANSCRIPTION_PATH = '/llm/v1/audio/transcriptions';

/**
 * Why a voice request failed, as the UI words it:
 *  - `model-unavailable` — the model is not found, its provider cannot do
 *    this operation (501 `unsupported_operation`), or `/llm` has no backend.
 *  - `limit` — a budget (402) or rate limit (429) refused it.
 *  - `too-large` — the edge refused the body (413).
 *  - `failed` — anything else, including a network failure.
 */
export type VoiceErrorCode = 'model-unavailable' | 'limit' | 'too-large' | 'failed';

export class VoiceTransportError extends Error {
  readonly code: VoiceErrorCode;

  constructor(code: VoiceErrorCode) {
    super(`voice request failed: ${code}`);
    this.name = 'VoiceTransportError';
    this.code = code;
  }
}

/** True when the request was aborted on purpose (the user pressed stop). */
export function isVoiceAbort(err: unknown): boolean {
  if (err instanceof EliteaApiError) return err.failure.kind === 'aborted';
  return typeof err === 'object' && err !== null && (err as { name?: unknown }).name === 'AbortError';
}

function codeForStatus(status: number): VoiceErrorCode {
  if (status === 402 || status === 429) return 'limit';
  if (status === 413) return 'too-large';
  if (status === 404 || status === 501 || status === 503) return 'model-unavailable';
  return 'failed';
}

/** Maps any thrown value to a {@link VoiceTransportError}. An abort is re-thrown unchanged. */
function toVoiceError(err: unknown): never {
  if (isVoiceAbort(err)) throw err;
  if (err instanceof EliteaApiError && err.failure.kind === 'http') throw new VoiceTransportError(codeForStatus(err.failure.status));
  throw new VoiceTransportError('failed');
}

export interface SpeechRequest {
  /** The project the user works in. The `/llm` edge bills it. */
  readonly projectId: string | number;
  readonly model: string;
  readonly input: string;
  readonly voice?: string | undefined;
  readonly speed?: number | undefined;
  readonly instructions?: string | undefined;
}

/** `POST /llm/v1/audio/speech` — the provider's raw audio bytes (mp3 unless the model says otherwise). */
export async function synthesizeSpeech(request: SpeechRequest, signal?: AbortSignal): Promise<ArrayBuffer> {
  const { projectId, ...body } = request;
  try {
    const envelope = await eliteaFetch<{ data: ArrayBuffer }>(
      SPEECH_PATH,
      {
        method: 'POST',
        body: JSON.stringify(body),
        headers: { 'Content-Type': 'application/json', 'X-Project-Id': String(projectId) },
        ...(signal ? { signal } : {}),
      },
      { originRoot: true, binary: true },
    );
    return envelope.data;
  } catch (err) {
    return toVoiceError(err);
  }
}

export interface TranscriptionRequest {
  readonly projectId: string | number;
  readonly model: string;
  /** A complete audio file; the voice client sends 16-bit mono WAV. */
  readonly audio: Blob;
  readonly language?: string | undefined;
}

/** `POST /llm/v1/audio/transcriptions` (multipart) — the transcript text, `''` when the provider heard nothing. */
export async function transcribeAudio(request: TranscriptionRequest, signal?: AbortSignal): Promise<string> {
  const form = new FormData();
  form.append('model', request.model);
  form.append('file', request.audio, 'speech.wav');
  form.append('response_format', 'json');
  if (request.language) form.append('language', request.language);
  try {
    const envelope = await eliteaFetch<{ data: { text?: unknown } | undefined }>(
      TRANSCRIPTION_PATH,
      {
        method: 'POST',
        body: form,
        headers: { 'X-Project-Id': String(request.projectId) },
        ...(signal ? { signal } : {}),
      },
      { originRoot: true },
    );
    const text = envelope.data?.text;
    return typeof text === 'string' ? text.trim() : '';
  } catch (err) {
    return toVoiceError(err);
  }
}
