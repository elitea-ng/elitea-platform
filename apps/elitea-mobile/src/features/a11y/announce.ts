import { AccessibilityInfo } from 'react-native';

/**
 * VoiceOver / TalkBack announcements for moments a screen-reader user cannot
 * see: an answer finished, a run paused for an approval, a stop. Never token
 * by token: announcements are debounced (the last one within the window
 * wins) and the same text is not repeated within `repeatMs`.
 */
export interface Announcer {
  say(text: string): void;
  /** Drops a pending announcement (the screen went away). */
  cancel(): void;
}

export interface AnnouncerOptions {
  announce?: (text: string) => void;
  debounceMs?: number;
  repeatMs?: number;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
}

export const ANNOUNCE_DEBOUNCE_MS = 600;
export const ANNOUNCE_REPEAT_MS = 4000;

export function createAnnouncer(options: AnnouncerOptions = {}): Announcer {
  const announce = options.announce ?? ((text: string) => AccessibilityInfo.announceForAccessibility(text));
  const debounceMs = options.debounceMs ?? ANNOUNCE_DEBOUNCE_MS;
  const repeatMs = options.repeatMs ?? ANNOUNCE_REPEAT_MS;
  const now = options.now ?? Date.now;
  const setTimer = options.setTimer ?? ((fn: () => void, ms: number) => setTimeout(fn, ms));
  const clearTimer = options.clearTimer ?? ((handle: unknown) => clearTimeout(handle as ReturnType<typeof setTimeout>));
  let timer: unknown = null;
  let pending: string | null = null;
  let last: { text: string; at: number } | null = null;
  const flush = () => {
    timer = null;
    const text = pending;
    pending = null;
    if (text === null) return;
    if (last !== null && last.text === text && now() - last.at < repeatMs) return;
    last = { text, at: now() };
    announce(text);
  };
  return {
    say(text: string) {
      if (text.trim() === '') return;
      pending = text;
      if (timer !== null) clearTimer(timer);
      timer = setTimer(flush, debounceMs);
    },
    cancel() {
      if (timer !== null) clearTimer(timer);
      timer = null;
      pending = null;
    },
  };
}
