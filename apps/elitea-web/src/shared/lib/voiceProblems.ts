/**
 * What can go wrong with voice, and the message the user reads for each.
 *
 * Before the HTTP transport, every one of these was silence: the socket.io
 * client had no server, so a configured speech model "played" nothing and a
 * configured transcription model "heard" nothing. Each failure now has a code
 * (`shared/api/voiceTransport.ts`'s `VoiceErrorCode`, plus `'no-model'` when nothing
 * at all can speak) and one sentence that names the cause.
 */
import { t } from '@/shared/i18n';

import type { VoiceErrorCode } from '@/shared/api/voiceTransport';

export type VoiceProblem = VoiceErrorCode | 'no-model';

const VOICE_PROBLEMS: ReadonlySet<string> = new Set<VoiceProblem>(['no-model', 'model-unavailable', 'budget', 'limit', 'too-large', 'failed']);

/**
 * The message for a microphone failure class, as `speechCapture.ts`'s
 * `mapGetUserMediaError` names it. The browser SpeechRecognition engine uses
 * the same three names. `undefined` for any other string.
 */
function microphoneErrorMessage(code: string): string | undefined {
  switch (code) {
    case 'not-allowed':
      return t(
        'widgets.chat.voiceButton.errorNotAllowed',
        'Microphone access denied. Please allow microphone access in your browser settings.',
      );
    case 'audio-capture':
      return t('widgets.chat.voiceButton.errorAudioCapture', 'No microphone found. Please connect a microphone and try again.');
    case 'network':
      return t(
        'widgets.chat.voiceButton.errorNetwork',
        'Voice input requires an internet connection. Please check your connection and try again.',
      );
    default:
      return undefined;
  }
}

/**
 * The message for a server-recognition failure: a microphone failure class or
 * a voice problem code. `undefined` for any other string, for example the
 * browser engine's `no-speech` or `aborted`, which stay silent.
 */
export function voiceErrorMessage(code: string): string | undefined {
  return microphoneErrorMessage(code) ?? (VOICE_PROBLEMS.has(code) ? voiceProblemMessage(code as VoiceProblem) : undefined);
}

export function voiceProblemMessage(problem: VoiceProblem): string {
  switch (problem) {
    case 'no-model':
      return t('features.chatInput.voice.noModel', 'No speech model is configured for this project.');
    case 'model-unavailable':
      return t('features.chatInput.voice.modelUnavailable', 'The speech model is not available. Check the AI configuration.');
    case 'budget':
      return t('features.chatInput.voice.budget', 'The project budget is used up.');
    case 'limit':
      return t('features.chatInput.voice.limit', 'Rate limit reached. Try again later.');
    case 'too-large':
      return t('features.chatInput.voice.tooLarge', 'The recording is too long to transcribe.');
    case 'failed':
      return t('features.chatInput.voice.failed', 'The voice request failed. Try again.');
    default:
      return problem satisfies never;
  }
}
