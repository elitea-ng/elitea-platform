/**
 * The workspace composer's triggers, as pure text functions: which "@"
 * (file) or "/" (command) token the caret is in, and which picked paths a
 * message still references.
 *
 * "@" opens anywhere a word starts; "/" only as the message's first word
 * (a path such as `src/main.rs` or a sentence with "and/or" never opens the
 * command menu).
 */

type ComposerTrigger = 'file' | 'command';

export interface ComposerToken {
  kind: ComposerTrigger;
  /** Offset of the trigger character. */
  start: number;
  /** Offset of the caret (the token's end). */
  end: number;
  /** What follows the trigger, up to the caret. */
  query: string;
}

/** The token the caret at `caret` is in, or `null` when it is in none. */
export function activeToken(value: string, caret: number): ComposerToken | null {
  const before = value.slice(0, Math.max(0, Math.min(caret, value.length)));
  const start = Math.max(before.lastIndexOf(' '), before.lastIndexOf('\n'), before.lastIndexOf('\t')) + 1;
  const word = before.slice(start);
  if (word.startsWith('@')) return { kind: 'file', start, end: before.length, query: word.slice(1) };
  if (word.startsWith('/') && before.slice(0, start).trim() === '') {
    return { kind: 'command', start, end: before.length, query: word.slice(1) };
  }
  return null;
}

/** The text that references a picked path: `@path` (a folder keeps its `/`). */
export function mentionText(path: string, kind: 'file' | 'dir'): string {
  return `@${path}${kind === 'dir' ? '/' : ''}`;
}

/**
 * The picked paths `text` still references (as `@path` followed by
 * whitespace or the end), in pick order. A reference the person deleted is
 * not sent.
 */
export function referencedPaths(text: string, picked: readonly string[]): string[] {
  return picked.filter((path, index) => {
    if (picked.indexOf(path) !== index) return false;
    const token = `@${path}`;
    let at = text.indexOf(token);
    while (at !== -1) {
      const next = text.charAt(at + token.length);
      const prev = at === 0 ? '' : text.charAt(at - 1);
      if ((next === '' || /\s/u.test(next)) && (prev === '' || /\s/u.test(prev))) return true;
      at = text.indexOf(token, at + 1);
    }
    return false;
  });
}
