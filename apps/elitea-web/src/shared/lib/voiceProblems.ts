/**
 * What can go wrong with voice, and the message the user reads for each.
 *
 * Before the HTTP transport, every one of these was silence: the socket.io
 * client had no server, so a configured speech model "played" nothing and a
 * configured transcription model "heard" nothing. Each failure now has a code
 * (`api/voiceTransport.ts`'s `VoiceErrorCode`, plus `'no-model'` when nothing
 * at all can speak) and one sentence that names the cause.
 */
import { t } from '@/shared/i18n';

import type { VoiceErrorCode } from '../api/voiceTransport';

export type VoiceProblem = VoiceErrorCode | 'no-model';

const VOICE_PROBLEMS: ReadonlySet<string> = new Set<VoiceProblem>(['no-model', 'model-unavailable', 'limit', 'too-large', 'failed']);

/** The message for a voice problem code, or `undefined` for any other string (a browser speech-recognition error name). */
export function voiceErrorMessage(code: string): string | undefined {
  return VOICE_PROBLEMS.has(code) ? voiceProblemMessage(code as VoiceProblem) : undefined;
}

export function voiceProblemMessage(problem: VoiceProblem): string {
  switch (problem) {
    case 'no-model':
      return t('features.chatInput.voice.noModel', 'No speech model is configured for this project.');
    case 'model-unavailable':
      return t('features.chatInput.voice.modelUnavailable', 'The speech model is not available. Check the AI configuration.');
    case 'limit':
      return t('features.chatInput.voice.limit', 'A budget or rate limit stopped the voice request.');
    case 'too-large':
      return t('features.chatInput.voice.tooLarge', 'The recording is too long to transcribe.');
    case 'failed':
      return t('features.chatInput.voice.failed', 'The voice request failed. Try again.');
    default:
      return problem satisfies never;
  }
}
