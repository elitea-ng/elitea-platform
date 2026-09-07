/**
 * The two shape rules the admin journeys regressed on (issues #539 and #544).
 *
 * ## Why a static test, and not a journey
 *
 * Both defects are properties of the SUITE, not of a screen: how many results
 * one failure destroys, and whether an assertion depends on the size of a
 * table. A journey cannot state either — it would have to fail 20 times on one
 * stack to reach the second, and it would have to fail on purpose to reach the
 * first. This file states them where they can be read in one second, and it
 * runs in the `scripts` vitest project on every CI run of ci-web.yml.
 *
 * ## The rules
 *
 *  1. `admin.features.spec.ts` is not serial AT FILE LEVEL. It was, and one
 *     failed test then made Playwright report the eight tests after it as "did
 *     not run" — neither a pass nor a failure. Three runs of `E2E (webkit)`
 *     reported the same eight. `serial` is kept for the three tests that really
 *     do run in an order, inside their own describe.
 *  2. `admin.app-requests.spec.ts` reads the moderation queue by NAME. It used
 *     to ask for `?limit=100&offset=0` and search the answer, and the queue
 *     sorts oldest-first — so its own newest row fell off the end once the
 *     table passed 100 rows, and the failure read as a lost request.
 *  3. No journey ENDS A SESSION it does not own. `auth.setup.ts` mints one
 *     server-side session per persona and all four workers replay its cookie;
 *     since shared migration 0117 a logout REVOKES that row, so one journey
 *     signing out on the shared state signs out the whole suite. Measured on
 *     the 1.60.0 smoke run: 21 chromium journeys failed downstream of J4,
 *     none for a reason of its own, and the refusal a revoked session
 *     produces — `401 missing authorization header` — reads exactly like a
 *     route that was never wired.
 *
 * Each rule is checked twice: against the real file, and against the shape the
 * file had before the correction. A rule with no failing case is a rule that
 * can be satisfied by returning "clean" for everything — the whole reason
 * `check-gates-selftest.mjs` exists.
 */
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const APP = join(dirname(fileURLToPath(import.meta.url)), '..');

const read = (relative) => readFileSync(join(APP, relative), 'utf8');

const JOURNEYS = 'e2e/journeys';
const REDIRECT_SPEC = 'e2e/journeys/shell/shell.redirect.spec.ts';
const FEATURES_SPEC = 'e2e/journeys/admin/admin.features.spec.ts';
const APP_REQUESTS_SPEC = 'e2e/journeys/admin/admin.app-requests.spec.ts';
const SEED_SCRIPT = 'scripts/e2e-stack.sh';

/* ── rule 1 ─────────────────────────────────────────────────────────────── */

/**
 * The 1-based lines that put a whole FILE into serial mode.
 *
 * Column zero is the discriminator, and it is enough: a `describe.configure`
 * inside a describe block is indented by the block it configures, and that one
 * is wanted. `test.describe.configure` outside any block configures the file.
 */
export function fileLevelSerialLines(source) {
  return source
    .split('\n')
    .map((line, index) => ({ line, number: index + 1 }))
    .filter(
      (entry) =>
        /^\w+\.describe\.configure\(\{\s*mode:\s*['"]serial['"]/.test(entry.line) ||
        /^\w+\.describe\.serial\b/.test(entry.line),
    )
    .map((entry) => entry.number);
}

/** The 1-based lines that put one describe BLOCK into serial mode. */
export function scopedSerialLines(source) {
  return source
    .split('\n')
    .map((line, index) => ({ line, number: index + 1 }))
    .filter((entry) => /^\s+\w+\.describe\.configure\(\{\s*mode:\s*['"]serial['"]/.test(entry.line))
    .map((entry) => entry.number);
}

/* ── rule 2 ─────────────────────────────────────────────────────────────── */

/**
 * Reads of a list endpoint that page instead of naming what they want.
 *
 * A line that sends `limit=` and no filter asks for "the first page of
 * everything" and then hopes the row is on it. The moderation queue offers an
 * exact `entity_id` filter, so the hope is not needed.
 *
 * A paged read that genuinely cannot filter — the route offers nothing
 * narrower, or the assertion NEEDS the exhaustive listing (the clerical
 * pin's before/after sweep) — satisfies this rule by carrying its
 * justification on the SAME line, naming `entity_id` in it: the reviewer
 * sees the reason where the read happens, and a bare hopeful page still
 * fails.
 */
export function unfilteredPagedReads(source) {
  return source
    .split('\n')
    .map((line) => line.trim())
    // A comment that QUOTES the old URL is how the correction explains itself,
    // so a rule that reads comments as code can only be satisfied by deleting
    // the account of what went wrong.
    .filter((line) => !line.startsWith('*') && !line.startsWith('//') && !line.startsWith('/*'))
    .filter((line) => line.includes('?limit=') && !line.includes('entity_id'));
}

/**
 * The body of the file-level `afterAll` hook, or `''` when there is none.
 *
 * Read as text, and only as far as the first line that closes at column zero:
 * a rule that searched the WHOLE file would be satisfied by a delete made
 * anywhere in it, including by the one J34g makes inside a test.
 */
export function teardownBody(source) {
  const start = source.indexOf('adminTest.afterAll(');
  if (start < 0) return '';
  const rest = source.slice(start);
  const end = rest.indexOf('\n});');
  return end < 0 ? rest : rest.slice(0, end);
}

/* ── rule 3 ─────────────────────────────────────────────────────────────── */

/**
 * The `test(...)` blocks that end a session while running on the SHARED
 * persona state.
 *
 * A test ends a session when its body reaches `/forward-auth/logout` or clicks
 * the "Log out" control. It OWNS the session when it takes the `browser`
 * fixture and makes its own context, which is what `e2e/fixtures/session.ts`
 * exists for. A test that takes `page` is running on the file the setup
 * project wrote, and every other worker holds the same cookie.
 *
 * Read as source rather than run as a journey for the same reason rules 1 and
 * 2 are: this is a property of the SUITE. The failure it prevents does not
 * appear in the offending test at all — that one passes — it appears in
 * whatever ran after it, as an authentication error with no cause nearby.
 */
export function sessionEndingTestsOnSharedState(source) {
  const offenders = [];
  // Split on the start of each `test(` / `adminTest(` block at column zero.
  const blocks = source.split(/\n(?=\w*[Tt]est\()/);
  for (const block of blocks) {
    const opening = /^\w*[Tt]est\(\s*(['"`])(.*?)\1\s*,\s*async\s*\(\{([^}]*)\}/s.exec(block);
    if (opening === null) continue;
    const [, , title, fixtures] = opening;
    const body = block
      .split('\n')
      .map((line) => line.trim())
      // A comment that QUOTES the endpoint is how the correction explains
      // itself; a rule that read comments as code could only be satisfied by
      // deleting the account of what went wrong.
      .filter((line) => !line.startsWith('*') && !line.startsWith('//') && !line.startsWith('/*'))
      .join('\n');
    const endsASession =
      body.includes('/forward-auth/logout') || /name:\s*'Log out'/.test(body);
    if (!endsASession) continue;
    const ownsItsSession = fixtures.includes('browser') && body.includes('browser.newContext(');
    if (!ownsItsSession) offenders.push(title);
  }
  return offenders;
}

/** Every journey spec under `e2e/journeys`, as `[relative path, source]`. */
function journeySpecs() {
  const found = [];
  const walk = (relative) => {
    for (const entry of readdirSync(join(APP, relative), { withFileTypes: true })) {
      const child = `${relative}/${entry.name}`;
      if (entry.isDirectory()) walk(child);
      else if (entry.name.endsWith('.spec.ts')) found.push([child, read(child)]);
    }
  };
  walk(JOURNEYS);
  return found;
}

/* ── the rules, on the real files and on the shape they replaced ────────── */

describe('#539 — one failure must not hide eight journeys', () => {
  it('admin.features.spec.ts is not serial at file level', () => {
    expect(fileLevelSerialLines(read(FEATURES_SPEC))).toEqual([]);
  });

  it('keeps serial for the ordered Help Center trio, in its own describe', () => {
    const source = read(FEATURES_SPEC);
    // Exactly one scoped group, and it holds three tests. A group that grows
    // is a group that hides results again.
    expect(scopedSerialLines(source)).toHaveLength(1);
    const group = source.slice(source.indexOf("adminTest.describe('the Help Center round trip'"));
    const closed = group.slice(0, group.indexOf('\n});'));
    expect(closed.match(/^ {2}adminTest\(/gm) ?? []).toHaveLength(3);
  });

  it('rejects the file-level shape the spec used to have', () => {
    const before = [
      "adminTest.use({ storageState: STORAGE_STATE.admin });",
      '',
      "adminTest.describe.configure({ mode: 'serial' });",
    ].join('\n');
    expect(fileLevelSerialLines(before)).toEqual([3]);
  });

  it('does not mistake a scoped group for a file-level one', () => {
    const scoped = ["adminTest.describe('group', () => {", "  adminTest.describe.configure({ mode: 'serial' });", '});'].join(
      '\n',
    );
    expect(fileLevelSerialLines(scoped)).toEqual([]);
    expect(scopedSerialLines(scoped)).toEqual([2]);
  });
});

describe('#544 — the app-requests journey must not read only the first page', () => {
  it('admin.app-requests.spec.ts names the row it reads', () => {
    expect(unfilteredPagedReads(read(APP_REQUESTS_SPEC))).toEqual([]);
  });

  it('rejects the unfiltered read the journey used to make', () => {
    const before =
      '      const response = await fetch(`${url}?limit=100&offset=0`, { credentials: ' +
      "'include' });";
    expect(unfilteredPagedReads(before)).toHaveLength(1);
  });

  it('reads a comment as prose, not as a read', () => {
    expect(unfilteredPagedReads(' * It used to ask for `?limit=100&offset=0` and search.')).toEqual(
      [],
    );
  });

  it('files every request through the helper that records it for the teardown', () => {
    const source = read(APP_REQUESTS_SPEC);
    expect(source).toContain('adminTest.afterAll(');
    // The record carries the AUTHOR as well as the name: the withdraw is scoped
    // to the session that filed the row, so a bare list of names cannot remove
    // the one the member persona filed.
    expect(source).toContain('filedEntities.push({ entity, author:');
  });

  /*
   * The teardown must DELETE, and prove it.
   *
   * The hook used to approve the rows it found still pending, because no delete
   * route existed. It does now, and a teardown that only decides leaves every
   * row behind — which is the half of #544 that stayed open after #610.
   */
  it('the teardown withdraws each row and re-reads it', () => {
    const source = read(APP_REQUESTS_SPEC);
    const hook = teardownBody(source);
    expect(hook).not.toEqual('');
    expect(hook).toContain('.delete(withdrawURL(');
    // The proof is the read-back through the operator's queue, not the delete's
    // own status code: a delete that reached the wrong row answers 200 too.
    expect(hook).toContain('queueByEntity(');
  });

  it('rejects a teardown that only decides the rows it filed', () => {
    const before = [
      'adminTest.afterAll(async () => {',
      "  const decided = await api.put(DECISION_URL, { data: { id: row.id, status: 'approved' } });",
      '});',
    ].join('\n');
    const hook = teardownBody(before);
    expect(hook).not.toEqual('');
    expect(hook).not.toContain('.delete(withdrawURL(');
  });

  it('the seed removes the probe rows earlier runs left behind', () => {
    const seed = read(SEED_SCRIPT);
    expect(seed).toContain('DELETE FROM centry.moderation_state');
    // …and keeps the two rows journeys 34 and 34c assert against.
    expect(seed).toContain("NOT IN ('e2e_app_request_probe_chromium', 'e2e_app_request_probe_webkit')");
  });
});

describe('#830 — a logout must not sign out every other worker', () => {
  it('no journey ends a session it did not create', () => {
    const offenders = journeySpecs().flatMap(([path, source]) =>
      sessionEndingTestsOnSharedState(source).map((title) => `${path} › ${title}`),
    );
    expect(offenders).toEqual([]);
  });

  it('J4 signs in through the provider before it signs out', () => {
    const source = read(REDIRECT_SPEC);
    expect(source).toContain("signInThroughOidc(page, 'e2e-member@autotest.local')");
    expect(source).toContain("browser.newContext({ storageState: undefined })");
  });

  it('rejects the shape J4 had, which took the shared page fixture', () => {
    const before = [
      "test('J4: logout clears user state and el.* storage', async ({ page }) => {",
      "  await page.goto(BASE_URL + '/app/settings/profile');",
      "  const logoutItem = page.getByRole('button', { name: 'Log out', exact: true });",
      '  await logoutItem.click();',
      '});',
    ].join('\n');
    expect(sessionEndingTestsOnSharedState(before)).toEqual([
      'J4: logout clears user state and el.* storage',
    ]);
  });

  it('accepts a test that makes its own context', () => {
    const after = [
      "test('J4: logout clears user state and el.* storage', async ({ browser }) => {",
      '  const context = await browser.newContext({ storageState: undefined });',
      '  const page = await context.newPage();',
      "  const logoutItem = page.getByRole('button', { name: 'Log out', exact: true });",
      '  await logoutItem.click();',
      '});',
    ].join('\n');
    expect(sessionEndingTestsOnSharedState(after)).toEqual([]);
  });
});
