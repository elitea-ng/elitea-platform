// node --test scripts/ci/check-sunset.test.mjs (run by no-binaries.yml)
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  dayNumber,
  decide,
  inspect,
  LEGACY_ITEMS,
  minorsAfter,
  parseConstants,
  REGISTRY,
  REPLACEMENTS,
  utcDay,
} from './check-sunset.mjs';

const SELF = fileURLToPath(import.meta.url);
const REPO = path.resolve(path.dirname(SELF), '../..');
const ALL_LEGACY = LEGACY_ITEMS.map((i) => i.id);
const LANDED = LEGACY_ITEMS.filter((i) => i.landed).map((i) => i.id);
const BOTH = REPLACEMENTS.map((i) => i.id);

const state = (over) => ({
  replacementShippedOn: '',
  sunsetAfter: '',
  commitDay: '2026-12-01',
  legacyPresent: LANDED,
  legacyRemoved: [],
  replacementsPresent: [],
  minorsAfterReplacement: null,
  ...over,
});

/** Runs fn with Date.now and `new Date()` one year ahead, then restores Date. */
function aYearLater(fn) {
  const RealDate = globalThis.Date;
  const shift = 366 * 86_400_000;
  class Shifted extends RealDate {
    constructor(...args) {
      if (args.length === 0) super(RealDate.now() + shift);
      else super(...args);
    }
    static now() {
      return RealDate.now() + shift;
    }
  }
  globalThis.Date = Shifted;
  try {
    assert.ok(new Date().getUTCFullYear() > new RealDate().getUTCFullYear(), 'the clock did not move');
    return fn();
  } finally {
    globalThis.Date = RealDate;
  }
}

// ── Fixture repositories ─────────────────────────────────────────────────────

function repo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sunset-'));
  const g = (args, env = {}) => execFileSync('git', args, { cwd: root, env: { ...process.env, ...env }, encoding: 'utf8' });
  g(['init', '-q', '-b', 'main']);
  g(['config', 'user.email', 'ci@example.invalid']);
  g(['config', 'user.name', 'ci']);
  g(['config', 'commit.gpgsign', 'false']);
  g(['config', 'tag.gpgsign', 'false']);
  const write = (rel, text) => {
    fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true });
    fs.writeFileSync(path.join(root, rel), text);
  };
  const remove = (rel) => fs.rmSync(path.join(root, rel), { force: true });
  const commit = (day, tag) => {
    g(['add', '-A']);
    const date = `${day}T12:00:00+00:00`;
    g(['commit', '-q', '--allow-empty', '-m', `at ${day}`], { GIT_COMMITTER_DATE: date, GIT_AUTHOR_DATE: date });
    if (tag) g(['tag', tag]);
    return g(['rev-parse', 'HEAD']).trim();
  };
  return { root, write, remove, commit, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) };
}

const registry = (shipped, sunset) =>
  `package adminaccess\n\nconst (\n\tReplacementShippedOn = "${shipped}"\n\tSunsetAfter          = "${sunset}"\n)\n`;
const PERMISSIONS = 'services/elitea-main/internal/adminaccess/permissions.go';
const LEGACY_NAMES = 'package adminaccess\n\nconst (\n\tRuntimePlugins = "runtime.plugins"\n\tConfigurationGovernance = "configuration.governance"\n)\n';
const INHERIT = 'services/elitea-main/internal/infra/legacyrbac/postgres.go';
const REPLACED = 'services/elitea-main/internal/adminaccess/replacements.go';
// The reason warning period of R3.8 (#2539), a legacy item since it landed.
const WARNING_PERIOD = 'services/elitea-main/internal/adminaccess/reason.go';
const WARNING_PERIOD_SOURCE = 'package adminaccess\n\nconst ReasonOptionalUntilSunset = true\n';
const REPLACEMENT_SOURCE =
  'package adminaccess\n\nconst (\n\tSealedSignInRoute = "/api/v2/auth/sealed"\n\tReasonMissingHeader = "Elitea-Reason-Missing"\n)\n';

// ── decide: the passing cases ────────────────────────────────────────────────

test('both constants empty, legacy items present: passes', () => {
  assert.deepEqual(decide(state()), []);
});

test('a date in the future with the legacy items present: passes', () => {
  assert.deepEqual(
    decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, commitDay: '2027-02-01' })),
    [],
  );
});

test('legacy items present within the 14-day grace after the date: passes', () => {
  assert.deepEqual(decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, commitDay: '2027-04-15' })), []);
});

test('a commit dated after the date with the legacy items gone and two minors since: passes', () => {
  assert.deepEqual(
    decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, legacyPresent: [], legacyRemoved: LANDED, commitDay: '2027-04-02', minorsAfterReplacement: 2 })),
    [],
  );
});

// ── decide: the four failures ────────────────────────────────────────────────

test('case 1: shipped set with no sunset, less than 90 days, or sunset alone', () => {
  assert.match(decide(state({ replacementShippedOn: '2027-01-01', replacementsPresent: BOTH })).join(), /case 1: .*SunsetAfter is empty/);
  assert.match(decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-03-31', replacementsPresent: BOTH })).join(), /case 1: .*89 days/);
  assert.deepEqual(decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH })), []);
  assert.match(decide(state({ sunsetAfter: '2027-04-01' })).join(), /case 1: SunsetAfter is 2027-04-01 and ReplacementShippedOn is empty/);
  assert.match(decide(state({ sunsetAfter: '2027-02-30' })).join(), /not a YYYY-MM-DD date/);
});

test('case 2: both replacements in the code and no date', () => {
  assert.match(decide(state({ replacementsPresent: BOTH })).join(), /case 2: both replacements/);
  assert.deepEqual(decide(state({ replacementsPresent: BOTH.slice(0, 1) })), []);
});

test('case 3: legacy gone before the date, with no date, or before two stable minors', () => {
  const dated = { replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, legacyPresent: [], legacyRemoved: LANDED };
  assert.match(decide(state({ ...dated, commitDay: '2027-03-31', minorsAfterReplacement: 3 })).join(), /case 3: .*before SunsetAfter/);
  assert.match(decide(state({ legacyPresent: [], legacyRemoved: LANDED, minorsAfterReplacement: 3 })).join(), /case 3: .*SunsetAfter is empty/);
  // One item removed early is a removal too, though the others are still there.
  assert.match(
    decide(state({ ...dated, legacyPresent: LANDED.slice(2), legacyRemoved: LANDED.slice(0, 2), commitDay: '2027-02-01', minorsAfterReplacement: 3 })).join(),
    /case 3: the legacy item\(s\) runtime\.plugins, configuration\.governance are gone .*before SunsetAfter/,
  );
  assert.match(decide(state({ ...dated, commitDay: '2027-05-01', minorsAfterReplacement: 1 })).join(), /case 3: .*1 stable minor/);
  assert.match(decide(state({ ...dated, commitDay: '2027-05-01', minorsAfterReplacement: null })).join(), /case 3: .*no stable release/);
});

test('case 4: a legacy item still there more than 14 days after the date', () => {
  const problems = decide(
    state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, legacyPresent: ['runtime.plugins'], commitDay: '2027-04-16' }),
  );
  assert.match(problems.join(), /case 4: .*15 days after SunsetAfter 2027-04-01.*runtime\.plugins.*one pull request/);
  // A partial removal past the grace still fails case 4 for what it keeps.
  const partial = decide(
    state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, legacyPresent: LANDED.slice(2), legacyRemoved: LANDED.slice(0, 2), commitDay: '2027-04-20', minorsAfterReplacement: 2 }),
  );
  assert.equal(partial.length, 1);
  assert.match(partial[0], /case 4: .*inherit token administration access/);
});

// ── Helpers ──────────────────────────────────────────────────────────────────

test('stable minors count distinct minors above the first release that ships both replacements', () => {
  const tags = [
    { tag: 'v7.6.0', hasReplacements: false },
    { tag: 'v7.7.0', hasReplacements: true },
    { tag: 'v7.7.1', hasReplacements: true },
    { tag: 'v7.8.0-nightly.20270101.1', hasReplacements: true },
    { tag: 'v7.8.0', hasReplacements: true },
  ];
  assert.equal(minorsAfter(tags), 1);
  assert.equal(minorsAfter([...tags, { tag: 'v8.0.0', hasReplacements: true }]), 2);
  assert.equal(minorsAfter(tags.slice(0, 1)), null);
});

test('dates: real days only, the committer day in UTC, constants parsed from Go', () => {
  assert.equal(dayNumber('2027-04-01') - dayNumber('2027-01-01'), 90);
  assert.ok(Number.isNaN(dayNumber('2027-02-29')));
  assert.equal(utcDay('2027-03-01T23:30:00-05:00'), '2027-03-02');
  assert.deepEqual(parseConstants(registry('2027-01-01', '2027-04-01')), { replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01' });
  assert.throws(() => parseConstants('package adminaccess\n'), /does not declare ReplacementShippedOn/);
});

// ── inspect: reading a commit ────────────────────────────────────────────────

test('a commit with no registry predates the rule: inspect answers null', () => {
  const r = repo();
  try {
    r.write('README.md', 'x\n');
    const sha = r.commit('2026-01-01', 'v1.0.0');
    assert.equal(inspect(r.root, sha), null);
  } finally {
    r.cleanup();
  }
});

test('inspect reads the commit, not the working tree, and the tags reachable from it', () => {
  const r = repo();
  try {
    r.write(REGISTRY, registry('', ''));
    r.write(PERMISSIONS, LEGACY_NAMES);
    r.write(INHERIT, 'package legacyrbac\n\nconst (\n\tTokenAdminAccessInherit = "inherit"\n)\n');
    r.write(WARNING_PERIOD, WARNING_PERIOD_SOURCE);
    const before = r.commit('2026-12-01', 'v7.6.0');

    r.write(REPLACED, REPLACEMENT_SOURCE);
    r.write(REGISTRY, registry('2027-01-01', '2027-04-01'));
    r.commit('2027-01-01', 'v7.7.0');
    r.commit('2027-02-01', 'v7.8.0-nightly.20270201.1');
    r.commit('2027-02-02', 'v7.8.0');
    const ninetyDaysLater = r.commit('2027-04-10', 'v8.0.0');

    // The removal leaves a comment that quotes a name: that is not the item.
    r.write(PERMISSIONS, 'package adminaccess\n\n// Formerly "runtime.plugins" and "configuration.governance".\n');
    r.remove(INHERIT);
    r.remove(WARNING_PERIOD);
    const removal = r.commit('2027-04-11');
    // The working tree is not what is checked.
    r.write(PERMISSIONS, LEGACY_NAMES);

    const old = inspect(r.root, before);
    assert.deepEqual(old.legacyPresent, LANDED);
    assert.deepEqual(old.legacyRemoved, []);
    assert.deepEqual(old.replacementsPresent, []);
    assert.equal(old.commitDay, '2026-12-01');
    assert.deepEqual(decide(old), []);

    assert.deepEqual(inspect(r.root, ninetyDaysLater).replacementsPresent, BOTH);
    const gone = inspect(r.root, removal);
    assert.deepEqual(gone.legacyPresent, []);
    assert.deepEqual(gone.legacyRemoved, LANDED);
    assert.equal(gone.minorsAfterReplacement, 2);
    assert.deepEqual(decide(gone), []);

    // The same removal on a branch from v7.8.0 sees one minor only.
    r.remove(PERMISSIONS);
    execFileSync('git', ['checkout', '-q', '-b', 'early', 'v7.8.0'], { cwd: r.root });
    fs.rmSync(path.join(r.root, PERMISSIONS), { force: true });
    fs.rmSync(path.join(r.root, INHERIT), { force: true });
    fs.rmSync(path.join(r.root, WARNING_PERIOD), { force: true });
    const early = r.commit('2027-04-11');
    assert.equal(inspect(r.root, early).minorsAfterReplacement, 1);
    assert.match(decide(inspect(r.root, early)).join(), /case 3: .*1 stable minor/);
  } finally {
    r.cleanup();
  }
});

test('the real tree at HEAD finds every landed row and passes', () => {
  const head = inspect(REPO, 'HEAD');
  assert.ok(head, `${REGISTRY} is in the tree`);
  assert.deepEqual(decide(head), []);
  // Before a date is set no removal can be legitimate, so a landed row that
  // finds nothing is a pattern that stopped matching.
  if (head.sunsetAfter) return;
  for (const item of LEGACY_ITEMS.filter((i) => i.landed)) {
    assert.ok(head.legacyPresent.includes(item.id), `${item.id} (${item.story}) is landed and its pattern finds nothing: a rename would read as removal`);
  }
  for (const item of REPLACEMENTS.filter((i) => i.landed)) {
    assert.ok(head.replacementsPresent.includes(item.id), `${item.id} (${item.story}) is landed and its pattern finds nothing`);
  }
});

// ── No clock ─────────────────────────────────────────────────────────────────

test('the verdicts are the same with the system time moved one year forward', () => {
  const r = repo();
  try {
    r.write(REGISTRY, registry('2027-01-01', '2027-04-01'));
    r.write(PERMISSIONS, LEGACY_NAMES);
    r.write(REPLACED, REPLACEMENT_SOURCE);
    const sha = r.commit('2027-02-01', 'v7.7.0');
    const cases = () => [
      decide(state()),
      decide(state({ replacementShippedOn: '2027-01-01', sunsetAfter: '2027-04-01', replacementsPresent: BOTH, commitDay: '2027-04-16' })),
      decide(state({ legacyPresent: [], legacyRemoved: LANDED, minorsAfterReplacement: 3 })),
      inspect(r.root, sha),
      decide(inspect(r.root, sha)),
    ];
    const now = cases();
    assert.deepEqual(aYearLater(cases), now);
  } finally {
    r.cleanup();
  }
  const source = fs.readFileSync(path.join(path.dirname(SELF), 'check-sunset.mjs'), 'utf8');
  assert.doesNotMatch(source, /Date\.now\(|new Date\(\)/, 'check-sunset.mjs reads the clock');
});
