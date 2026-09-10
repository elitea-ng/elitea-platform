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
 *     do run in an order, inside their own describe. Every OTHER journey is
 *     swept too: a file that runs serially has to be named in
 *     `FILE_LEVEL_SERIAL` and has to still be serial, so the shape stays a
 *     decision rather than something that spread. The newest entry —
 *     `support.widget.spec.ts`, whose four tests compete for one
 *     platform-wide flag and queued instead of running — must also SAY in the
 *     spec why it is exempt.
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
 *     produced — `401 missing authorization header` — read exactly like a
 *     route that was never wired. That reading is closed: the refusal now
 *     says `code: session_revoked` (#538), which `e2e/fixtures/api.ts`'s
 *     `describeRefusal` turns into this rule's name in the report.
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

/**
 * Every directory that holds journey SPECS.
 *
 * `e2e/live` joined the list when the credential-gated lanes landed: rule 3 is
 * about the shared session, and a live journey signs in on the same storage
 * state every other one does. A rule that enumerated only `e2e/journeys` would
 * have gone on reporting "clean" for a directory it never opened — the
 * "absence reads as correctness" shape this file exists to close.
 */
const JOURNEY_ROOTS = ['e2e/journeys', 'e2e/live'];
const REDIRECT_SPEC = 'e2e/journeys/shell/shell.redirect.spec.ts';
const FEATURES_SPEC = 'e2e/journeys/admin/admin.features.spec.ts';
const APP_REQUESTS_SPEC = 'e2e/journeys/admin/admin.app-requests.spec.ts';
const SEED_SCRIPT = 'scripts/e2e-stack.sh';
const WIDGET_SPEC = 'e2e/journeys/support/support.widget.spec.ts';
const ENTRYPOINTS_SPEC = 'e2e/journeys/support/support.entrypoints.spec.ts';
const CONTEXT_BUDGET_SPEC = 'e2e/journeys/chat/chat.contextBudget.spec.ts';

/**
 * Every journey that runs its WHOLE file in one worker, in order.
 *
 * Rule 1 forbids that shape by default, because one failure then reports each
 * test after it as "did not run" — neither a pass nor a failure. The list is
 * an allowlist, not a description: a file that turns itself serial without
 * being named here fails this gate, so the shape stays a decision somebody
 * made rather than one that spread.
 *
 * The five without a `mustSay` predate the rule and carry their reason in
 * their own headers (an ordered generation, an ordered admin form). The sixth
 * is the waiver this gate exists to hold open, and it has to SAY why in the
 * spec, where the next reader of that file meets it: `support.widget.spec.ts`'s
 * four tests all take the same platform-wide flag
 * (`support_assistant_enabled`), and `fullyParallel` puts them on four workers
 * that then queue for it — measured, three `afterAll` hooks over their 120 s
 * budget and one test dead inside `page.goto` after 210 s. One worker has no
 * queue.
 *
 * The seventh is the same shape one layer down. `chat.contextBudget.spec.ts`'s
 * two tests both write `default_context_management`, which hangs off the
 * PERSONA and not off anything either test created, and the second one opens
 * by writing back the very value the first one has just replaced. On two
 * workers that is a suite writing over its own assertion, and it reported
 * itself as a product defect ("a changed profile budget must reach the
 * panel") in both engines. It has to SAY that, because the next reader's
 * first instinct — the one the previous fix took — is a browser cache.
 */
const FILE_LEVEL_SERIAL = [
  { path: 'e2e/journeys/deepwiki/deepwiki.real-engine.spec.ts' },
  { path: 'e2e/journeys/deepwiki/deepwiki.generation.spec.ts' },
  { path: 'e2e/journeys/admin/admin.schedules.spec.ts' },
  { path: 'e2e/journeys/admin/admin.configuration.spec.ts' },
  { path: APP_REQUESTS_SPEC },
  { path: WIDGET_SPEC, mustSay: /ONE WORKER, IN ORDER/ },
  { path: ENTRYPOINTS_SPEC, mustSay: /ONE PLATFORM FLAG, IN ORDER/ },
  { path: CONTEXT_BUDGET_SPEC, mustSay: /ONE ACCOUNT, ONE WRITER/ },
];

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
  for (const root of JOURNEY_ROOTS) walk(root);
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

  it('no journey runs its whole file serially without being named here', () => {
    const allowed = new Set(FILE_LEVEL_SERIAL.map((entry) => entry.path));
    const offenders = journeySpecs()
      .filter(([path, source]) => fileLevelSerialLines(source).length > 0 && !allowed.has(path))
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });

  it('every entry on that list is really serial, and the waiver says why', () => {
    for (const { path, mustSay } of FILE_LEVEL_SERIAL) {
      const source = read(path);
      // A listed file that is no longer serial is a stale exemption, and the
      // next file to take that name would inherit it in silence.
      expect(fileLevelSerialLines(source), `${path} is listed as serial but is not`).toHaveLength(1);
      if (mustSay === undefined) continue;
      // …and the reason is IN the spec, where its next reader meets it.
      expect(source, `${path} must state why it runs in one worker`).toMatch(mustSay);
    }
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

/* ── rule 4 ─────────────────────────────────────────────────────────────── */

/**
 * Does this `playwright.config.ts` let the persona sign-ins run together?
 *
 * Reads the `setup` project's OWN block — from its `name: 'setup'` line to the
 * line that closes it at the same indent — because `fullyParallel` appears on
 * three other projects in the same file and a whole-file search would be
 * satisfied by any of them.
 */
export function setupProjectRunsInParallel(source) {
  const lines = source.split('\n');
  const start = lines.findIndex((line) => /name:\s*'setup'/.test(line));
  if (start === -1) return true;
  const indent = (lines[start].match(/^\s*/) ?? [''])[0].length;
  const body = [];
  for (let index = start; index < lines.length; index += 1) {
    body.push(lines[index]);
    if (index > start && /^\s*\},?\s*$/.test(lines[index]) && (lines[index].match(/^\s*/) ?? [''])[0].length < indent) {
      break;
    }
  }
  return !/fullyParallel:\s*false/.test(body.join('\n'));
}

/**
 * Answers `readPersonalProjectId` accepts as "this persona owns a personal
 * project", ignoring prose.
 *
 * The rule is about ONE of them: the seeded project id. `resolvePersonalProjectID`
 * answers it for any caller that holds a role on project 1 and owns nothing of
 * its own, so a poll that stops at the first non-empty answer stops instantly
 * and proves nothing.
 */
export function personalProjectPollAcceptsSeededId(source) {
  const body = source.slice(source.indexOf('async function readPersonalProjectId'));
  if (body === '') return true;
  const code = body
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => !line.startsWith('*') && !line.startsWith('//') && !line.startsWith('/*'))
    .join('\n');
  return !code.includes('DEFAULT_PROJECT_ID');
}

describe('#839 — every persona must leave the setup owning a personal project', () => {
  /*
   * Sign-in only ASKS for the personal project. The provisioner runs one
   * attempt at a time and DROPS the rest, and `GET /social/author` hides the
   * absence behind the seeded project 1, which also stops it re-arming the
   * provisioner. So the persona that lost the race owns nothing for the whole
   * run — and the suite still passes, in the other of two self-consistent
   * worlds. Six visual baselines flipped between those worlds from run to run
   * with identical pixel counts each time.
   */
  it('the three sign-ins do not compete for the single provisioning slot', () => {
    expect(setupProjectRunsInParallel(read('playwright.config.ts'))).toBe(false);
  });

  it('rejects the setup project shape that let them run together', () => {
    const before = [
      '  projects: [',
      '    {',
      "      name: 'setup',",
      '      testMatch: /auth\\.setup\\.ts/,',
      '      use: { ...devices },',
      '    },',
      '    {',
      "      name: 'chromium',",
      '      fullyParallel: false,',
      '    },',
      '  ],',
    ].join('\n');
    expect(setupProjectRunsInParallel(before)).toBe(true);
  });

  it('the personal-project wait rejects the seeded project as an answer', () => {
    expect(personalProjectPollAcceptsSeededId(read('e2e/auth.setup.ts'))).toBe(false);
  });

  it('rejects the poll that stopped at the first non-empty answer', () => {
    const before = [
      'async function readPersonalProjectId(page) {',
      '  let id;',
      '  await expect',
      '    .poll(async () => {',
      "      const author = await page.request.get(BASE_URL + '/api/v2/social/author/');",
      "      if (!author.ok()) return '';",
      "      id = (await author.json()).personal_project_id;",
      "      return id ?? '';",
      '    })',
      "    .not.toBe('');",
      '  return id;',
      '}',
    ].join('\n');
    expect(personalProjectPollAcceptsSeededId(before)).toBe(true);
  });

  it('reads a comment naming the seeded id as prose, not as a check', () => {
    const commentOnly = [
      'async function readPersonalProjectId(page) {',
      '  // DEFAULT_PROJECT_ID is what the fallback answers.',
      "  return '';",
      '}',
    ].join('\n');
    expect(personalProjectPollAcceptsSeededId(commentOnly)).toBe(true);
  });
});

/* ── rule 5 ─────────────────────────────────────────────────────────────── */

/**
 * The live lanes' contract, stated where it can be read in one second.
 *
 * `e2e/live` holds the journeys ported from the legacy suite's
 * credential-bound cases. The wave rule they exist for is that a legacy test
 * is REUSED wherever its prerequisites exist and never parked behind a
 * permanent skip — and a skip is exactly what a tired author reaches for when
 * a credential is missing. Two properties keep that honest, and neither is
 * visible from inside any one spec:
 *
 *  1. NO `test.skip` under `e2e/live`. A skipped test is reported as a
 *     pass-shaped row; an unconfigured provider must contribute zero rows.
 *  2. The config GATES the lanes on the environment. Without the gate every
 *     live spec would list on every run and fail for want of a secret, which
 *     reads as a broken suite rather than as an unconfigured machine.
 */
export function liveSkips(specs) {
  return specs
    .filter(([path]) => path.startsWith('e2e/live/'))
    .flatMap(([path, source]) =>
      source
        .split('\n')
        .map((line, index) => ({ line: line.trim(), number: index + 1 }))
        // A comment that explains the rule is prose, not a skip.
        .filter((entry) => !entry.line.startsWith('*') && !entry.line.startsWith('//'))
        .filter((entry) => /\btest\.skip\s*\(/.test(entry.line) || /\btest\.fixme\s*\(/.test(entry.line))
        .map((entry) => `${path}:${entry.number}`),
    );
}

/** Does the config compute a live project's `testIgnore` from the environment? */
export function liveProjectsAreEnvironmentGated(source) {
  return (
    source.includes('testIgnore: unconfiguredLiveToolkitSpecs()') &&
    source.includes("testIgnore: liveImageModel() === '' ? [LIVE_IMAGE_SPECS] : []")
  );
}

describe('the live lanes must never claim coverage they did not take', () => {
  it('no journey under e2e/live skips itself', () => {
    expect(liveSkips(journeySpecs())).toEqual([]);
  });

  it('rejects the shape a missing credential invites', () => {
    const before = [
      "test('LIVE-TK-github-1: …', async ({ page }) => {",
      "  test.skip(!process.env.E2E_LIVE_GITHUB_TOKEN, 'no GitHub token');",
      '});',
    ].join('\n');
    expect(liveSkips([['e2e/live/toolkits.github.spec.ts', before]])).toEqual([
      'e2e/live/toolkits.github.spec.ts:2',
    ]);
  });

  it('reads a comment about skipping as prose', () => {
    const commentOnly = ['// There is no test.skip( here, and there must never be one.'].join('\n');
    expect(liveSkips([['e2e/live/README.spec.ts', commentOnly]])).toEqual([]);
  });

  it('both live projects take their testIgnore from the environment', () => {
    expect(liveProjectsAreEnvironmentGated(read('playwright.config.ts'))).toBe(true);
  });

  it('rejects a live project that lists its specs unconditionally', () => {
    const before = [
      "      name: 'toolkits-live',",
      '      testMatch: /live\\/toolkits\\..+\\.spec\\.ts/,',
      '      fullyParallel: false,',
    ].join('\n');
    expect(liveProjectsAreEnvironmentGated(before)).toBe(false);
  });
});

/* ── rule 6 ─────────────────────────────────────────────────────────────── */

/**
 * The destructive fixture self-test must run where it can neither meet a
 * running journey NOR take one down with it.
 *
 * API-FX3 calls `sweepAutotestEntities`, which deletes every `autotest_`
 * conversation, agent and pipeline in the shared project. Two properties are
 * needed, and the first attempt bought one by giving up the other:
 *
 *  1. IT MUST NOT RUN BESIDE THE JOURNEYS IT WOULD SWEEP. Run in `chromium`
 *     or `webkit` it would delete, at an unpredictable moment, whatever
 *     twenty other journeys were holding, and every one of them would fail on
 *     a 404 for a row it created — with no cause anywhere near the failure.
 *  2. ITS OWN FAILURE MUST NOT SKIP ANYTHING. It first ran in an ordering
 *     project that both engines DEPENDED on. A dependency's failure skips its
 *     dependents, so one failing assertion here reported `308 did not run`
 *     and the whole browser suite went unmeasured (measured on b705c058).
 *
 * `teardown` gives both. Playwright runs a teardown project after its parent
 * and after everything that depends on the parent, so the sweep meets a
 * finished run rather than a starting one — and nothing depends on IT, so it
 * reports its own failure and skips nobody.
 *
 * The rule therefore checks four things: the file is named, it owns a
 * project, `setup` declares that project as its teardown, and NO project
 * lists it as a dependency.
 */
export function sweepJourneyIsIsolated(source) {
  const named = source.includes(
    'const FIXTURE_ISOLATION_JOURNEY = /journeys\\/api\\/api\\.fixture-isolation\\.spec\\.ts/',
  );
  const owned = source.includes("name: 'fixtures-sweep'");
  const isTeardown = /teardown:\s*'fixtures-sweep'/.test(source);
  // A dependency edge is exactly what makes a failure here skip a suite.
  const dependedOn = /dependencies:\s*\[[^\]]*'fixtures-sweep'/.test(source);
  // Once per engine project: neither may pick the file up itself.
  const ignored = (source.match(/^\s*FIXTURE_ISOLATION_JOURNEY,$/gm) ?? []).length;
  return named && owned && isTeardown && !dependedOn && ignored === 2;
}

describe('the sweep must not run beside the journeys it would sweep', () => {
  it('the fixture self-tests own a teardown project and are ignored by both engines', () => {
    expect(sweepJourneyIsIsolated(read('playwright.config.ts'))).toBe(true);
  });

  it('rejects a config that let one engine pick the sweep up', () => {
    const before = [
      "const FIXTURE_ISOLATION_JOURNEY = /journeys\\/api\\/api\\.fixture-isolation\\.spec\\.ts/;",
      "      teardown: 'fixtures-sweep',",
      "      name: 'fixtures-sweep',",
      '        FIXTURE_ISOLATION_JOURNEY,',
    ].join('\n');
    expect(sweepJourneyIsIsolated(before)).toBe(false);
  });

  /*
   * The shape that actually failed. Every other property held — the file was
   * named, it owned a project, both engines ignored it — and the dependency
   * edge alone turned one red assertion into an unmeasured suite.
   */
  it('rejects the dependency edge that made one failure skip 308 journeys', () => {
    const before = [
      "const FIXTURE_ISOLATION_JOURNEY = /journeys\\/api\\/api\\.fixture-isolation\\.spec\\.ts/;",
      "      name: 'fixtures-sweep',",
      "      dependencies: ['setup', 'toolkits-empty', 'fixtures-sweep'],",
      '        FIXTURE_ISOLATION_JOURNEY,',
      '        FIXTURE_ISOLATION_JOURNEY,',
    ].join('\n');
    expect(sweepJourneyIsIsolated(before)).toBe(false);
  });

  it('accepts the teardown shape', () => {
    const after = [
      "const FIXTURE_ISOLATION_JOURNEY = /journeys\\/api\\/api\\.fixture-isolation\\.spec\\.ts/;",
      "      name: 'setup',",
      "      teardown: 'fixtures-sweep',",
      "      name: 'fixtures-sweep',",
      "      dependencies: ['setup', 'toolkits-empty'],",
      '        FIXTURE_ISOLATION_JOURNEY,',
      '        FIXTURE_ISOLATION_JOURNEY,',
    ].join('\n');
    expect(sweepJourneyIsIsolated(after)).toBe(true);
  });
});

/* ── rule 7 ─────────────────────────────────────────────────────────────── */

/**
 * The publish journeys' tenancy has to be SEEDED where the fixtures look for
 * it, and the two files have to agree on the names.
 *
 * `e2e/fixtures/api.ts` resolves the author project and the non-shared model
 * BY NAME — deliberately, so no journey repeats an id the seed picks out of a
 * reserved range. That makes the name a contract between two files that are
 * edited for different reasons and never run together: rename the project in
 * the seed and every publish journey fails at run time on CI with "the caller
 * is a member of no project of that name", which reads like a broken stack
 * rather than a rename.
 *
 * Three properties, and the third is the one that only fails on a FIRST seed:
 *
 *  1. the seed creates a project of the name the fixture asks for, and grants
 *     BOTH personas a membership in it — the API journeys run on the admin
 *     state while the project is member-owned, so one membership is not enough;
 *  2. the seed creates the model the fixture asks for, `shared = false`, which
 *     is what makes the private-model refusals refuse for their own reason;
 *  3. the model row is written AFTER the `DO $schemas$` loop that creates
 *     `p_<id>`. Written with the project rows it would fail on a missing
 *     relation the first time the seed ever runs, and pass on every re-run —
 *     the shape the schema loop's own comment records.
 */
export function publishTenancySeeded(seedSource, fixtureSource) {
  const projectName = /PUBLISH_AUTHOR_PROJECT_NAME = '([^']+)'/.exec(fixtureSource)?.[1] ?? '';
  const modelName = /PRIVATE_MODEL_NAME = '([^']+)'/.exec(fixtureSource)?.[1] ?? '';
  if (projectName === '' || modelName === '') return false;

  const projectInsert = seedSource.indexOf(`'${projectName}'`);
  if (projectInsert < 0) return false;

  // The loop is found by its BODY, not by the `DO $schemas$` label: the
  // label is quoted in a comment near the top of the seed, and an index that
  // stopped there would put the whole file "after the loop".
  const schemaLoop = seedSource.indexOf('PERFORM create_tenant_schema(');
  if (schemaLoop < 0 || schemaLoop < projectInsert) return false;
  const projectBlock = seedSource.slice(projectInsert, schemaLoop);
  const grantsBothPersonas =
    projectBlock.includes('e2e-member@autotest.local') &&
    projectBlock.includes('e2e-admin@autotest.local');

  const modelInsert = seedSource.indexOf(`'${modelName}'`);
  const modelIsPrivate = /shared = false|, false,/.test(
    seedSource.slice(modelInsert, modelInsert + 600),
  );
  return grantsBothPersonas && modelInsert > schemaLoop && modelIsPrivate;
}

describe('the publish journeys must find the tenancy their fixtures resolve', () => {
  it('the seed provisions the author project and the non-shared model', () => {
    expect(publishTenancySeeded(read(SEED_SCRIPT), read('e2e/fixtures/api.ts'))).toBe(true);
  });

  it('rejects a seed that renamed the project the fixtures ask for', () => {
    const seed = read(SEED_SCRIPT).replaceAll("'e2e-publish-author'", "'e2e-publishing'");
    expect(publishTenancySeeded(seed, read('e2e/fixtures/api.ts'))).toBe(false);
  });

  it('rejects a seed that grants only the owning persona a membership', () => {
    const seed = read(SEED_SCRIPT).replace(
      "WHERE u.email IN ('e2e-member@autotest.local', 'e2e-admin@autotest.local')\nON CONFLICT (project_id, user_id, role_id) DO NOTHING;\n\n-- The empty vault pair",
      "WHERE u.email = 'e2e-member@autotest.local'\nON CONFLICT (project_id, user_id, role_id) DO NOTHING;\n\n-- The empty vault pair",
    );
    expect(publishTenancySeeded(seed, read('e2e/fixtures/api.ts'))).toBe(false);
  });

  it('rejects the model row written before the schema loop that creates its schema', () => {
    const seed = read(SEED_SCRIPT);
    const row = "'e2e-private-model-llm', 'E2E-PRIVATE-MODEL'";
    const moved = seed
      .replace(row, 'ROW MOVED')
      .replace('PERFORM create_tenant_schema(', `${row}\nPERFORM create_tenant_schema(`);
    expect(publishTenancySeeded(moved, read('e2e/fixtures/api.ts'))).toBe(false);
  });
});

/* ── rule 8 ─────────────────────────────────────────────────────────────── */

/**
 * The seeded project's `secrets_header_value` is made by the STACK, and the
 * setup only asserts it.
 *
 * ## What the value is
 *
 * `secrets_header_value` is the `X-SECRET` the expanded version-details read
 * compares against — the call the SDK worker materializes a nested agent with,
 * and the one three API journeys authenticate with. A project without one
 * refuses every caller with 403 `This project has no secrets_header_value
 * secret`. It lives in the project's Fernet vault, so only elitea-main can
 * write it: `BackfillProjectSecretsHeaderValues` gives one to every row of
 * `centry.project` before the listeners bind. On this stack that pass ran at
 * `up`, against a database with no `centry` schema at all, so it saw nothing —
 * which is why the seed has to restart elitea-main after writing its rows.
 *
 * ## Why it is a rule and not a comment
 *
 * `auth.setup.ts` used to mint the value through the API, best-effort, with
 * the failure swallowed to a `console.warn`. That put a WRITE on the critical
 * path of a SCREENSHOT: the single row of the `settings-secrets` visual
 * baseline IS this secret, so a mint that failed for a reason of its own — a
 * persona without `configuration.secrets.secret.create`, a route absent from
 * the image under test — left the page drawing "No secrets" and the run
 * reported a visual diff on the Secrets page. The line that named the cause
 * sat in the setup log, which nobody reads when a picture has changed.
 *
 * Three properties, and each one alone restores that reading:
 *
 *  1. the seed restarts elitea-main AFTER it has written the project vaults —
 *     a restart before them repeats the pass that saw nothing;
 *  2. it PROVES the pass wrote one, by watching project 1's vault blob change,
 *     rather than only waiting for the container to report healthy: the pass
 *     logs a warning and returns success for a project it skipped;
 *  3. `auth.setup.ts` READS and asserts, and does not mint or swallow. A setup
 *     that repairs the absence cannot detect it, and the next reader of an
 *     empty Secrets page is a baseline diff again.
 */
export function runtimeSecretHeaderSeeded(seedSource, setupSource) {
  const vaultInsert = seedSource.indexOf('INSERT INTO centry.secrets_data');
  const restart = seedSource.indexOf('$COMPOSE_F restart elitea-main');
  if (vaultInsert < 0 || restart < 0 || restart < vaultInsert) return false;

  // The digest of project 1's vault, and a bounded wait on it AFTER the
  // restart. The reader is a function defined above the restart, so the wait is
  // what has to sit below it — a seed that reads the digest once and never
  // again proves the container came back, not that the pass wrote anything.
  const readsTheDigest = seedSource.includes(
    "md5(data) FROM centry.secrets_data WHERE id = 'project-1'",
  );
  const waitsForIt = seedSource.slice(restart).includes('project_one_vault_digest');
  if (!readsTheDigest || !waitsForIt) return false;

  // The IMPORT and the CALL, not the prose: the doc comment on the assertion
  // names `resolveProjectSecretHeader` on purpose — it says where the mint went
  // — and a rule that read prose would forbid the sentence that explains it.
  if (/import\s*\{[^}]*resolveProjectSecretHeader[^}]*\}/.test(setupSource)) return false;
  const helperAt = setupSource.indexOf('async function assertRuntimeSecretHeader');
  if (helperAt < 0) return false;
  const helper = setupSource.slice(helperAt);
  if (helper.includes('console.warn') || helper.includes('resolveProjectSecretHeader(')) {
    return false;
  }
  return helper.includes('readProjectSecretHeader') && helper.includes('expect(');
}

describe("the seeded project's X-SECRET value is a property of the stack", () => {
  const SETUP = 'e2e/auth.setup.ts';

  it('the seed mints it and the setup asserts it', () => {
    expect(runtimeSecretHeaderSeeded(read(SEED_SCRIPT), read(SETUP))).toBe(true);
  });

  it('rejects a seed that never restarts elitea-main', () => {
    const seed = read(SEED_SCRIPT).replace(
      '$COMPOSE_BIN $COMPOSE_F restart elitea-main',
      '# restart removed',
    );
    expect(runtimeSecretHeaderSeeded(seed, read(SETUP))).toBe(false);
  });

  it('rejects a restart that runs before the vaults it is meant to fill', () => {
    const seed = read(SEED_SCRIPT).replace(
      '$COMPOSE_BIN $COMPOSE_F restart elitea-main',
      '# moved',
    );
    const moved = seed.replace(
      'INSERT INTO centry.secrets_data',
      '$COMPOSE_BIN $COMPOSE_F restart elitea-main\nINSERT INTO centry.secrets_data',
    );
    expect(runtimeSecretHeaderSeeded(moved, read(SETUP))).toBe(false);
  });

  it('rejects a restart that waits for health instead of for the write', () => {
    const seed = read(SEED_SCRIPT).replaceAll('md5(data)', 'length(data)');
    expect(runtimeSecretHeaderSeeded(seed, read(SETUP))).toBe(false);
  });

  it('rejects a seed that reads the digest once and never waits on it', () => {
    const seed = read(SEED_SCRIPT);
    const poll = seed.indexOf('$COMPOSE_BIN $COMPOSE_F restart elitea-main');
    const trimmed =
      seed.slice(0, poll) +
      seed.slice(poll).replaceAll('project_one_vault_digest', 'true # digest wait removed');
    expect(runtimeSecretHeaderSeeded(trimmed, read(SETUP))).toBe(false);
  });

  it('rejects the best-effort mint the visual gate used to report for', () => {
    const before = [
      'async function ensureRuntimeSecretHeader(page) {',
      '  try {',
      '    await resolveProjectSecretHeader(page.request, DEFAULT_PROJECT_ID);',
      '  } catch (error) {',
      '    console.warn(`[auth.setup] could not pre-mint the project secret header`);',
      '  }',
      '}',
    ].join('\n');
    expect(runtimeSecretHeaderSeeded(read(SEED_SCRIPT), before)).toBe(false);
  });

  it('rejects an assertion that swallows its own failure', () => {
    const swallowed = read(SETUP).replace(
      'expect(value, explain).toBeTruthy();',
      'if (!value) console.warn(explain);',
    );
    expect(runtimeSecretHeaderSeeded(read(SEED_SCRIPT), swallowed)).toBe(false);
  });
  it('rejects a setup that mints through the resolver instead of reading', () => {
    const setup = read(SETUP)
      .replace(
        "import { readProjectSecretHeader, SECRETS_HEADER_NAME } from './fixtures/api';",
        "import { resolveProjectSecretHeader, SECRETS_HEADER_NAME } from './fixtures/api';",
      )
      .replace(
        'await readProjectSecretHeader(page.request, DEFAULT_PROJECT_ID)',
        'await resolveProjectSecretHeader(page.request, DEFAULT_PROJECT_ID)',
      );
    expect(runtimeSecretHeaderSeeded(read(SEED_SCRIPT), setup)).toBe(false);
  });
});
