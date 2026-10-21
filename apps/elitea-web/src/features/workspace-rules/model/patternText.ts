/**
 * A prefix rule's pattern and examples as the panel's fields type them, with
 * shell-like quoting so the text keeps every token boundary:
 *
 * - words are separated by spaces; a word with spaces, quotes, `|`, `[`, `]`,
 *   `*` or other shell characters is written in single quotes (`'my dir'`),
 *   or with `"…"` / a backslash, as a shell would read it;
 * - alternatives are `[test|check]` (or `test|check`), each quoted as needed;
 * - a bare `*` is any one word (`{any: true}`), `'*'` the literal word `*`;
 * - one example per line, read by the host like a shell would (`shlex`).
 *
 * `formatPattern` and `parsePattern` round-trip every pattern.
 */
import type { PatternToken, PrefixRule, RuleExample } from '@/shared/desktop/rulesIpc';

const PLAIN = /^[\p{L}\p{N}_@%+=:,./-]+$/u;

/** A word quoted so a shell (and `parsePattern`) reads it back as one word. */
function quoteWord(word: string): string {
  return PLAIN.test(word) ? word : `'${word.replaceAll("'", `'"'"'`)}'`;
}

const isWildcard = (token: PatternToken): token is { any: true } => typeof token === 'object' && !Array.isArray(token);

export function formatPattern(pattern: readonly PatternToken[]): string {
  return pattern
    .map((token) => {
      if (isWildcard(token)) return '*';
      return typeof token === 'string' ? quoteWord(token) : `[${token.map(quoteWord).join('|')}]`;
    })
    .join(' ');
}

/** Why a pattern's text does not parse. */
export class PatternSyntaxError extends Error {}

interface WordState {
  alternatives: string[];
  /** Whether any part of the current alternative was quoted or escaped. */
  quoted: boolean[];
  bracket: boolean;
  closed: boolean;
}

const newWord = (): WordState => ({ alternatives: [''], quoted: [false], bracket: false, closed: false });

function finish(word: WordState): PatternToken {
  if (word.bracket && !word.closed) throw new PatternSyntaxError('a `[` is not closed with `]`');
  const [only] = word.alternatives;
  if (word.alternatives.length === 1 && only !== undefined) return only === '*' && word.quoted[0] !== true ? { any: true } : only;
  return word.alternatives;
}

/** Read a quoted run starting at `text[at]` (a quote); returns its text and the index after it. */
function readQuoted(text: string, at: number): [string, number] {
  const quote = text[at];
  let out = '';
  let index = at + 1;
  while (index < text.length) {
    const char = text[index]!;
    if (char === quote) return [out, index + 1];
    if (quote === '"' && char === '\\' && index + 1 < text.length) {
      out += text[index + 1];
      index += 2;
      continue;
    }
    out += char;
    index += 1;
  }
  throw new PatternSyntaxError(`a ${quote} quote is not closed`);
}

/** The words read so far and the one being read. */
interface Parser {
  tokens: PatternToken[];
  word: WordState | null;
}

const current = (parser: Parser): WordState => (parser.word ??= newWord());

function append(parser: Parser, part: string, quoted: boolean): void {
  const word = current(parser);
  if (word.closed) throw new PatternSyntaxError('nothing may follow `]` in the same word');
  const last = word.alternatives.length - 1;
  word.alternatives[last] += part;
  word.quoted[last] = word.quoted[last] === true || quoted;
}

function endWord(parser: Parser): void {
  if (parser.word !== null) parser.tokens.push(finish(parser.word));
  parser.word = null;
}

/** One reading step: the index after what it read, or `null` when the character is not its kind. */
type Step = (parser: Parser, text: string, index: number) => number | null;

const STEPS: readonly Step[] = [
  (parser, text, index) => (/\s/u.test(text[index]!) ? (endWord(parser), index + 1) : null),
  (parser, text, index) => {
    if (text[index] !== "'" && text[index] !== '"') return null;
    const [part, next] = readQuoted(text, index);
    append(parser, part, true);
    return next;
  },
  (parser, text, index) => (text[index] === '\\' && index + 1 < text.length ? (append(parser, text[index + 1]!, true), index + 2) : null),
  (parser, text, index) => (text[index] === '[' && parser.word === null ? ((parser.word = { ...newWord(), bracket: true }), index + 1) : null),
  (parser, text, index) => {
    if (text[index] !== ']' || parser.word?.bracket !== true || parser.word.closed) return null;
    parser.word.closed = true;
    return index + 1;
  },
  (parser, text, index) => {
    if (text[index] !== '|') return null;
    const word = current(parser);
    if (word.closed) throw new PatternSyntaxError('nothing may follow `]` in the same word');
    word.alternatives.push('');
    word.quoted.push(false);
    return index + 1;
  },
  (parser, text, index) => (append(parser, text[index]!, false), index + 1),
];

/**
 * The pattern typed as text.
 *
 * @throws PatternSyntaxError when a quote or a `[` is not closed.
 */
export function parsePattern(text: string): PatternToken[] {
  const parser: Parser = { tokens: [], word: null };
  let index = 0;
  while (index < text.length) {
    for (const step of STEPS) {
      const next = step(parser, text, index);
      if (next !== null) {
        index = next;
        break;
      }
    }
  }
  endWord(parser);
  return parser.tokens;
}

/** An example as one line: argv quoted like a shell would. */
export function formatExample(example: RuleExample): string {
  return typeof example === 'string' ? example : example.map(quoteWord).join(' ');
}

export function parseExamples(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

/** "2 examples checked", for the list. */
export function exampleCount(rule: PrefixRule): number {
  return (rule.match?.length ?? 0) + (rule.not_match?.length ?? 0);
}
