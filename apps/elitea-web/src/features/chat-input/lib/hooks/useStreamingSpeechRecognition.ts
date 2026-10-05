/**
 * Server speech recognition for dictation and speaking mode, over HTTPS.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/
 * useStreamingSpeechRecognition.hooks.js`, with its transport replaced. The old
 * hook streamed PCM over socket.io (`asr_start`/`asr_audio_chunk`/`asr_stop`)
 * to a server that cut it into utterances and sent each one to the
 * transcription route. elitea-main runs no socket.io server, so the old hook
 * recorded and nothing ever answered. Now the browser does the server's work:
 *
 *   microphone frames (`../helpers/speechCapture.ts`)
 *     -> silence segmenter (`UtteranceSegmenter`, the old server's whisper VAD)
 *     -> one upload per utterance (`../helpers/speechTranscriber.ts`)
 *     -> `POST /llm/v1/audio/transcriptions` (`shared/api/voiceTransport.ts`).
 *
 * The callbacks keep the old event contract, so `useSpeakingModeLoop` and
 * `VoiceButton` are unchanged: `onSpeechStarted` (was `asr_speech_started`),
 * `onVadFlush` (was `asr_vad_flush`), `onTranscript` with a final text and
 * `onTranscriptDone` (was `asr_transcript_done`, once per utterance, even when
 * empty). There are no interim transcripts: the route answers whole utterances.
 * A realtime model that only speaks the WebSocket protocol is not reachable
 * this way; `/llm/v1/realtime` is the documented follow-up.
 *
 * SESSION FENCING. A new `startRecording` aborts the previous session's
 * pending uploads, so a late transcript from it cannot land in the new one.
 * `stopRecording` does NOT abort: it ends the utterance in progress and lets
 * its transcript arrive, so the last words of a dictation are not lost.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import type { ModelListItem } from '../../api/models';
import { mapGetUserMediaError, SPEECH_SAMPLE_RATE, startSpeechCapture } from '../helpers/speechCapture';
import type { SpeechCapture } from '../helpers/speechCapture';
import { createSpeechTranscriber } from '../helpers/speechTranscriber';
import type { SpeechTranscriber } from '../helpers/speechTranscriber';
import { UtteranceSegmenter } from '../helpers/voiceAudio.helpers';
import type { SegmenterEvent } from '../helpers/voiceAudio.helpers';
import type { TranscriptEvent } from './useSpeechRecognition';

export interface UseStreamingSpeechRecognitionParams {
  readonly onTranscript?: (event: TranscriptEvent) => void;
  readonly onTranscriptDone?: () => void;
  readonly onSpeechStarted?: () => void;
  readonly onVadFlush?: () => void;
  /** A `getUserMedia` class (`not-allowed`/`audio-capture`/`network`) or a voice problem code (`shared/lib/voiceProblems.ts`). */
  readonly onError?: (error: string) => void;
  readonly projectId?: string | undefined;
  readonly asrModel?: ModelListItem | undefined;
}

export interface UseStreamingSpeechRecognitionResult {
  readonly isRecording: boolean;
  /** A transcription model is configured and a project is selected — "supported" means "server recognition can run", not a browser feature check. */
  readonly isSupported: boolean;
  readonly startRecording: () => Promise<void>;
  readonly stopRecording: () => void;
}

interface Session {
  readonly segmenter: UtteranceSegmenter;
  readonly transcriber: SpeechTranscriber;
  capture: SpeechCapture | null;
}

/** The latest callbacks, read through one ref so a session never calls a stale closure. */
type Callbacks = Omit<UseStreamingSpeechRecognitionParams, 'projectId' | 'asrModel'>;

function dispatchSegmenterEvent(event: SegmenterEvent, session: Session, callbacks: Callbacks): void {
  if (event.kind === 'speech-start') {
    callbacks.onSpeechStarted?.();
  } else if (event.kind === 'segment') {
    callbacks.onVadFlush?.();
    session.transcriber.add(event.samples);
  } else {
    // Too short to upload. The old server still sent an empty
    // `transcript_done`, so the speaking-mode counter stays balanced.
    callbacks.onTranscriptDone?.();
  }
}

export function useStreamingSpeechRecognition(
  params: UseStreamingSpeechRecognitionParams = {},
): UseStreamingSpeechRecognitionResult {
  const { projectId, asrModel } = params;
  const [isRecording, setIsRecording] = useState(false);
  const callbacksRef = useRef<Callbacks>(params);
  // The RECORDING session (null once stopped) and the uploads of the most
  // recent session (kept after a stop, so its transcripts still arrive, and
  // aborted by the next start or by unmount).
  const sessionRef = useRef<Session | null>(null);
  const uploadsRef = useRef<AbortController | null>(null);

  useEffect(() => {
    callbacksRef.current = params;
  });

  const isSupported = !!asrModel && projectId !== undefined;

  const startRecording = useCallback(async () => {
    uploadsRef.current?.abort();
    sessionRef.current?.capture?.release();
    sessionRef.current = null;
    if (!asrModel || projectId === undefined) return;

    const abort = new AbortController();
    uploadsRef.current = abort;
    const transcriber = createSpeechTranscriber({
      projectId,
      model: asrModel.name,
      language: navigator.language?.split('-')[0] || undefined,
      sampleRate: SPEECH_SAMPLE_RATE,
      signal: abort.signal,
      onText: (text) => callbacksRef.current.onTranscript?.({ interim: '', final: text }),
      onDone: () => callbacksRef.current.onTranscriptDone?.(),
      onError: (code) => callbacksRef.current.onError?.(code),
    });
    const session: Session = { segmenter: new UtteranceSegmenter(), transcriber, capture: null };
    sessionRef.current = session;

    try {
      session.capture = await startSpeechCapture((frame) => {
        if (sessionRef.current !== session) return;
        for (const event of session.segmenter.push(frame)) dispatchSegmenterEvent(event, session, callbacksRef.current);
      });
    } catch (err) {
      if (sessionRef.current === session) sessionRef.current = null;
      callbacksRef.current.onError?.(mapGetUserMediaError(err));
      return;
    }
    if (sessionRef.current !== session) {
      // Stopped or restarted while the microphone was opening.
      session.capture.release();
      return;
    }
    setIsRecording(true);
  }, [asrModel, projectId]);

  const stopRecording = useCallback(() => {
    const session = sessionRef.current;
    if (!session) return;
    sessionRef.current = null;
    session.capture?.release();
    // End the utterance in progress; its transcript still arrives.
    for (const event of session.segmenter.flush()) dispatchSegmenterEvent(event, session, callbacksRef.current);
    setIsRecording(false);
  }, []);

  // Unmount: release the microphone and discard every pending transcript.
  useEffect(() => {
    return () => {
      const session = sessionRef.current;
      sessionRef.current = null;
      uploadsRef.current?.abort();
      session?.capture?.release();
    };
  }, []);

  return { isRecording, isSupported, startRecording, stopRecording };
}
