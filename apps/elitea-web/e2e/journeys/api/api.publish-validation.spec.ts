/**
 * The pre-publish quality gate: what it answers, what it refuses, and how it
 * attributes a problem it found inside somebody else's agent.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's pre-publish check cases (PUB-44…PUB-50, PUB-52…PUB-57,
 * PUB-59, PUB-60, PUB-62, PUB-63 and PUB-65). Publishing an agent to the
 * catalogue runs a check first — over the agent, and over every sub-agent it
 * calls — and the result decides whether the publish may proceed:
 *
 *   PASS  no findings that matter; an approval token is issued.
 *   WARN  findings a reader should see; the publish is still allowed and a
 *         token is still issued.
 *   FAIL  blocking findings; HTTP 422, and NO token is issued.
 *
 * The token is a GRANT, not a shape. The publish route once accepted any string
 * of sixteen lowercase hexadecimal characters in place of the check, so the
 * gate was optional for anyone willing to type one; the token is now signed and
 * bound to the version it was issued for, and the two cases under "the approval
 * token" below are what that means to a client — a well-formed token nobody
 * issued and a real token issued for another version are both refused.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ITEMS ARE ASSERTED AND NOT ONLY THE STATUS
 * ─────────────────────────────────────────────────────────────────────────────
 * The publish dialog renders this result: it lists each finding beside the
 * field it names, and it says which agent the finding is about. A journey that
 * checked only `status` would pass against a check that answered FAIL for
 * everything, which is the failure mode a quality gate actually has — it makes
 * publishing impossible and reports nothing an author can act on. So every case
 * below names the FIELD, and the sub-agent cases name the CONTEXT string, both
 * read from `runPublishValidation` in
 * `internal/api/v2/eliteacore/handler.go`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RIG CANNOT SHOW
 * ─────────────────────────────────────────────────────────────────────────────
 * The check has an AI half. This deployment reports `ai_validation_available:
 * false` and every finding it raises is `source: "deterministic"`, so the cases
 * that assert on an AI-sourced finding are out of scope here rather than
 * ported as a skip — a mock model cannot produce a grounded review of an
 * agent's instructions, and a journey that pretended otherwise would assert
 * the mock.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent is deleted in a `finally`; the two cases that publish
 * withdraw first, because a live version refuses the delete of the agent that
 * owns it.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext, APIResponse } from '@playwright/test';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** A version name in the alphabet the route accepts (`^[a-zA-Z0-9._-]+$`). */
function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

/** The refusal body, as text, for a message that says what actually happened. */
async function refusal(response: APIResponse): Promise<string> {
  return `${response.status()}: ${(await response.text()).slice(0, 500)}`;
}

/** Instructions long enough to satisfy the check's own floor. */
const QUALITY_INSTRUCTIONS =
  'You are a release notes assistant. Turn the commits, tickets and review notes the user ' +
  'gives you into a short summary that names what changed, who it affects and what is still open.';

/** A description long enough that the check raises no warning about it. */
const QUALITY_DESCRIPTION =
  'Drafts release notes from commits, tickets and review notes.';

function createQualityAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    {
      instructions: QUALITY_INSTRUCTIONS,
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.', 'What is still open?'],
    },
    projectId,
    QUALITY_DESCRIPTION,
  );
}

/**
 * An agent the check must refuse: instructions under the floor, no starters,
 * and a description too short to describe anything.
 */
function createSparseAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    { instructions: 'Do the thing.', conversationStarters: [] },
    projectId,
    'Does things.',
  );
}

function validate(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  body: Record<string, unknown>,
): Promise<APIResponse> {
  return request.post(
    `${API_BASE}/elitea_core/publish_validate/prompt_lib/${projectId}/${versionId}`,
    { data: body },
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

/** One finding, in the shape both the dialog and these assertions read. */
interface Finding extends Record<string, unknown> {
  readonly field?: string;
  readonly issue?: string;
  readonly source?: string;
  readonly context?: string | null;
}

/** The whole result, with the four lists the dialog renders. */
interface ValidationResult extends Record<string, unknown> {
  readonly status?: string;
  readonly critical_issues?: readonly Finding[];
  readonly warnings?: readonly Finding[];
  readonly recommendations?: readonly Finding[];
  readonly counts?: Record<string, number>;
  readonly validation_token?: string | null;
  readonly ai_validation_available?: boolean;
}

async function resultOf(response: APIResponse): Promise<ValidationResult> {
  return (await response.json()) as ValidationResult;
}

/** Every `msg` a 400's validation-error array carries, flattened. */
function messagesOf(body: unknown): readonly string[] {
  const error = (body as { error?: unknown })?.error;
  if (!Array.isArray(error)) return [];
  return error.map((entry) => String((entry as { msg?: unknown })?.msg ?? ''));
}

/** Every `loc` path a 400's validation-error array names, flattened. */
function locationsOf(body: unknown): readonly string[] {
  const error = (body as { error?: unknown })?.error;
  if (!Array.isArray(error)) return [];
  return error.flatMap((entry) => ((entry as { loc?: unknown })?.loc as string[] | undefined) ?? []);
}

/** Attach the child agent as a tool of the parent VERSION. */
async function attachSubAgent(
  request: APIRequestContext,
  projectId: string,
  child: { readonly id: string; readonly versionId: string },
  parent: { readonly id: string; readonly versionId: string },
): Promise<void> {
  // The URL names the CHILD and the body names the PARENT, which is the
  // opposite of how the path reads.
  const attached = await request.patch(
    `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}` +
      `/${child.id}/${child.versionId}`,
    {
      data: {
        application_id: Number(parent.id),
        version_id: Number(parent.versionId),
        has_relation: true,
      },
    },
  );
  expect(attached.status(), await refusal(attached)).toBe(201);
}

/** Withdraw whatever is live on this agent, then delete it. */
async function withdrawAndDelete(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<void> {
  const versions = await readApplicationVersions(request, applicationId, projectId);
  for (const version of versions) {
    if (version.status === 'published') {
      await request.post(
        `${API_BASE}/elitea_core/unpublish/prompt_lib/${projectId}/${version.id}`,
        { data: {} },
      );
    }
  }
  await deleteAgent(request, applicationId, projectId);
}

/* ── the result shape ─────────────────────────────────────────────────────── */

test('a well-formed agent passes, and the result carries every list the dialog renders', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_pass'), projectId);

  try {
    const response = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), await refusal(response)).toBe(200);
    const result = await resultOf(response);

    expect(result.status).toBe('PASS');
    // Every list is PRESENT, including the empty ones. The dialog iterates all
    // four, and an absent key renders as a missing section rather than as an
    // empty one — which reads to an author as "this was not checked".
    expect(Array.isArray(result.critical_issues)).toBe(true);
    expect(Array.isArray(result.warnings)).toBe(true);
    expect(Array.isArray(result.recommendations)).toBe(true);
    expect(typeof result['summary']).toBe('string');
    expect(result.counts).toMatchObject({
      critical: 0,
      warnings: 0,
      suggestions: expect.any(Number),
    });
    expect(result.ai_validation_available).toBe(false);
    // A PASS issues the approval token the publish route accepts in place of
    // running the check again.
    expect(typeof result.validation_token, JSON.stringify(result)).toBe('string');

    // The tags suggestion is what a PASS still tells the author: an agent with
    // fewer than three tags is publishable and hard to find, and the check
    // says so as a RECOMMENDATION rather than as a warning.
    const tags = (result.recommendations ?? []).find((item) => item.field === 'tags');
    expect(tags, `no tags suggestion in ${JSON.stringify(result.recommendations)}`).toBeDefined();
    expect(tags?.source).toBe('deterministic');
    expect(typeof tags?.['suggestion']).toBe('string');
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

test('a generic version name is a warning, and a warning still issues a token', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_warn'), projectId);

  try {
    // `v1` is one of the names the check calls generic. It is deliberately not
    // a refusal: the author may publish under it, and the dialog shows them
    // why they might not want to.
    const response = await validate(request, projectId, agent.versionId, { version_name: 'v1' });
    expect(response.status(), await refusal(response)).toBe(200);
    const result = await resultOf(response);

    expect(result.status).toBe('WARN');
    expect((result.warnings ?? []).map((item) => item.field)).toContain('version_name');
    expect(result.counts?.['warnings']).toBeGreaterThan(0);
    expect(typeof result.validation_token, 'a WARN withheld the token').toBe('string');
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

test('an under-specified agent fails, and a failure issues no token', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createSparseAgent(request, autotestName('val_fail'), projectId);

  try {
    const response = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    // 422, not 200 with a FAIL body: the status code is what stops a client
    // that renders the result without reading it.
    expect(response.status(), await refusal(response)).toBe(422);
    const result = await resultOf(response);

    expect(result.status).toBe('FAIL');
    expect(result.counts?.['critical']).toBeGreaterThan(0);
    expect((result.critical_issues ?? []).map((item) => item.field)).toContain('instructions');
    // The token is the whole point of the refusal. A FAIL that still handed
    // one out would let the caller publish by sending it straight back.
    expect(result.validation_token, JSON.stringify(result)).toBeNull();
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

test('every finding says which rule raised it', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createSparseAgent(request, autotestName('val_source'), projectId);

  try {
    const response = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    const result = await resultOf(response);
    const everything = [
      ...(result.critical_issues ?? []),
      ...(result.warnings ?? []),
      ...(result.recommendations ?? []),
    ];
    expect(everything.length, 'a sparse agent produced no findings at all').toBeGreaterThan(0);
    // `source` is what lets the dialog separate a rule from a reviewer, and it
    // is why an author can tell "the platform requires this" from "a reviewer
    // suggests it". On this deployment every finding is deterministic.
    for (const item of everything) {
      expect(['deterministic', 'ai'], JSON.stringify(item)).toContain(item.source);
    }
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

/* ── the request refusals ─────────────────────────────────────────────────── */

test('the check requires a version name', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_noname'), projectId);

  try {
    const refused = await validate(request, projectId, agent.versionId, { version_name: '' });
    expect(refused.status(), await refusal(refused)).toBe(400);
    const body = await refused.json();
    // The message AND the field it belongs beside: this refusal is rendered
    // under the name input, and the other 400 this route can answer belongs
    // there too but says something else.
    expect(messagesOf(body)).toContain('field required');
    expect(locationsOf(body)).toContain('version_name');
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

test('a version name outside the alphabet is refused before any checking runs', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_badname'), projectId);

  try {
    const refused = await validate(request, projectId, agent.versionId, {
      version_name: 'release one!',
    });
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect(messagesOf(await refused.json()).join(' ')).toContain('does not match regex');
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

test('checking a version id that names nothing answers 404', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  // A well-formed id no row can hold, and a well-formed name — so the missing
  // version is the only finding left. A route that checked the NAME first
  // would answer 400 and hide it.
  const refused = await validate(request, projectId, '999999999', {
    version_name: versionName('rel'),
  });
  expect(refused.status(), await refusal(refused)).toBe(404);
  expect((await refused.json()) as Record<string, unknown>).toMatchObject({
    error: 'version not found',
  });
});

test('the base version can be checked', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_base'), projectId);

  try {
    // The version an agent is created with IS `base`, and the guard that used
    // to refuse it by name is gone. Asserted through the NAME, because the
    // case is about the name: a read that confirmed the id alone would pass on
    // a handler that had renamed it.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.find((version) => version.id === agent.versionId)?.name).toBe('base');

    const response = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), await refusal(response)).toBe(200);
    expect((await resultOf(response)).status).toBe('PASS');
  } finally {
    await deleteAgent(request, agent.id, projectId);
  }
});

/* ── the approval token ───────────────────────────────────────────────────── */

test('a check that passed hands out a token the publish accepts', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const name = autotestName('val_token');
  const agent = await createQualityAgent(request, name, projectId);

  try {
    const checked = await validate(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
    });
    expect(checked.status(), await refusal(checked)).toBe(200);
    const token = (await resultOf(checked)).validation_token;
    expect(typeof token).toBe('string');

    // This is the wizard's own two-step: check, show the result, then publish
    // with the token so the second request does not re-run the check.
    const published = await publish(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
      validation_token: token,
    });
    expect(published.status(), await refusal(published)).toBe(200);
    expect(String(((await published.json()) as Record<string, unknown>)['public_agent_id'])).toBe(
      agent.id,
    );
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('a token the platform never issued is refused', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const agent = await createQualityAgent(request, autotestName('val_forged'), projectId);

  try {
    // WELL FORMED and unsigned. The publish route used to check the token's
    // SHAPE alone — sixteen or more lowercase hexadecimal characters — so any
    // string of the right alphabet skipped the quality gate on any version.
    // This value is exactly the length and alphabet the platform mints, which
    // is the point: only a token the platform SIGNED may stand in for a check.
    const forged = 'ab'.repeat(72);
    const refused = await publish(request, projectId, agent.versionId, {
      version_name: versionName('rel'),
      validation_token: forged,
    });
    expect(refused.status(), await refusal(refused)).toBe(400);
    const body = (await refused.json()) as Record<string, unknown>;
    expect(body).toMatchObject({ error: 'validation_token_invalid' });
    // The message is what the dialog shows the author, and it must tell them
    // what to do rather than only that something was wrong.
    expect(String(body['msg'])).toContain('not issued for this version');

    // Nothing was published. A refusal that had already cloned the row answers
    // the same 400.
    const versions = await readApplicationVersions(request, agent.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, agent.id);
  }
});

test('a token issued for another version does not publish this one', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const checked = await createQualityAgent(request, autotestName('val_tok_own'), projectId);
  const other = await createQualityAgent(request, autotestName('val_tok_other'), projectId);

  try {
    // A token the platform really ISSUED, for a different agent's version. It
    // is signed, so only the binding to the version can refuse it — which is
    // the half a shape check cannot do at all.
    const issued = await validate(request, projectId, checked.versionId, {
      version_name: versionName('rel'),
    });
    expect(issued.status(), await refusal(issued)).toBe(200);
    const token = (await resultOf(issued)).validation_token;
    expect(typeof token).toBe('string');

    const borrowed = await publish(request, projectId, other.versionId, {
      version_name: versionName('rel'),
      validation_token: token,
    });
    expect(borrowed.status(), await refusal(borrowed)).toBe(400);
    const body = (await borrowed.json()) as Record<string, unknown>;
    expect(body).toMatchObject({ error: 'validation_token_invalid' });
    expect(String(body['msg'])).toContain('not issued for this version');
    const otherVersions = await readApplicationVersions(request, other.id, projectId);
    expect(otherVersions.every((version) => version.status !== 'published')).toBe(true);

    // …and the same token publishes the version it WAS issued for, which is
    // what tells this apart from a route that refuses every token.
    const own = await publish(request, projectId, checked.versionId, {
      version_name: versionName('rel'),
      validation_token: token,
    });
    expect(own.status(), await refusal(own)).toBe(200);
  } finally {
    await withdrawAndDelete(request, projectId, other.id);
    await withdrawAndDelete(request, projectId, checked.id);
  }
});

/* ── the inline fallback ──────────────────────────────────────────────────── */

test('publishing without a token runs the check inline: the good agent goes through and the sparse one does not', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const good = await createQualityAgent(request, autotestName('val_inline_ok'), projectId);
  const sparse = await createSparseAgent(request, autotestName('val_inline_bad'), projectId);

  try {
    // No token at all. The gate is not a client-side courtesy: a caller that
    // skips the check entirely still meets it, which is what stops an agent
    // reaching the catalogue through a hand-written request.
    const published = await publish(request, projectId, good.versionId, {
      version_name: versionName('rel'),
    });
    expect(published.status(), await refusal(published)).toBe(200);

    const refused = await publish(request, projectId, sparse.versionId, {
      version_name: versionName('rel'),
    });
    expect(refused.status(), await refusal(refused)).toBe(422);
    const body = (await refused.json()) as {
      error?: unknown;
      validation_result?: ValidationResult;
    };
    expect(body.error).toBe('validation_failed');
    // The RESULT rides along, because the dialog shows the author what to fix
    // rather than only that they may not publish.
    expect(body.validation_result?.status).toBe('FAIL');
    expect((body.validation_result?.critical_issues ?? []).map((item) => item.field)).toContain(
      'instructions',
    );

    const versions = await readApplicationVersions(request, sparse.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, good.id);
    await withdrawAndDelete(request, projectId, sparse.id);
  }
});

/* ── the sub-agents the check reaches into ────────────────────────────────── */

test("a problem inside a sub-agent is attributed to that sub-agent, not to its parent", async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const childName = autotestName('val_subchild');
  const child = await createSparseAgent(request, childName, projectId);
  const parent = await createQualityAgent(request, autotestName('val_subparent'), projectId);

  try {
    await attachSubAgent(request, projectId, child, parent);

    const response = await validate(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    // The PARENT is well formed and still fails, because publishing it
    // publishes a private copy of the child — the reader gets both.
    expect(response.status(), await refusal(response)).toBe(422);
    const result = await resultOf(response);
    expect(result.status).toBe('FAIL');

    const attributed = (result.critical_issues ?? []).filter(
      (item) => typeof item.context === 'string' && item.context.startsWith('sub-agent:'),
    );
    expect(attributed.length, JSON.stringify(result.critical_issues)).toBeGreaterThan(0);
    // The context NAMES the agent and the version, which is what lets an
    // author open the right editor. A finding attributed to the parent would
    // send them to an agent whose instructions are fine.
    expect(attributed[0]?.context).toBe(`sub-agent: ${childName} (base)`);
    expect(attributed.map((item) => item.field)).toContain('instructions');
    expect(attributed[0]?.source).toBe('deterministic');

    // …and NOTHING is attributed to the parent, whose own instructions are
    // fine. This is the half that says the attribution is real: a check that
    // reported the sub-agent's problem against the parent would answer the
    // same FAIL and send the author to the wrong editor.
    expect(attributed).toHaveLength((result.critical_issues ?? []).length);
  } finally {
    await deleteAgent(request, parent.id, projectId);
    await deleteAgent(request, child.id, projectId);
  }
});

test('two sub-agents are attributed separately', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const firstName = autotestName('val_twosub_a');
  const secondName = autotestName('val_twosub_b');
  const first = await createSparseAgent(request, firstName, projectId);
  const second = await createSparseAgent(request, secondName, projectId);
  const parent = await createQualityAgent(request, autotestName('val_twosub_parent'), projectId);

  try {
    await attachSubAgent(request, projectId, first, parent);
    await attachSubAgent(request, projectId, second, parent);

    const response = await validate(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), await refusal(response)).toBe(422);
    const result = await resultOf(response);

    // TWO contexts, one per agent. A check that collapsed them into one would
    // let an author fix the agent it named and publish again into the same
    // refusal, with nothing saying which of the two was still wrong.
    const contexts = new Set(
      [...(result.critical_issues ?? []), ...(result.warnings ?? [])]
        .map((item) => item.context)
        .filter((context): context is string => typeof context === 'string'),
    );
    expect(contexts).toContain(`sub-agent: ${firstName} (base)`);
    expect(contexts).toContain(`sub-agent: ${secondName} (base)`);

    // The short DESCRIPTION is a warning rather than a blocker, and it is
    // attributed the same way — the two lists share the attribution rule.
    const described = (result.warnings ?? []).filter((item) => item.field === 'description');
    expect(described.length, JSON.stringify(result.warnings)).toBeGreaterThan(0);
    expect(described[0]?.source).toBe('deterministic');
    expect(String(described[0]?.context)).toContain('sub-agent:');
  } finally {
    await deleteAgent(request, parent.id, projectId);
    await deleteAgent(request, first.id, projectId);
    await deleteAgent(request, second.id, projectId);
  }
});

test('the check reaches a sub-agent two levels down', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const middleName = autotestName('val_nested_mid');
  const leafName = autotestName('val_nested_leaf');
  const leaf = await createSparseAgent(request, leafName, projectId);
  const middle = await createSparseAgent(request, middleName, projectId);
  const parent = await createQualityAgent(request, autotestName('val_nested_parent'), projectId);

  try {
    await attachSubAgent(request, projectId, leaf, middle);
    await attachSubAgent(request, projectId, middle, parent);

    const response = await validate(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), await refusal(response)).toBe(422);
    const result = await resultOf(response);
    expect(result.status).toBe('FAIL');

    // BOTH levels. Publishing the parent embeds the whole tree, so a check
    // that stopped at the first level would let an agent reach the catalogue
    // carrying a sub-agent nobody had looked at.
    const contexts = new Set(
      (result.critical_issues ?? [])
        .map((item) => item.context)
        .filter((context): context is string => typeof context === 'string'),
    );
    expect(contexts).toContain(`sub-agent: ${middleName} (base)`);
    expect(contexts).toContain(`sub-agent: ${leafName} (base)`);
    expect((result.critical_issues ?? []).map((item) => item.field)).toContain('instructions');
  } finally {
    await deleteAgent(request, parent.id, projectId);
    await deleteAgent(request, middle.id, projectId);
    await deleteAgent(request, leaf.id, projectId);
  }
});

test('well-formed sub-agents raise nothing that blocks the parent', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const first = await createQualityAgent(request, autotestName('val_goodsub_a'), projectId);
  const second = await createQualityAgent(request, autotestName('val_goodsub_b'), projectId);
  const parent = await createQualityAgent(request, autotestName('val_goodsub_parent'), projectId);

  try {
    await attachSubAgent(request, projectId, first, parent);
    await attachSubAgent(request, projectId, second, parent);

    const response = await validate(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    // The negative case, and it is the one that keeps the four above honest: a
    // check that attributed a finding to every sub-agent it walked would pass
    // all of them and make the feature unusable.
    expect(response.status(), await refusal(response)).toBe(200);
    const result = await resultOf(response);
    expect(result.status).toBe('PASS');
    expect(result.critical_issues).toEqual([]);
  } finally {
    await deleteAgent(request, parent.id, projectId);
    await deleteAgent(request, first.id, projectId);
    await deleteAgent(request, second.id, projectId);
  }
});

test('two sub-agents sharing a name are reported once, with the count', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const sharedName = autotestName('val_dupname');
  // Two DIFFERENT agents under one name. The platform allows it, and the
  // runtime addresses a sub-agent tool by name — so the parent would have two
  // tools it cannot tell apart, which is why this blocks rather than warns.
  const first = await createQualityAgent(request, sharedName, projectId);
  const second = await createQualityAgent(request, sharedName, projectId);
  const parent = await createQualityAgent(request, autotestName('val_dupname_parent'), projectId);

  try {
    await attachSubAgent(request, projectId, first, parent);
    await attachSubAgent(request, projectId, second, parent);

    const response = await validate(request, projectId, parent.versionId, {
      version_name: versionName('rel'),
    });
    expect(response.status(), await refusal(response)).toBe(422);
    const result = await resultOf(response);
    expect(result.status).toBe('FAIL');

    const clashes = (result.critical_issues ?? []).filter((item) => item.field === 'name');
    // ONE finding, not one per occurrence: the author has a single thing to
    // fix, and a list that repeated it would read as two separate problems.
    expect(clashes, JSON.stringify(result.critical_issues)).toHaveLength(1);
    expect(clashes[0]?.issue).toContain('not unique');
    expect(clashes[0]?.issue).toContain('2 occurrences');
    expect(clashes[0]?.context).toBe(`sub-agent: ${sharedName}`);
    expect(clashes[0]?.source).toBe('deterministic');
  } finally {
    await deleteAgent(request, parent.id, projectId);
    await deleteAgent(request, first.id, projectId);
    await deleteAgent(request, second.id, projectId);
  }
});
