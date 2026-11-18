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

/** Every spec under `dir`, as paths relative to `base`, with `/` separators. */
function specsUnder(dir, base = dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return specsUnder(path, base);
    return entry.name.endsWith('.spec.ts') ? [relative(base, path).split(sep).join('/')] : [];
  });
}

/** The `{ ... }` block around `index`: where a name declared there can be used. */
function enclosingBlock(source, index) {
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
      const pressesEnter = new RegExp(String.raw`(?:${receiver}|\bkeyboard)\s*\.press\(\s*(['"])Enter\1`).test(after);
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
