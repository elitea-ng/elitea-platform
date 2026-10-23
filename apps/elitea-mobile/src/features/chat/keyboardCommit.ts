/**
 * Telling the keyboard's own late edit from a user's edit after Send (#56).
 *
 * iOS holds an autocorrection (or a QuickType candidate) for the word under the caret and
 * commits it through `onChangeText` when the input loses that word: here, right AFTER Send
 * has read the text and BEFORE the send settles. The composer kept "an edit made meanwhile",
 * so the corrected copy of the message stayed in the input and a quick second tap sent it
 * twice. A change that only rewrites the sent text's LAST word into a close spelling (the
 * case, an accent, a few letters, a split "alot" → "a lot"), possibly with the space the
 * keyboard appends, and that arrives within `KEYBOARD_COMMIT_WINDOW_MS` of Send, is the
 * keyboard's: the message it belongs to was sent, so the input is cleared. Anything else
 * (a new word, an edit earlier in the text, a later change) is the user's and is kept.
 */

/** How soon after Send a keyboard commit arrives (it is the same run loop in practice). */
export const KEYBOARD_COMMIT_WINDOW_MS = 1000;

/** Levenshtein distance, case-folded (the comparison is only ever between two short words). */
function distance(a: string, b: string): number {
  const x = Array.from(a.toLocaleLowerCase());
  const y = Array.from(b.toLocaleLowerCase());
  let previous = Array.from({ length: y.length + 1 }, (_, j) => j);
  for (let i = 1; i <= x.length; i += 1) {
    const current = [i];
    for (let j = 1; j <= y.length; j += 1) {
      current[j] = Math.min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (x[i - 1] === y[j - 1] ? 0 : 1));
    }
    previous = current;
  }
  return previous[y.length];
}

/** True when `current` is `sent` with only its last word replaced the way a keyboard corrects one. */
export function isKeyboardCommit(sent: string, current: string): boolean {
  if (current === sent) return true;
  const last = /^([\s\S]*?)(\S+)\s*$/.exec(sent);
  if (last === null) return false;
  const [, prefix, word] = last;
  if (!current.startsWith(prefix)) return false;
  // One word, or a split into two ("alot" → "a lot"), then at most the one space iOS appends.
  const tail = /^(\S+(?: \S+)?) ?$/.exec(current.slice(prefix.length));
  if (tail === null) return false;
  const replacement = tail[1];
  if (replacement === word) return true;
  const letters = Array.from(word).length;
  // A correction stays close to what was typed: at most half the word's letters change (at least 2).
  return distance(word, replacement.replace(' ', '')) <= Math.max(2, Math.ceil(letters / 2));
}

/**
 * Whether the input still holds only the sent message (so it can be cleared): unchanged, or
 * changed only by a keyboard commit that came right after Send.
 */
export function holdsOnlySent(sent: string, current: string, sentAt: number, lastChangeAt: number | null): boolean {
  if (current === sent) return true;
  if (lastChangeAt === null || lastChangeAt - sentAt > KEYBOARD_COMMIT_WINDOW_MS) return false;
  return isKeyboardCommit(sent, current);
}
