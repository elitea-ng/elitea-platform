import * as Speech from 'expo-speech';
import { useSyncExternalStore } from 'react';
import { Platform } from 'react-native';

import { deviceLanguage } from '@/features/speech/dictation';
import { registerWipeHook } from '@/security/endSession';
import { registerLockHook } from '@/security/lock/useLockController';

/**
 * Read an answer aloud with the device's own speech synthesiser (expo-speech,
 * AVSpeechSynthesizer on iOS): on the device, nothing is sent anywhere. One
 * answer at a time; reading another stops the first. Stopped when the
 * session is wiped and when the thread goes away.
 */

/** The synthesiser takes long texts, but a very long answer is cut to this many characters. */
export const MAX_SPOKEN_CHARS = 20_000;

/**
 * Markdown to something worth hearing: code blocks are announced, not read
 * symbol by symbol; links read their text; emphasis, heading and list marks
 * go.
 */
export function spokenText(markdown: string): string {
  const text = markdown
    .replace(/^ {0,3}(```|~~~)[^\n]*\n[\s\S]*?(?:\n {0,3}\1[^\n]*|$)/gm, ' Code block. ')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/^ {0,3}#{1,6}\s+/gm, '')
    .replace(/^ {0,3}>\s?/gm, '')
    .replace(/^[ \t]*[-*+][ \t]+/gm, '')
    .replace(/(\*\*|__)(.+?)\1/g, '$2')
    .replace(/(^|[^\w*])([*_])([^\s*_](?:.*?[^\s*_])?)\2(?![\w*])/g, '$1$3')
    .replace(/^[ \t]*(-{3,}|\*{3,}|_{3,})[ \t]*$/gm, '')
    .replace(/\|/g, ' ')
    .replace(/[ \t]+/g, ' ')
    .replace(/\n{3,}/g, '\n\n')
    .trim();
  return text.length > MAX_SPOKEN_CHARS ? text.slice(0, MAX_SPOKEN_CHARS) : text;
}

let speaking: string | null = null;
const listeners = new Set<() => void>();

function setSpeaking(key: string | null) {
  if (speaking === key) return;
  speaking = key;
  for (const listener of [...listeners]) listener();
}

export function canReadAloud(): boolean {
  return Platform.OS !== 'web';
}

/** Reads `text` (Markdown) aloud as `key`; reading something else stops the current one. */
export function readAloud(key: string, markdown: string): void {
  const text = spokenText(markdown);
  if (text === '' || !canReadAloud()) return;
  void Speech.stop();
  setSpeaking(key);
  const done = () => {
    if (speaking === key) setSpeaking(null);
  };
  Speech.speak(text, { language: deviceLanguage(), onDone: done, onStopped: done, onError: done });
}

export function stopReading(): void {
  if (speaking === null) return;
  setSpeaking(null);
  void Speech.stop();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** The key of the message being read, or null. */
export function useReading(): string | null {
  return useSyncExternalStore(subscribe, () => speaking, () => speaking);
}

registerWipeHook('read_aloud', stopReading);
// The lock screen must not read the workspace aloud or keep listening.
registerLockHook('read_aloud', stopReading);
