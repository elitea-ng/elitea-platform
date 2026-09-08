/**
 * Publishing an agent to the catalogue: the round trip, and the eight ways it
 * is refused.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's publish lifecycle cases (PUB-01, PUB-02, PUB-04…PUB-08
 * and PUB-10): a draft is published and the draft survives, a withdrawal turns
 * the clone back into a draft instead of deleting it, the `base` version is
 * publishable, a name that is already live is refused, withdrawing something
 * that was never published is refused, a name with spaces or punctuation and an
 * empty name are both refused before anything is created, and a version id that
 * names nothing answers 404.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THESE RUN IN A SECOND PROJECT
 * ─────────────────────────────────────────────────────────────────────────────
 * Publishing is a CROSS-PROJECT operation. The clone is written in the author's
 * own schema and a TWIN is written into the public project's, which is the only
 * schema ELITEA Catalog reads (internal/api/v2/eliteacore/catalog_mirror.go).
 * Project 1 IS the public project on this rig, so a publish issued from there
 * takes the `isPublicProject` short circuit, writes no twin, and the author-side
 * clone and the catalogue row become the same row — which makes half of the
 * contract unobservable, including "the catalogue kept it after the author's own
 * listing changed" and "the withdrawal reached the catalogue too".
 *
 * `scripts/e2e-stack.sh` seeds `e2e-publish-author` for this, and the ids are
 * resolved BY NAME through the product's own project listing: the seed picks
 * the number out of a reserved range, and a journey repeating that number would
 * keep passing against whatever row later held it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE REFUSALS ARE ASSERTED ON THEIR BODIES
 * ─────────────────────────────────────────────────────────────────────────────
 * The publish route answers 400 for four different findings and 422 for three
 * more, and the publish dialog renders each one differently. A journey that
 * asserted only the status code would pass on a handler that refused every
 * request for the first reason it thought of. Each case below therefore names
 * the finding the handler writes (`internal/api/v2/eliteacore/handler.go`), and
 * the "nothing was created" half is read back from the agent's own version list
 * — a refusal that had already cloned the row answers the same status.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent is withdrawn and deleted in a `finally`, in that order:
 * a live published version refuses the delete of the agent that owns it, so a
 * cleanup that only deleted would leave both the agent AND its catalogue row.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  readCatalogue,
  readVersion,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext, APIResponse } from '@playwright/test';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * A version name, in the alphabet the route accepts.
 *
 * `^[a-zA-Z0-9._-]+$` — no `autotest_` prefix is possible in the cases that
 * assert the alphabet, so the uniqueness comes from the random tail alone.
 */
function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * An agent that CAN pass the quality gate.
 *
 * Publishing runs the pre-publish check inline when no approval token is sent,
 * and the check raises a CRITICAL issue for instructions under 50 characters
 * (`runPublishValidation`). An agent built from the plain fixture cannot be
 * published at all, so every case here — including the refusal cases, which
 * must fail for their OWN reason and not for a sparse agent — starts from this.
 */
async function createPublishableAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    {
      instructions:
        'You are a release notes assistant. Turn the commits, tickets and review notes the ' +
        'user gives you into a short summary that names what changed, who it affects and what ' +
        'is still open.',
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.', 'What is still open?'],
    },
    projectId,
  );
}

function publish(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  body: Record<string, unknown>,
): Promise<APIResponse> {
  return request.post(`${API_BASE}/elitea_core/publish/prompt_lib/${projectId}/${versionId}`, {
    data: body,
  });
}

function unpublish(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
): Promise<APIResponse> {
  return request.post(`${API_BASE}/elitea_core/unpublish/prompt_lib/${projectId}/${versionId}`, {
    data: {},
  });
}

/**
 * Withdraw everything this agent has live, then delete it.
 *
 * The order is the product's own rule: a live published version refuses the
 * delete of the agent that owns it (`Unpublish first.`), so a cleanup that only
 * deleted would leave the agent, its clone AND its catalogue row behind — and
 * the next run's catalogue assertions would then be reading this run's rubbish.
 */
async function withdrawAndDelete(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<void> {
  const versions = await readApplicationVersions(request, applicationId, projectId);
  for (const version of versions) {
    if (version.status === 'published') {
      await unpublish(request, projectId, version.id);
    }
  }
  await deleteAgent(request, applicationId, projectId);
}

/** The refusal body, as text, for a message that says what actually happened. */
async function refusal(response: APIResponse): Promise<string> {
  return `${response.status()}: ${(await response.text()).slice(0, 400)}`;
}

/** Every `msg` a validation-error array carries, flattened for one assertion. */
function messagesOf(body: unknown): readonly string[] {
  const error = (body as { error?: unknown })?.error;
  if (!Array.isArray(error)) return [];
  return error.map((entry) => String((entry as { msg?: unknown })?.msg ?? ''));
}

/** Every `rule` the 422 validation result carries. */
function rulesOf(body: unknown): readonly string[] {
  const result = (body as { validation_result?: { issues?: unknown } })?.validation_result;
  const issues = result?.issues;
  if (!Array.isArray(issues)) return [];
  return issues.map((issue) => String((issue as { rule?: unknown })?.rule ?? ''));
}

/* ── the round trip ───────────────────────────────────────────────────────── */

test('publishing a draft adds a published clone and leaves the draft alone', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_roundtrip');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const published = await publish(request, projectId, agent.versionId, { version_name: release });
    expect(published.status(), await refusal(published)).toBe(200);
    const body = (await published.json()) as Record<string, unknown>;

    // The response names the AUTHOR-side rows and, because this project is not
    // the catalogue, the catalogue's own pair as well. The second pair is the
    // twin: it is omitted when the author already stands in the public project,
    // so its presence here is what says the mirror ran.
    expect(String(body['public_agent_id'])).toBe(agent.id);
    expect(body['version_name']).toBe(release);
    expect(body['catalog_agent_id'], `no catalogue twin in ${JSON.stringify(body)}`).toBeTruthy();
    expect(body['catalog_version_id'], `no catalogue twin in ${JSON.stringify(body)}`).toBeTruthy();

    const cloneId = String(body['public_version_id']);
    expect(cloneId, 'the publish named no cloned version').not.toBe(agent.versionId);

    // TWO versions, not one: the draft the author keeps editing and the clone
    // the catalogue serves. A handler that flipped the source row's status
    // instead would answer the same 200 and leave the author with no draft.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    const draft = versions.find((version) => version.id === agent.versionId);
    const clone = versions.find((version) => version.id === cloneId);
    expect(draft?.status, `the source version is now ${JSON.stringify(versions)}`).toBe('draft');
    expect(draft?.name, 'the source version was renamed by the publish').toBe('base');
    expect(clone?.status).toBe('published');
    expect(clone?.name).toBe(release);

    // The clone records which draft it came from, in its own stored meta. That
    // is the link a reader follows from a catalogue entry back to its source.
    //
    // The RESPONSE says the same thing, and the two are asserted together: the
    // response key used to repeat the clone's id, so one publish answered
    // "where did this come from?" two ways and a client that followed the
    // response arrived at the published copy instead of the draft.
    const cloneDetails = await readVersion(request, agent.id, cloneId, projectId);
    expect(cloneDetails.meta['source_version_id']).toBe(agent.versionId);
    expect(body['source_version_id']).toBe(agent.versionId);
    expect(body['source_version_id'], 'the response names the clone as its own source').not.toBe(
      cloneId,
    );

    // ELITEA Catalog serves it. Polled, because the twin is written in the
    // publish transaction but the catalogue read is a separate connection.
    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('withdrawing turns the published clone back into a draft and empties the catalogue row', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_withdraw');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const published = await publish(request, projectId, agent.versionId, { version_name: release });
    expect(published.status(), await refusal(published)).toBe(200);
    const cloneId = String(((await published.json()) as Record<string, unknown>)['public_version_id']);

    const withdrawn = await unpublish(request, projectId, cloneId);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);

    // The clone SURVIVES as a draft. The legacy platform reverted rather than
    // deleted, and an author who withdraws a release expects to find the
    // version they published still there — a delete would take the version
    // name with it, and with it the ability to publish that name again.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    const clone = versions.find((version) => version.id === cloneId);
    expect(clone, `the withdrawal deleted the clone: ${JSON.stringify(versions)}`).toBeDefined();
    expect(clone?.status).toBe('draft');
    expect(clone?.name).toBe(release);

    // …and the catalogue no longer carries it. An agent that stays in the
    // catalogue after its author took it down is the one outcome the publish
    // terms promise cannot happen.
    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(false);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('the base version is publishable', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_base');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    // The version created with the agent IS `base`, and the guard that used to
    // refuse it by name is gone. Asserted through the name rather than the id:
    // the case is about the NAME being publishable, so a read that confirmed
    // the id alone would pass on a handler that had renamed it.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.find((version) => version.id === agent.versionId)?.name).toBe('base');

    const published = await publish(request, projectId, agent.versionId, { version_name: release });
    expect(published.status(), await refusal(published)).toBe(200);

    await expect
      .poll(async () => (await readCatalogue(request)).some((row) => row.name === name), {
        timeout: 20_000,
      })
      .toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

/* ── the refusals ─────────────────────────────────────────────────────────── */

test('a version name that is already live is refused', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_dupname');
  const release = versionName('rel');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const first = await publish(request, projectId, agent.versionId, { version_name: release });
    expect(first.status(), await refusal(first)).toBe(200);

    // The SAME draft, published again under the SAME name. The clone already
    // owns that name on this agent, and the version-name column is unique per
    // application — so a handler that did not pre-check would answer 500 on a
    // constraint violation instead of naming the collision.
    const second = await publish(request, projectId, agent.versionId, { version_name: release });
    expect(second.status(), await refusal(second)).toBe(422);
    expect(rulesOf(await second.json())).toContain('version_name_exists_in_source');

    // And exactly one clone exists. A refusal that had already written the row
    // answers the same 422.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.filter((version) => version.name === release)).toHaveLength(1);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('withdrawing a version that was never published is refused', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_neverpub');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const withdrawn = await unpublish(request, projectId, agent.versionId);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(409);
    expect((await withdrawn.json()) as Record<string, unknown>).toMatchObject({
      error: 'version is not published',
    });

    // The draft is untouched — a "withdrawal" that reverted a draft to a draft
    // would answer the same refusal while having rewritten the row.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.find((version) => version.id === agent.versionId)?.status).toBe('draft');
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('a version name with spaces or punctuation is refused before anything is created', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_badname');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const before = await readApplicationVersions(request, agent.id, projectId);

    const refused = await publish(request, projectId, agent.versionId, {
      version_name: 'release one!',
    });
    expect(refused.status(), await refusal(refused)).toBe(400);
    // The MESSAGE, not only the status: this refusal is rendered in the publish
    // dialog beside the name field, and the four other 400s this route can
    // answer belong beside something else.
    expect(messagesOf(await refused.json()).join(' ')).toContain('does not match regex');

    // Nothing was created. The alphabet is checked before the version is even
    // looked up, so a clone here would mean the order had changed.
    const after = await readApplicationVersions(request, agent.id, projectId);
    expect(after).toHaveLength(before.length);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('an empty version name is refused as a missing field', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('pub_emptyname');
  const agent = await createPublishableAgent(request, name, projectId);

  try {
    const refused = await publish(request, projectId, agent.versionId, { version_name: '' });
    expect(refused.status(), await refusal(refused)).toBe(400);
    // `field required`, not the alphabet message: an empty string is a MISSING
    // name, and the dialog says so differently from a rejected one.
    expect(messagesOf(await refused.json())).toContain('field required');

    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('publishing a version id that names nothing answers 404', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);

  // A well-formed id no row can hold. The name is well formed too, so the only
  // finding left is the missing version — a route that refused the NAME first
  // would answer 400 and hide it.
  const refused = await publish(request, projectId, '999999999', {
    version_name: versionName('rel'),
  });
  expect(refused.status(), await refusal(refused)).toBe(404);
  expect((await refused.json()) as Record<string, unknown>).toMatchObject({
    error: 'version not found',
  });
});
