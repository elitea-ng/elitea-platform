/**
 * Uploads each utterance the segmenter cuts to `POST
 * /llm/v1/audio/transcriptions`, one request at a time — the dispatch half of
 * the old server's whisper path (`sio/asr.py`'s `_do_flush` and
 * `on_whisper_call_done`), moved into the browser with the transport.
 *
 * ONE REQUEST IN FLIGHT. Utterances that end while a request is out are held
 * and sent together when it returns. The old server did this to stay under
 * a provider's requests-per-minute limit (Azure whisper: 10 RPM) instead of
 * queueing retries behind it.
 *
 * EVERY UTTERANCE GETS ONE `onDone`, whatever happened to it — text, an empty
 * answer, a refusal — because the speaking-mode loop counts pending utterances
 * and sends the message only when the count reaches zero. A merged request
 * that carried three utterances answers three `onDone` calls.
 *
 * A RATE LIMIT IS DROPPED QUIETLY, as the old server dropped a 429: the
 * utterance is lost, recording stays alive, and the user keeps talking. Every
 * other refusal is reported once through `onError`. That includes a used-up
 * budget (402, code `budget`): it does not clear on its own, so a quiet drop
 * would let the user dictate forever with nothing landing in the composer.
 */
import { isVoiceAbort, transcribeAudio, VoiceTransportError } from '@/shared/api/voiceTransport';
import type { VoiceErrorCode } from '@/shared/api/voiceTransport';

import { encodeWav } from './voiceAudio.helpers';

export interface SpeechTranscriberOptions {
  readonly projectId: string | number;
  readonly model: string;
  readonly language: string | undefined;
  readonly sampleRate: number;
  /** Aborting it discards every result still to come (a new session started, or the hook unmounted). */
  readonly signal: AbortSignal;
  readonly onText: (text: string) => void;
  readonly onDone: () => void;
  readonly onError: (code: VoiceErrorCode) => void;
}

export interface SpeechTranscriber {
  /** Queues one utterance for transcription. */
  readonly add: (samples: Float32Array) => void;
}

function concat(parts: readonly Float32Array[]): Float32Array {
  const out = new Float32Array(parts.reduce((sum, part) => sum + part.length, 0));
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

export function createSpeechTranscriber(options: SpeechTranscriberOptions): SpeechTranscriber {
  const { signal } = options;
  let inFlight = false;
  let held: Float32Array[] = [];

  const report = (err: unknown): void => {
    const code: VoiceErrorCode = err instanceof VoiceTransportError ? err.code : 'failed';
    if (code !== 'limit') options.onError(code);
  };

  const send = (samples: Float32Array, utterances: number): void => {
    inFlight = true;
    const audio = encodeWav(samples, options.sampleRate);
    void transcribeAudio({ projectId: options.projectId, model: options.model, audio, language: options.language }, signal)
      .then(
        (text) => {
          if (!signal.aborted && text) options.onText(text);
        },
        (err: unknown) => {
          if (!signal.aborted && !isVoiceAbort(err)) report(err);
        },
      )
      .finally(() => {
        inFlight = false;
        if (signal.aborted) return;
        for (let i = 0; i < utterances; i++) options.onDone();
        if (held.length > 0) {
          const next = held;
          held = [];
          send(concat(next), next.length);
        }
      });
  };

  return {
    add: (samples) => {
      if (signal.aborted) return;
      if (inFlight) held.push(samples);
      else send(samples, 1);
    },
  };
}
