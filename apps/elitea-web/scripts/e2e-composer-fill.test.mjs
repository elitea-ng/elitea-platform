/**
 * No e2e spec fills the chat composer and then sends by hand (#2221, #2229).
 *
 * ## The defect this keeps out
 *
 * `chat-send-button` is rendered only while the composer holds text. A chat
 * pane that is still resolving its conversation can discard a fill that lands
 * too early, so `input.fill(...)`, `toBeEnabled()`, `click()` waits its whole
 * budget for a control that never comes (run 44164037437, and run 33812152063
 * before it). `fillComposer` in `e2e/fixtures/api.ts` retries the fill until
 * Send exists; every spec that sends through the real composer goes through it.
 *
 * ## Why a static test
 *
 * The race is a property of how a spec is WRITTEN, and it only shows up on a
 * slow runner, so no run of the suite proves its absence. This reads the
 * specs the way `e2e-journey-shape.test.mjs` does, in the `scripts` vitest
 * project, on every ci-web.yml run.
 *
 * ## The rule
 *
 * Under `e2e/` (every folder), a `.fill(` on the composer locator that is
 * followed within a few lines by a click on the Send control, or by pressing
 * Enter, is a failure. The locator may be declared over several lines and the
 * fill chained on its own line. Three specs fill by hand on purpose and are
 * named in `EXEMPT`. The first cases check the rule against the shapes the
 * specs had before, so it cannot pass by finding nothing.
 */
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const E2E_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'e2e');

/** How many lines after a composer fill a Send click (or Enter) still counts as "straight after". */
const WINDOW_LINES = 8;

/**
 * Specs that fill by hand on purpose (paths relative to `e2e/`):
 * - `chat.queued-messages.spec.ts` types and clicks Send WHILE a turn runs
 *   (steering): the pane resolved long before, and the send during a run is
 *   the behaviour under test.
 * - `chat.streaming.spec.ts` asserts Send is ABSENT before it fills.
 * - `chat.tail-sensitive-tools.spec.ts` drives the composer by hand to keep
 *   the timing of the turn's tail.
 */
const EXEMPT = new Set([
  'streaming/chat.queued-messages.spec.ts',
  'streaming/chat.streaming.spec.ts',
  'streaming/chat.tail-sensitive-tools.spec.ts',
]);

const COMPOSER = String.raw`(?:getByTestId\(\s*['"]chat-message-input['"],?\s*\)|locator\(\s*(?:'[^']*chat-message-input[^']*'|"[^"]*chat-message-input[^"]*"),?\s*\))`;

/** A key press that sends: Enter, alone or with a send modifier (Shift+Enter is a newline, not a send). */
const SEND_KEY = String.raw`(?:(?:Control|Meta|ControlOrMeta)\+)?Enter`;

/** Every spec under `dir`, as paths relative to `base`, with `/` separators. */
function specsUnder(dir, base = dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return specsUnder(path, base);
    return entry.name.endsWith('.spec.ts') ? [relative(base, path).split(sep).join('/')] : [];
  });
}

/** Characters after which a `/` starts a regular expression, not a division. */
const REGEX_PRECEDERS = new Set('(,=:[!&|?{};+-*%<>~^}'.split(''));
/** Words after which a `/` starts a regular expression (`return /x/.test(y)`). */
const REGEX_KEYWORDS = new Set(['return', 'typeof', 'case', 'in', 'of', 'delete', 'void', 'throw', 'new', 'else', 'do', 'yield', 'await', 'instanceof']);

/**
 * `source` with the inside of every comment, string, template literal and
 * regular expression literal blanked to spaces (same length, newlines kept),
 * so a brace inside one is not counted as a block boundary (#2250). A `/`
 * is a regular expression after an operator, `}` or a keyword such as
 * `return`, and a division after a name, a number or `)`; a template's `${}`
 * holds code of its own, with its own strings and templates.
 */
export function maskLiterals(source) {
  if (lastMasked?.source === source) return lastMasked.masked;
  const masked = maskUncached(source);
  lastMasked = { source, masked };
  return masked;
}

let lastMasked;

function maskUncached(source) {
  const out = source.split('');
  const blank = (from, to) => {
    for (let k = from; k < to; k += 1) if (out[k] !== '\n') out[k] = ' ';
  };
  /** The index after the template literal opening at `open`. */
  function template(open) {
    let j = open + 1;
    while (j < source.length && source[j] !== '`') {
      if (source[j] === '\\') {
        blank(j, j + 2);
        j += 2;
      } else if (source[j] === '$' && source[j + 1] === '{') {
        j = scan(j + 2, true) + 1;
      } else {
        blank(j, j + 1);
        j += 1;
      }
    }
    return j + 1;
  }
  /** Scans code from `start`; inside a `${}` it stops at the `}` that closes it and returns that index. */
  function scan(start, inTemplate) {
    let lastCode = '';
    let lastWord = '';
    let depth = 0;
    let i = start;
    while (i < source.length) {
      const ch = source[i];
      const next = source[i + 1];
      if (ch === '/' && next === '/') {
        const end = source.indexOf('\n', i);
        const stop = end === -1 ? source.length : end;
        blank(i, stop);
        i = stop;
      } else if (ch === '/' && next === '*') {
        const end = source.indexOf('*/', i + 2);
        const stop = end === -1 ? source.length : end + 2;
        blank(i, stop);
        i = stop;
      } else if (ch === '"' || ch === "'") {
        let j = i + 1;
        while (j < source.length && source[j] !== ch && source[j] !== '\n') j += source[j] === '\\' ? 2 : 1;
        blank(i + 1, j);
        i = j + 1;
        lastCode = ch;
        lastWord = '';
      } else if (ch === '`') {
        i = template(i);
        lastCode = ch;
        lastWord = '';
      } else if (ch === '/' && (lastCode === '' || REGEX_PRECEDERS.has(lastCode) || (lastCode === 'a' && REGEX_KEYWORDS.has(lastWord)))) {
        let j = i + 1;
        let inClass = false;
        while (j < source.length && source[j] !== '\n') {
          if (source[j] === '\\') j += 1;
          else if (source[j] === '[') inClass = true;
          else if (source[j] === ']') inClass = false;
          else if (source[j] === '/' && !inClass) break;
          j += 1;
        }
        blank(i + 1, j);
        i = j + 1;
        lastCode = '/';
        lastWord = '';
      } else if (/[A-Za-z_$]/.test(ch)) {
        let j = i + 1;
        while (j < source.length && /[\w$]/.test(source[j])) j += 1;
        lastWord = source.slice(i, j);
        lastCode = 'a';
        i = j;
      } else {
        if (inTemplate && ch === '{') depth += 1;
        else if (inTemplate && ch === '}') {
          if (depth === 0) return i;
          depth -= 1;
        }
        if (!/\s/.test(ch)) {
          lastCode = ch;
          lastWord = '';
        }
        i += 1;
      }
    }
    return i;
  }
  scan(0, false);
  return out.join('');
}

/** The `{ ... }` block around `index`: where a name declared there can be used. */
function enclosingBlock(rawSource, index) {
  const source = maskLiterals(rawSource);
  let depth = 0;
  let start = 0;
  for (let i = index - 1; i >= 0; i -= 1) {
    if (source[i] === '}') depth += 1;
    else if (source[i] === '{') {
      if (depth === 0) {
        start = i;
        break;
      }
      depth -= 1;
    }
  }
  let end = source.length;
  depth = 0;
  for (let i = start === 0 && source[0] !== '{' ? 0 : start + 1; i < source.length; i += 1) {
    if (source[i] === '{') depth += 1;
    else if (source[i] === '}') {
      if (depth === 0) {
        end = i;
        break;
      }
      depth -= 1;
    }
  }
  return [start, end];
}

/**
 * 1-based lines of every composer `.fill(` followed within WINDOW_LINES by a
 * click on the Send control or an Enter press.
 */
export function findBareComposerSends(source) {
  // A name is the composer only inside the block that declares it: another function's `input` is not.
  const declared = [];
  const declaration = new RegExp(String.raw`\b(?:const|let) (\w+)\s*=\s*[^;]*?${COMPOSER}`, 'g');
  for (const match of source.matchAll(declaration)) declared.push({ name: match[1], scope: enclosingBlock(source, match.index) });
  const fills = [{ pattern: new RegExp(String.raw`${COMPOSER}\s*\.fill\(`, 'g'), receiver: COMPOSER, scope: [0, source.length] }];
  for (const { name, scope } of declared) {
    fills.push({ pattern: new RegExp(String.raw`\b${name}(?:\([^)]*\))?\s*\.fill\(`, 'g'), receiver: String.raw`\b${name}(?:\([^)]*\))?`, scope });
  }
  const lines = source.split('\n');
  const found = new Set();
  for (const { pattern, receiver, scope } of fills) {
    for (const match of source.matchAll(pattern)) {
      if (match.index < scope[0] || match.index > scope[1]) continue;
      const index = source.slice(0, match.index).split('\n').length - 1;
      const after = lines.slice(index + 1, index + 1 + WINDOW_LINES).join('\n');
      const clicksSend = /chat-send-button|\bsendButton\b/.test(after) && /\.click\(\)/.test(after);
      // Enter on the composer itself, or on the focused element (the keyboard): not Enter on some other control.
      // `Control+Enter` and `Meta+Enter` send too; `page.press(<composer selector>, 'Enter')` is the same press (#2250).
      const pressesEnter =
        new RegExp(String.raw`(?:${receiver}|\bkeyboard)\s*\.press\(\s*(['"])${SEND_KEY}\1`).test(after) ||
        new RegExp(String.raw`\.press\(\s*(?:'[^']*chat-message-input[^']*'|"[^"]*chat-message-input[^"]*"|\`[^\`]*chat-message-input[^\`]*\`)\s*,\s*(['"])${SEND_KEY}\1`).test(after);
      if (clicksSend || pressesEnter) found.add(index + 1);
    }
  }
  return [...found].sort((a, b) => a - b);
}

const BEFORE = `
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
`;

const AFTER = `
  const send = await fillComposer(page, prompt);
  await send.click();
`;

describe('e2e specs send through fillComposer (#2221, #2229)', () => {
  it('flags the bare fill, enabled-check, click shape that #2221 counted', () => {
    expect(findBareComposerSends(BEFORE)).toHaveLength(1);
    expect(
      findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.getByTestId("chat-send-button").click();`),
    ).toHaveLength(1);
    expect(
      findBareComposerSends(`const composer = page.getByTestId('chat-message-input');\nawait composer.fill(x);\nconst sendButton = page.getByTestId('chat-send-button');\nawait sendButton.click();`),
    ).toHaveLength(1);
  });

  it('flags a locator declared over several lines, a chained fill, and fill then Enter (#2229)', () => {
    expect(
      findBareComposerSends(`const input = page\n  .getByTestId(\n    'chat-message-input',\n  );\nawait input.fill(x);\nawait page.getByTestId('chat-send-button').click();`),
    ).toHaveLength(1);
    expect(
      findBareComposerSends(`await page\n  .getByTestId('chat-message-input')\n  .fill(x);\nawait page.getByTestId('chat-send-button').click();`),
    ).toHaveLength(1);
    expect(findBareComposerSends(`const box = page.locator('[data-testid="chat-message-input"]');\nawait box.fill(x);\nawait box.press('Enter');`)).toHaveLength(1);
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.keyboard.press("Enter");`)).toHaveLength(1);
  });

  it('ties Enter to the composer, and a name to the block that declares it (#2231)', () => {
    // Enter on another control after a fill is not a send of the composer.
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.getByTestId('other').press('Enter');`)).toEqual([]);
    // The same name in another function is not the composer.
    expect(
      findBareComposerSends(
        `function a() {\n  const input = page.getByTestId('chat-message-input');\n}\nfunction b() {\n  const input = page.getByTestId('title');\n  await input.fill(x);\n  await input.press('Enter');\n}`,
      ),
    ).toEqual([]);
    // Inside its own block it still is.
    expect(
      findBareComposerSends(`function a() {\n  const input = page.getByTestId('chat-message-input');\n  await input.fill(x);\n  await input.press('Enter');\n}`),
    ).toHaveLength(1);
  });

  it('flags page.press on the composer selector and the modified Enter (#2250)', () => {
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.press('[data-testid="chat-message-input"]', 'Enter');`)).toHaveLength(1);
    expect(findBareComposerSends(`const box = page.getByTestId('chat-message-input');\nawait box.fill(x);\nawait box.press('Control+Enter');`)).toHaveLength(1);
    expect(findBareComposerSends(`const box = page.getByTestId('chat-message-input');\nawait box.fill(x);\nawait box.press('ControlOrMeta+Enter');`)).toHaveLength(1);
    // Shift+Enter is a newline, and another control's press is not the composer's.
    expect(findBareComposerSends(`const box = page.getByTestId('chat-message-input');\nawait box.fill(x);\nawait box.press('Shift+Enter');`)).toEqual([]);
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.press('#title', 'Enter');`)).toEqual([]);
  });

  it('does not count a brace inside a string, a regex or a comment as a block (#2250)', () => {
    const source = [
      'function a() {',
      "  const input = page.getByTestId('chat-message-input');",
      "  const label = '}'; // a } in a comment",
      '  const re = /[}]\\}/;',
      '  await input.fill(x);',
      "  await input.press('Enter');",
      '}',
    ].join('\n');
    expect(findBareComposerSends(source)).toHaveLength(1);
    const masked = maskLiterals(source);
    expect(masked).toHaveLength(source.length);
    expect(masked.split('}').length - 1).toBe(1);
  });

  it('reads a regex after return, and a template that nests a template, as literals (#2250)', () => {
    const regex = 'function a() {\n  return /[}]/.test(x);\n}';
    expect(maskLiterals(regex).split('}').length - 1).toBe(1);
    const nested = 'function a() {\n  const t = `a ${ `b }` + "}" } c }`;\n}';
    expect(maskLiterals(nested)).toHaveLength(nested.length);
    expect(maskLiterals(nested).split('}').length - 1).toBe(2);
    // A division is not a regex: the `}` after it still counts.
    expect(maskLiterals('function a() {\n  const r = x / 2; /* } */ \n}').split('}').length - 1).toBe(1);
  });

  it('accepts the helper, and a fill that is never followed by a send', () => {
    expect(findBareComposerSends(AFTER)).toEqual([]);
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait expect(page.getByTestId('chat-send-button')).toBeDisabled();`)).toEqual([]);
  });

  it('names its exemptions, and each one still exists', () => {
    const present = new Set(specsUnder(E2E_DIR));
    expect([...EXEMPT].filter((name) => !present.has(name))).toEqual([]);
  });

  it('finds no bare composer send in any e2e spec', () => {
    const offenders = [];
    for (const file of specsUnder(E2E_DIR).filter((name) => !EXEMPT.has(name))) {
      const lines = findBareComposerSends(readFileSync(join(E2E_DIR, file), 'utf8'));
      if (lines.length > 0) offenders.push(`${file}:${lines.join(',')}`);
    }
    expect(
      offenders,
      'fill the composer with `fillComposer` (e2e/fixtures/api.ts) and click the Send control it returns: a bare fill is discarded while the pane resolves its conversation, and the click then waits for a button that never renders',
    ).toEqual([]);
  });
});
