/**
 * No streaming spec fills the chat composer and then clicks Send by hand (#2221).
 *
 * ## The defect this keeps out
 *
 * `chat-send-button` is rendered only while the composer holds text. A chat
 * pane that is still resolving its conversation can discard a fill that lands
 * too early, so `input.fill(...)`, `toBeEnabled()`, `click()` waits its whole
 * budget for a control that never comes (run 44164037437, and run 33812152063
 * before it). `fillComposer` in `e2e/fixtures/api.ts` retries the fill until
 * Send exists; every spec that sends through the real composer goes through it.
 * 28 specs did not, and the rest of the suite carried the same shape until
 * this file existed.
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
 * Under `e2e/streaming/`, a `.fill(` on the composer locator that is followed
 * within a few lines by a click on the Send control is a failure. Two specs
 * fill by hand on purpose: `chat.queued-messages.spec.ts` sends while a turn
 * runs (named in `EXEMPT`), and `chat.streaming.spec.ts` asserts Send is
 * ABSENT first (its click is past the window, so it passes unnamed). The
 * first cases check the rule against the shape the specs had before, so it
 * cannot pass by finding nothing.
 */
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const STREAMING_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'e2e', 'streaming');

/** How many lines after a composer fill a Send click still counts as "straight after". */
const WINDOW_LINES = 8;

/**
 * Specs that fill by hand on purpose. `chat.queued-messages.spec.ts` types and
 * clicks Send WHILE a turn runs (steering): the pane resolved long before, and
 * the send during a run is the behaviour under test.
 */
const EXEMPT = new Set(['chat.queued-messages.spec.ts']);

/**
 * 1-based lines of every composer `.fill(` followed within WINDOW_LINES by a
 * click on the Send control.
 */
export function findBareComposerSends(source) {
  const lines = source.split('\n');
  const composerNames = new Set();
  for (const line of lines) {
    const named = /\bconst (\w+) = .*getByTestId\((['"])chat-message-input\2\)/.exec(line);
    if (named) composerNames.add(named[1]);
  }
  const fillOnComposer = (line) => {
    if (/getByTestId\((['"])chat-message-input\1\)\.fill\(/.test(line)) return true;
    return [...composerNames].some((name) => new RegExp(`\\b${name}(?:\\([^)]*\\))?\\.fill\\(`).test(line));
  };
  const found = [];
  lines.forEach((line, index) => {
    if (!fillOnComposer(line)) return;
    const after = lines.slice(index + 1, index + 1 + WINDOW_LINES).join('\n');
    const clicksSend = /chat-send-button|\bsendButton\b/.test(after) && /\.click\(\)/.test(after);
    if (clicksSend) found.push(index + 1);
  });
  return found;
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

describe('streaming specs send through fillComposer (#2221)', () => {
  it('flags the bare fill, enabled-check, click shape that #2221 counted', () => {
    expect(findBareComposerSends(BEFORE)).toHaveLength(1);
    expect(
      findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait page.getByTestId("chat-send-button").click();`),
    ).toHaveLength(1);
    expect(
      findBareComposerSends(`const composer = page.getByTestId('chat-message-input');\nawait composer.fill(x);\nconst sendButton = page.getByTestId('chat-send-button');\nawait sendButton.click();`),
    ).toHaveLength(1);
  });

  it('accepts the helper, and a fill that is never followed by a send', () => {
    expect(findBareComposerSends(AFTER)).toEqual([]);
    expect(findBareComposerSends(`await page.getByTestId('chat-message-input').fill(x);\nawait expect(page.getByTestId('chat-send-button')).toBeDisabled();`)).toEqual([]);
  });

  it('finds no bare composer send in any streaming spec', () => {
    const offenders = [];
    for (const file of readdirSync(STREAMING_DIR).filter((name) => name.endsWith('.spec.ts') && !EXEMPT.has(name))) {
      const lines = findBareComposerSends(readFileSync(join(STREAMING_DIR, file), 'utf8'));
      if (lines.length > 0) offenders.push(`${file}:${lines.join(',')}`);
    }
    expect(
      offenders,
      'fill the composer with `fillComposer` (e2e/fixtures/api.ts) and click the Send control it returns: a bare fill is discarded while the pane resolves its conversation, and the click then waits for a button that never renders',
    ).toEqual([]);
  });
});
