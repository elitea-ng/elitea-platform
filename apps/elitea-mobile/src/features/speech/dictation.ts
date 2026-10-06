import { useEffect, useRef, useState } from 'react';
import { Platform } from 'react-native';
import { ExpoSpeechRecognitionModule, type ExpoSpeechRecognitionErrorCode } from 'expo-speech-recognition';

import { log } from '@/lib/log';
import { registerWipeHook } from '@/security/endSession';
import { registerLockHook } from '@/security/lock/useLockController';

/**
 * Dictation into the composer, ON THE DEVICE ONLY (owner decision): the
 * recogniser runs with `requiresOnDeviceRecognition`, so no audio goes to
 * Apple's servers or to the workspace. A device (or a language) without an
 * on-device model gets no dictation rather than a silent fallback to server
 * recognition. The workspace policy's `allow_cloud_stt` is for the
 * workspace's own transcription model (a later wave); it never turns on
 * Apple's server recogniser.
 *
 * The guarantee is native, not this file's check: `supportsOnDeviceRecognition()`
 * asks about the system default language, which can differ from the one
 * dictation uses, and expo-speech-recognition 57.1.0 used to send the audio to
 * Apple's servers for a language without an on-device model. Our patch
 * (patches/expo-speech-recognition+57.1.0.patch, applied on install) makes
 * `start()` refuse such a language with `language-not-supported` and always
 * sets `requiresOnDeviceRecognition` on the request. The version is pinned
 * exactly so the patch cannot silently stop applying
 * (scripts/speech-on-device-patch.test.mjs).
 *
 * Nothing is recorded or kept: the recogniser streams words, the composer
 * shows them, and the text is sent only when the user sends the message.
 */

export type DictationProblem = 'permission' | 'unsupported' | 'no_speech' | 'interrupted' | 'failed';

export function dictationProblemText(problem: DictationProblem): string {
  switch (problem) {
    case 'permission':
      return 'Allow the microphone and speech recognition for this app in Settings to dictate.';
    case 'unsupported':
      return "Dictation on this device isn't available for your language. Your speech is never sent off the device.";
    case 'no_speech':
      return "Didn't catch that. Try again.";
    case 'interrupted':
      return 'Dictation stopped.';
    default:
      return "Dictation didn't work. Try again.";
  }
}

/** The recogniser's error codes as what the composer says. `aborted` is the user's own stop. */
export function problemOf(code: ExpoSpeechRecognitionErrorCode): DictationProblem | null {
  switch (code) {
    case 'aborted':
      return null;
    case 'not-allowed':
      return 'permission';
    case 'language-not-supported':
    case 'service-not-allowed':
      return 'unsupported';
    case 'no-speech':
    case 'speech-timeout':
      return 'no_speech';
    case 'interrupted':
      return 'interrupted';
    default:
      return 'failed';
  }
}

/** The text after dictation: what was typed before it, a space, then the words heard. */
export function joinDictation(before: string, heard: string): string {
  const words = heard.trim();
  if (words === '') return before;
  if (before === '' || /\s$/.test(before)) return before + words;
  return `${before} ${words}`;
}

/** The device language for the recogniser (BCP 47), else en-US. */
export function deviceLanguage(): string {
  try {
    const locale = Intl.DateTimeFormat().resolvedOptions().locale;
    return typeof locale === 'string' && locale !== '' ? locale : 'en-US';
  } catch {
    return 'en-US';
  }
}

/**
 * Android's dedicated on-device recogniser (createOnDeviceSpeechRecognizer) is
 * what expo-speech-recognition uses only from API 33. On API 31-32 it reports
 * on-device support but runs the default service with EXTRA_PREFER_OFFLINE, a
 * hint the service may ignore (and send audio to its servers).
 */
export const ANDROID_MIN_ON_DEVICE_API = 33;

/** True when this device can dictate without sending audio anywhere. */
export function canDictateOnDevice(platform: { OS: string; Version: number | string } = Platform): boolean {
  if (platform.OS === 'web') return false;
  if (platform.OS === 'android' && !(typeof platform.Version === 'number' && platform.Version >= ANDROID_MIN_ON_DEVICE_API)) return false;
  try {
    return ExpoSpeechRecognitionModule.isRecognitionAvailable() && ExpoSpeechRecognitionModule.supportsOnDeviceRecognition();
  } catch {
    return false;
  }
}

export interface DictationHandlers {
  /** The words heard so far (`final` once the recogniser is done with them). */
  onText: (heard: string, final: boolean) => void;
  onProblem: (problem: DictationProblem) => void;
}

/**
 * One dictation at a time: `start` asks for the microphone and recogniser
 * permissions (the first time), then listens until the user stops, the
 * recogniser ends, or the screen goes away.
 */
export function useDictation(handlers: DictationHandlers) {
  const [listening, setListening] = useState(false);
  const latest = useRef(handlers);
  /** Events count only between this hook's own start and the recogniser's end. */
  const active = useRef(false);
  useEffect(() => {
    latest.current = handlers;
  });

  useEffect(() => {
    if (Platform.OS === 'web') return;
    const end = () => {
      active.current = false;
      setListening(false);
    };
    const subs = [
      ExpoSpeechRecognitionModule.addListener('result', (event) => {
        if (!active.current) return;
        latest.current.onText(event.results[0]?.transcript ?? '', event.isFinal);
      }),
      ExpoSpeechRecognitionModule.addListener('error', (event) => {
        if (!active.current) return;
        const problem = problemOf(event.error);
        if (problem !== null) latest.current.onProblem(problem);
        end();
      }),
      ExpoSpeechRecognitionModule.addListener('end', end),
    ];
    return () => {
      for (const sub of subs) sub.remove();
      // Leaving the screen stops listening.
      if (active.current) abortDictation();
      active.current = false;
    };
  }, []);

  const start = async () => {
    if (listening) return;
    if (!canDictateOnDevice()) {
      latest.current.onProblem('unsupported');
      return;
    }
    try {
      const permission = await ExpoSpeechRecognitionModule.requestPermissionsAsync();
      if (!permission.granted) {
        latest.current.onProblem('permission');
        return;
      }
      active.current = true;
      setListening(true);
      ExpoSpeechRecognitionModule.start({
        lang: deviceLanguage(),
        interimResults: true,
        // Never Apple's server recogniser, whatever the device would choose.
        requiresOnDeviceRecognition: true,
        addsPunctuation: true,
        continuous: false,
        iosTaskHint: 'dictation',
      });
    } catch (error) {
      log.info('dictation did not start', { error });
      active.current = false;
      setListening(false);
      latest.current.onProblem('failed');
    }
  };

  const stop = () => {
    try {
      ExpoSpeechRecognitionModule.stop();
    } catch {
      active.current = false;
      setListening(false);
    }
  };

  return { listening, start, stop };
}

/** Stops any dictation at once (screen gone, session wiped). */
export function abortDictation(): void {
  if (Platform.OS === 'web') return;
  try {
    ExpoSpeechRecognitionModule.abort();
  } catch {
    // Not running.
  }
}

registerWipeHook('dictation', abortDictation);
// The lock screen must not read the workspace aloud or keep listening.
registerLockHook('dictation', abortDictation);
