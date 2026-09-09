/**
 * The agent READ contract: what the list says about an agent, and what a
 * version read says about itself.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's application READ cases — the list envelope, the
 * authors on every row, the five fields a row must carry, the author of a new
 * version, and a fresh version's fork flag. Its sibling
 * `api.agent-version-contract.spec.ts` covers the WRITE half.
 *
 * They are API-only by nature: each is a statement about a projection, and
 * three of the five fields the list must carry are read by no screen at all —
 * `owner_id`, `is_forked` and `has_interrupt` are consumed by the SDK and by
 * the publish paths, so no rendered row can say whether they are right.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TWO DEFECTS THESE JOURNEYS FOUND
 * ─────────────────────────────────────────────────────────────────────────────
 * `updated_at` was the zero time on EVERY agent in every deployment. The field
 * has always been on the wire; `applications` had no such column, and nothing
 * scanned one, so every client that shows or sorts by "last modified" was
 * reading the constant "0001-01-01T00:00:00Z". The contract even documented it
 * that way, which made it permanent rather than fixed.
 *
 * The version read omitted the `author` object the version WRITE answers. The
 * agent editor reloads through that read, so the name beside a version
 * survived until the page was refreshed and then became an id.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE LIST READS ARE NAMED, NOT PAGED
 * ─────────────────────────────────────────────────────────────────────────────
 * Both engines run this file at the same time as every other journey, all of
 * them creating agents in the same project. A read that asks for the first
 * page and hopes its own rows are on it is a read that fails when the project
 * grows — so every list read here sends the list's own `query` filter and
 * matches on its own `autotest_` name.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every journey creates its own `autotest_*` agents through the API
 * and deletes them in a `finally`, and no journey reads another's rows or
 * depends on the order they run in.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  agentVersionBody,
  createAgentWithVersion,
  deleteAgent,
  readAgentList,
  readCallerIdentity,
  readVersion,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const versionURL = (applicationId: string, versionId: string): string =>
  `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId}/${versionId}`;

/* ── the list envelope and the fields every row carries ───────────────────── */

test('the agent list answers a total and rows envelope', async ({ request }) => {
  const family = autotestName('envelope');
  const first = await createAgentWithVersion(request, `${family}_a`, {});
  const second = await createAgentWithVersion(request, `${family}_b`, {});
  try {
    const listed = await readAgentList(request, { query: family });

    expect(typeof listed.body['total'], `the list answered ${JSON.stringify(listed.body).slice(0, 200)}`).toBe('number');
    expect(Array.isArray(listed.body['rows'])).toBe(true);
    // Both agents this journey made are in it, so the envelope is describing
    // a real page and not an empty one that happens to have the right keys.
    const names = listed.rows.map((row) => String(row['name'] ?? ''));
    expect(names).toContain(`${family}_a`);
    expect(names).toContain(`${family}_b`);
    expect(listed.body['total'] as number).toBeGreaterThanOrEqual(2);
    // The paging keys the envelope declares. A client that pages reads these.
    for (const key of ['page', 'page_size', 'total_pages']) {
      expect(typeof listed.body[key], `the envelope carries no ${key}`).toBe('number');
    }
  } finally {
    await deleteAgent(request, first.id);
    await deleteAgent(request, second.id);
  }
});

test('every listed agent carries an author with an id, an email and a name', async ({
  request,
}) => {
  const family = autotestName('authors');
  const agent = await createAgentWithVersion(request, `${family}_a`, {});
  try {
    const caller = await readCallerIdentity(request);
    const listed = await readAgentList(request, { query: family });
    expect(listed.rows.length).toBeGreaterThan(0);

    for (const row of listed.rows) {
      const authors = row['authors'] as readonly Record<string, unknown>[] | undefined;
      expect(authors, `agent ${String(row['name'])} carries no authors`).toBeDefined();
      expect(Array.isArray(authors)).toBe(true);
      expect((authors ?? []).length).toBeGreaterThan(0);
      for (const author of authors ?? []) {
        expect(author['id']).toBeTruthy();
        expect(author['email']).toBeTruthy();
        expect(author['name']).toBeTruthy();
      }
    }

    // …and the author is the ACCOUNT that created the agent, not the project.
    // The join used to read `applications.owner_id`, which is the owning
    // project, so the list named whichever account's user id happened to
    // equal the project id — user 1 in nearly every deployment.
    const mine = listed.rows.find((row) => row['name'] === `${family}_a`);
    const authors = (mine?.['authors'] as readonly Record<string, unknown>[]) ?? [];
    expect(String(authors[0]?.['id'] ?? '')).toBe(caller.id);
    expect(String(authors[0]?.['email'] ?? '')).toBe(caller.email);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('every listed agent carries owner_id, is_forked, meta, has_interrupt and agent_type', async ({
  request,
}) => {
  const family = autotestName('rowfields');
  const agent = await createAgentWithVersion(request, `${family}_a`, { agentType: 'openai' });
  try {
    const listed = await readAgentList(request, { query: family });
    const row = listed.rows.find((entry) => entry['name'] === `${family}_a`);
    expect(row, 'the agent this journey created is not in its own filtered list').toBeDefined();

    // The owning PROJECT, not the caller. Three routes disagreed about this
    // field's meaning at one point, and the SDK reads it as the project.
    expect(row?.['owner_id']).toBe(DEFAULT_PROJECT_ID);
    expect(row?.['is_forked'], 'an agent created here is nobody’s fork').toBe(false);
    expect(row?.['meta'], 'meta must be an object, not null').not.toBeNull();
    expect(typeof row?.['meta']).toBe('object');
    expect(row?.['has_interrupt']).toBe(false);
    expect(row?.['agent_type']).toBeTruthy();
    // `tags` is always present, as an array, even when empty.
    expect(Array.isArray(row?.['tags'])).toBe(true);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/* ── what a version says about itself ─────────────────────────────────────── */

test('a new version reports its author, and reports it again on reload', async ({ request }) => {
  const agent = await createAgentWithVersion(request, autotestName('versionauthor'), {});
  try {
    const caller = await readCallerIdentity(request);

    // The READ, not the write echo. The write has always answered an author
    // object; this read answered `author_id` and nothing else, so the editor
    // showed a name until it reloaded and then showed a number.
    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.authorId).toBe(caller.id);

    const detail = await request.get(versionURL(agent.id, agent.versionId));
    const body = (await detail.json()) as { author?: Record<string, unknown> };
    expect(
      body.author,
      'the version read carries no author object — the editor reloads through this read',
    ).toBeDefined();
    expect(String(body.author?.['id'] ?? '')).toBe(caller.id);
    expect(String(body.author?.['email'] ?? '')).toBe(caller.email);
    expect(String(body.author?.['name'] ?? ''), 'the author has no name').not.toBe('');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

test('a freshly created version is not marked as forked', async ({ request }) => {
  const name = autotestName('notforked');
  const response = await request.post(
    `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        name,
        description: `${AUTOTEST_PREFIX}fork flag`,
        type: 'agent',
        versions: [agentVersionBody({ name: 'base', agentType: 'openai' })],
      },
    },
  );
  expect(response.status()).toBe(201);
  const created = (await response.json()) as {
    id?: string;
    version_details?: Record<string, unknown>;
  };
  try {
    expect(
      created.version_details?.['is_forked'],
      'a version nobody forked is reported as forked; the flag is computed from the fork provenance in meta',
    ).toBe(false);
    // The agent row agrees. `is_forked` there is computed from a different
    // column (`applications.shared_id`), so the two can disagree.
    const listed = await readAgentList(request, { query: name });
    expect(listed.rows.find((row) => row['name'] === name)?.['is_forked']).toBe(false);
  } finally {
    await deleteAgent(request, String(created.id ?? ''));
  }
});

/* ── the last-modified stamp ──────────────────────────────────────────────── */

/**
 * `updated_at` must move when the agent is saved.
 *
 * It was the Go zero time on every row — the field is always on the wire and
 * there was no column behind it — so this journey's first assertion is that a
 * brand-new agent reports a real date at all, and its second is that an
 * ordinary save moves it. A constant satisfies neither.
 *
 * The save is a VERSION save, which is what a user makes when they edit an
 * agent's instructions: the stamp lives on the agent row, so a fix that only
 * stamped the agent's own rename would leave the ordinary case unmoved.
 */
test('updated_at reports a real date and moves when the agent is saved', async ({ request }) => {
  const name = autotestName('updatedat');
  const agent = await createAgentWithVersion(request, name, { instructions: 'The brief.' });
  try {
    const listedUpdatedAt = async (): Promise<string> => {
      const listed = await readAgentList(request, { query: name });
      const row = listed.rows.find((entry) => entry['name'] === name);
      expect(row, 'the agent is not in its own filtered list').toBeDefined();
      return String(row?.['updated_at'] ?? '');
    };

    const created = await listedUpdatedAt();
    expect(
      created,
      'updated_at is the zero sentinel — no write can ever have reached this field',
    ).not.toBe('0001-01-01T00:00:00Z');
    expect(Number.isNaN(Date.parse(created)), `updated_at is not a date: ${created}`).toBe(false);

    const saved = await request.put(versionURL(agent.id, agent.versionId), {
      data: agentVersionBody({ name: 'base', instructions: 'The revised brief.' }),
    });
    expect(saved.status(), `the save answered ${(await saved.text()).slice(0, 200)}`).toBe(201);

    // Polled, not slept: the read that matters is the server's, and the
    // assertion is about the value it serves rather than about elapsed time.
    //
    // It polls the STRING and not `Date.parse`. The column keeps microseconds
    // and `Date.parse` keeps milliseconds, so a save that lands inside the
    // same millisecond as the create — the two are one HTTP round trip apart —
    // would compare equal and fail a numeric assertion for a value that did
    // move. The direction is asserted separately, on the parsed value.
    await expect
      .poll(listedUpdatedAt, {
        message: 'a version save did not move the agent’s updated_at',
        timeout: 15_000,
      })
      .not.toBe(created);
    const afterSave = await listedUpdatedAt();
    expect(Date.parse(afterSave)).toBeGreaterThanOrEqual(Date.parse(created));
  } finally {
    await deleteAgent(request, agent.id);
  }
});
