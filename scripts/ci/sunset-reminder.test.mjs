// node --test scripts/ci/sunset-reminder.test.mjs (run by no-binaries.yml)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { REGISTRY } from './check-sunset.mjs';
import { CREATOR, decide, REMIND_DAYS, restClient, run, title } from './sunset-reminder.mjs';

function tree(sunsetAfter, { legacy = true } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'sunset-reminder-'));
  fs.mkdirSync(path.dirname(path.join(root, REGISTRY)), { recursive: true });
  fs.writeFileSync(
    path.join(root, REGISTRY),
    `package adminaccess\n\nconst (\n\tReplacementShippedOn = "${sunsetAfter ? '2027-01-01' : ''}"\n\tSunsetAfter = "${sunsetAfter}"\n)\n`,
  );
  if (legacy) {
    fs.writeFileSync(
      path.join(path.dirname(path.join(root, REGISTRY)), 'permissions.go'),
      'package adminaccess\n\nconst RuntimePlugins = "runtime.plugins"\n',
    );
  }
  return root;
}

/** A fake GitHub with one issue list; findTitles answers what create wrote. */
function fakeGitHub() {
  const issues = [];
  return {
    issues,
    async findTitles(wanted) {
      return issues.map((i) => i.title).filter((t) => t === wanted);
    },
    async create(issue) {
      issues.push(issue);
    },
  };
}

test('31 days before nothing; 30 days before one issue; a second run opens none', async () => {
  const root = tree('2027-04-01');
  try {
    const github = fakeGitHub();
    assert.equal((await run({ root, today: '2027-03-01', github })).open, false);
    assert.equal(github.issues.length, 0);

    assert.equal((await run({ root, today: '2027-03-02', github })).open, true);
    assert.equal(github.issues.length, 1);
    assert.equal(github.issues[0].title, title('2027-04-01'));
    assert.match(github.issues[0].body, /runtime\.plugins/);

    assert.equal((await run({ root, today: '2027-03-02', github })).open, false);
    assert.equal((await run({ root, today: '2027-03-20', github })).open, false);
    assert.equal(github.issues.length, 1);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('no date set: nothing to remind of, and GitHub is not asked', async () => {
  const root = tree('');
  try {
    const github = {
      async findTitles() {
        throw new Error('asked');
      },
      async create() {
        throw new Error('created');
      },
    };
    assert.equal((await run({ root, today: '2027-03-02', github })).open, false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('the legacy items gone: nothing, and GitHub is not asked', async () => {
  const root = tree('2027-04-01', { legacy: false });
  try {
    const github = fakeGitHub();
    github.findTitles = async () => {
      throw new Error('asked');
    };
    const decision = await run({ root, today: '2027-03-20', github });
    assert.equal(decision.open, false);
    assert.match(decision.reason, /gone/);
    assert.equal(github.issues.length, 0);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('the window: REMIND_DAYS before the date and after it, until an issue exists', () => {
  assert.equal(REMIND_DAYS, 30);
  assert.equal(decide({ today: '2027-04-20', sunsetAfter: '2027-04-01', existing: [] }).open, true);
  assert.equal(decide({ today: '2027-04-20', sunsetAfter: '2027-04-01', existing: [title('2027-04-01')] }).open, false);
  // An issue for an earlier date is not this date's issue.
  assert.equal(decide({ today: '2027-04-20', sunsetAfter: '2027-04-01', existing: [title('2027-03-01')] }).open, true);
  assert.throws(() => decide({ today: '2027-04-20', sunsetAfter: '2027-13-01', existing: [] }), /not a YYYY-MM-DD/);
});

test('the REST client lists the workflow\'s issues in any state, page by page, exact titles only', async () => {
  const calls = [];
  const wanted = title('2027-04-01');
  const filler = (n) => Array.from({ length: n }, (_, i) => ({ title: `other ${i}` }));
  const pages = [filler(100), [...filler(3), { title: `${wanted} (old)` }, { title: wanted, pull_request: {} }, { title: wanted }]];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    const json = url.includes('/issues?') ? pages[Number(new URL(url).searchParams.get('page')) - 1] : {};
    return { ok: true, json: async () => json, text: async () => '' };
  };
  const client = restClient({ token: 't', repository: 'o/r', fetchImpl });
  assert.deepEqual(await client.findTitles(wanted), [wanted]);
  assert.equal(calls.length, 2);
  const first = new URL(calls[0].url);
  assert.equal(first.pathname, '/repos/o/r/issues');
  assert.equal(first.searchParams.get('state'), 'all');
  assert.equal(first.searchParams.get('creator'), CREATOR);
  await client.create({ title: 'x', body: 'y' });
  assert.equal(calls[2].init.method, 'POST');
  assert.match(calls[2].url, /\/repos\/o\/r\/issues$/);
  assert.equal(calls[2].init.headers.authorization, 'Bearer t');
});
