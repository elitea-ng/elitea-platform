/**
 * Publishing an agent that DELEGATES: what happens to its sub-agent tree.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's sub-agent publishing cases — PUB-16…20, PUB-23,
 * PUB-24, PUB-28, PUB-29, PUB-32, PUB-33, PUB-37, PUB-43, PUB-64, PUB-66 and
 * PUB-69…73. In one sentence each: publishing SNAPSHOTS the whole sub-agent
 * tree into private copies, the copies record where they came from and which
 * published parent owns them, the author's own agents are left as drafts,
 * withdrawal removes the copies and nothing else, deleting a sub-agent detaches
 * it from every parent, a cycle and an over-deep chain are both refused with
 * one structural finding, and a pipeline is neither a sub-agent nor publishable.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE COPY EXISTS AT ALL
 * ─────────────────────────────────────────────────────────────────────────────
 * A published agent must keep working when its author edits or deletes the
 * agents it delegates to. So the publish clones each referenced agent into an
 * `embedded` shell of its own and points the published version at THAT — the
 * snapshot — while the author's original stays a draft they may keep editing.
 * Every case below therefore reads the tree back out of the product rather than
 * trusting the publish's 200: `embedSubAgents` runs AFTER the publish
 * transaction commits (`internal/api/v2/eliteacore/handler.go`), so a copy that
 * never happened is invisible from the response.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * A PUBLISHED VERSION CARRIES TWO REFERENCES PER SUB-AGENT
 * ─────────────────────────────────────────────────────────────────────────────
 * The publish copies the source version's tool attachments verbatim, which
 * brings the author's OWN reference across, and then links the embedded copy
 * beside it. So the published version lists the child twice, once pointing at
 * the author's agent and once at the snapshot. Every assertion here therefore
 * names which of the two it means — `embeddedCopyOf` below — rather than
 * reading `tools[0]`, which is whichever the join happened to order first.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHERE THIS SERVICE REFUSES A CYCLE, AND WHERE THE BASELINE DID
 * ─────────────────────────────────────────────────────────────────────────────
 * DISCLOSED GAP. The baseline refused a circular link at LINK time: the request
 * that would close the loop was rejected. This service accepts it and refuses at
 * PUBLISH time instead — the pre-publish check walks the graph with a visited
 * set and answers a `circular dependency` finding, so nothing circular can be
 * published and the check cannot loop for ever. The cycle case below asserts
 * the refusal at the surface that makes it, and states the link's own status so
 * that a link-time guard added later fails here loudly instead of silently
 * making this fixture unbuildable.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHICH PROJECT EACH CASE PUBLISHES FROM
 * ─────────────────────────────────────────────────────────────────────────────
 * Two, and the difference is the subject of three of the cases. The AUTHOR
 * cases publish from `e2e-publish-author`, which is not the catalogue, so the
 * clone and the catalogue twin are different rows and a withdrawal has
 * something to remove. The MODERATOR cases publish from inside the catalogue
 * project itself, which is the in-place flow: no twin, the published version
 * IS the catalogue row, and the public agent detail is the read that serves it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent is `autotest_*`, and the teardown withdraws before it
 * deletes: a live published version refuses the delete of the agent that owns
 * it, so a cleanup that only deleted would leave the agent, its clone and its
 * catalogue row behind. The embedded shells go with the withdrawal, which is
 * one of the things the cases assert.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  agentVersionBody,
  attachSubAgent,
  createAgentWithVersion,
  deleteAgent,
  readApplicationVersions,
  readVersion,
  resolveCatalogueProjectId,
  resolvePublishAuthorProjectId,
  subAgentToolsOf,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext, APIResponse } from '@playwright/test';
import type { SubAgentToolRow } from '../../fixtures/api';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/** A version name in the alphabet the publish route accepts: `[a-zA-Z0-9._-]`. */
function versionName(tag: string): string {
  return `${tag}-${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * An agent that CAN pass the quality gate.
 *
 * Publishing runs the pre-publish check inline when no approval token is sent,
 * and the check raises a CRITICAL issue for instructions under 50 characters.
 * Every agent in this file — parents, children and grandchildren alike — is
 * built from this, so that a case which is about a sub-agent tree never fails
 * because one node was sparse.
 */
async function createPublishableAgent(
  request: APIRequestContext,
  name: string,
  projectId: string,
): Promise<Agent> {
  const created = await createAgentWithVersion(
    request,
    name,
    {
      instructions:
        'You are a release notes assistant. Turn the commits, tickets and review notes the ' +
        'user gives you into a short summary that names what changed and what is still open.',
      welcomeMessage: 'Send me the commits and I will draft the notes.',
      conversationStarters: ['Summarise this release.'],
    },
    projectId,
  );
  return { id: created.id, versionId: created.versionId, name };
}

interface Agent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
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

function publishValidate(
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

/** The refusal body, as text, for a message that says what actually happened. */
async function refusal(response: APIResponse): Promise<string> {
  return `${response.status()}: ${(await response.text()).slice(0, 500)}`;
}

/** Publish and return the id of the published clone. */
async function publishAndGetClone(
  request: APIRequestContext,
  projectId: string,
  versionId: string,
  release: string,
): Promise<string> {
  const response = await publish(request, projectId, versionId, { version_name: release });
  expect(response.status(), await refusal(response)).toBe(200);
  const body = (await response.json()) as Record<string, unknown>;
  const cloneId = String(body['public_version_id'] ?? '');
  expect(cloneId, `the publish named no cloned version: ${JSON.stringify(body)}`).not.toBe('');
  return cloneId;
}

/**
 * The sub-agent entries a version holds, as the version read serves them.
 */
async function subAgentsOf(
  request: APIRequestContext,
  applicationId: string,
  versionId: string,
  projectId: string,
): Promise<readonly SubAgentToolRow[]> {
  const stored = await readVersion(request, applicationId, versionId, projectId);
  return subAgentToolsOf(stored.tools);
}

/**
 * The EMBEDDED snapshot of `source` on a published version — the entry that
 * points somewhere other than the author's own agent.
 *
 * Named rather than indexed for the reason the header gives: a published
 * version lists the child twice, and the copy is the one the runtime is meant
 * to reach.
 */
function embeddedCopyOf(
  subAgents: readonly SubAgentToolRow[],
  source: Agent,
): SubAgentToolRow | undefined {
  return subAgents.find((entry) => entry.applicationId !== source.id);
}

/** Whether a version read answers "gone" — the withdrawal's own proof. */
async function versionIsGone(
  request: APIRequestContext,
  applicationId: string,
  versionId: string,
  projectId: string,
): Promise<boolean> {
  const response = await request.get(
    `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${applicationId}/${versionId}`,
  );
  return response.status() === 404 || response.status() === 400;
}

/**
 * Withdraw everything this agent has live, then delete it.
 *
 * The order is the product's own rule: a live published version refuses the
 * delete of the agent that owns it.
 */
async function withdrawAndDelete(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<void> {
  const versions = await readApplicationVersions(request, applicationId, projectId).catch(() => []);
  for (const version of versions) {
    if (version.status === 'published') {
      await unpublish(request, projectId, version.id);
    }
  }
  await deleteAgent(request, applicationId, projectId);
}

/** Clean up a whole fixture tree, deepest parent first. Never throws. */
async function cleanUp(
  request: APIRequestContext,
  projectId: string,
  agents: readonly Agent[],
): Promise<void> {
  for (const agent of agents) {
    await withdrawAndDelete(request, projectId, agent.id).catch(() => {});
  }
}

/* ── the snapshot ─────────────────────────────────────────────────────────── */

test('publishing snapshots each sub-agent into a private copy that records where it came from', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa_child'), projectId);

  try {
    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: child.versionId },
      projectId,
    );
    expect(linked.status(), await refusal(linked)).toBe(201);

    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );

    const published = await subAgentsOf(request, parent.id, cloneId, projectId);
    const copy = embeddedCopyOf(published, child);
    expect(
      copy,
      `the published version references only the author's own agent: ${JSON.stringify(published)}`,
    ).toBeDefined();

    // The copy is a real agent shell of its own, whose single version is
    // `embedded`. Read back through the product, because the embed runs after
    // the publish transaction and its failure is only in the log.
    const embedded = await readVersion(request, copy!.applicationId, copy!.versionId, projectId);
    expect(embedded.status).toBe('embedded');

    // PROVENANCE. Five keys, and each answers a different question a reader of
    // the catalogue has: which project, agent and version this snapshot was
    // taken from, and which published parent owns it. Without the last pair a
    // withdrawal cannot tell one parent's copies from another's.
    expect(String(embedded.meta['source_project_id'])).toBe(projectId);
    expect(String(embedded.meta['source_application_id'])).toBe(child.id);
    expect(String(embedded.meta['source_version_id'])).toBe(child.versionId);
    expect(String(embedded.meta['parent_published_app_id'])).toBe(parent.id);
    expect(String(embedded.meta['parent_published_version_id'])).toBe(cloneId);

    // …and the AUTHOR's own sub-agent is untouched: still a draft, not marked
    // published by its parent's release. An author who publishes a parent has
    // not published the agents behind it.
    const childVersions = await readApplicationVersions(request, child.id, projectId);
    expect(childVersions.every((version) => version.status === 'draft')).toBe(true);
  } finally {
    await cleanUp(request, projectId, [parent, child]);
  }
});

test('a two-level sub-agent chain is embedded at every level', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa2_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa2_child'), projectId);
  const grandchild = await createPublishableAgent(request, autotestName('sa2_grand'), projectId);

  try {
    for (const [parentVersionId, node] of [
      [parent.versionId, child],
      [child.versionId, grandchild],
    ] as const) {
      const linked = await attachSubAgent(
        request,
        parentVersionId,
        { applicationId: node.id, versionId: node.versionId },
        projectId,
      );
      expect(linked.status(), await refusal(linked)).toBe(201);
    }

    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );

    const firstLevel = embeddedCopyOf(
      await subAgentsOf(request, parent.id, cloneId, projectId),
      child,
    );
    expect(firstLevel, 'the first level was not embedded').toBeDefined();

    // THE POINT OF THE CASE. A snapshot that stopped at the first level would
    // publish a parent whose own sub-agent still reached into the author's
    // private project, so editing the grandchild would change the published
    // agent's behaviour. The recursion is what prevents that, and it is only
    // visible from the embedded copy's own tools.
    const secondLevel = embeddedCopyOf(
      await subAgentsOf(request, firstLevel!.applicationId, firstLevel!.versionId, projectId),
      grandchild,
    );
    expect(
      secondLevel,
      `the embedded first level references no snapshot of its own child`,
    ).toBeDefined();

    const embeddedGrandchild = await readVersion(
      request,
      secondLevel!.applicationId,
      secondLevel!.versionId,
      projectId,
    );
    expect(embeddedGrandchild.status).toBe('embedded');
    expect(String(embeddedGrandchild.meta['source_application_id'])).toBe(grandchild.id);
  } finally {
    await cleanUp(request, projectId, [parent, child, grandchild]);
  }
});

test('a parent that names a sub-agent version other than base embeds exactly that version', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa3_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa3_child'), projectId);

  try {
    // A SECOND version on the child, and the reference names it rather than
    // `base`. Publishing the newest version of a sub-agent instead of the one
    // the parent chose would change what the published parent does.
    const secondName = versionName('alt');
    const secondVersion = await request.post(
      `${API_BASE}/elitea_core/versions/prompt_lib/${projectId}/${child.id}`,
      {
        data: agentVersionBody({
          name: secondName,
          agentType: 'openai',
          instructions:
            'You are the alternative behaviour of this sub-agent and you answer differently.',
        }),
      },
    );
    expect(secondVersion.status(), await refusal(secondVersion)).toBe(201);
    const altVersionId = String(((await secondVersion.json()) as { id?: unknown }).id ?? '');
    expect(altVersionId, 'the version create named no id').not.toBe('');
    expect(altVersionId).not.toBe(child.versionId);

    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: altVersionId },
      projectId,
    );
    expect(linked.status(), await refusal(linked)).toBe(201);

    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );
    const copy = embeddedCopyOf(await subAgentsOf(request, parent.id, cloneId, projectId), child);
    expect(copy, 'nothing was embedded').toBeDefined();

    const embedded = await readVersion(request, copy!.applicationId, copy!.versionId, projectId);
    expect(
      String(embedded.meta['source_version_id']),
      'the embed snapshotted a version the parent never referenced',
    ).toBe(altVersionId);
    expect(embedded.name).toBe(secondName);
  } finally {
    await cleanUp(request, projectId, [parent, child]);
  }
});

/* ── withdrawal, and what it may take ─────────────────────────────────────── */

test('withdrawing a parent removes its embedded copies and leaves the author their sub-agent', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa4_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa4_child'), projectId);

  try {
    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: child.versionId },
      projectId,
    );
    expect(linked.status(), await refusal(linked)).toBe(201);

    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );
    const copy = embeddedCopyOf(await subAgentsOf(request, parent.id, cloneId, projectId), child);
    expect(copy, 'nothing was embedded, so the withdrawal has nothing to remove').toBeDefined();

    const withdrawn = await unpublish(request, projectId, cloneId);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);

    // The snapshot goes. Polled, because the cascade runs outside the request
    // that answered 200.
    await expect
      .poll(
        async () => versionIsGone(request, copy!.applicationId, copy!.versionId, projectId),
        {
          timeout: 20_000,
          message: 'the embedded sub-agent copy is still readable after the withdrawal',
        },
      )
      .toBe(true);

    // THE AUTHOR'S OWN AGENT DOES NOT. This is the assertion the case exists
    // for: the withdrawal reaches a list that holds the author's reference as
    // well as the snapshot, and a cascade that could not tell them apart
    // deleted the agent the author was still editing — every version of it,
    // with a 200 on the withdrawal and nothing said.
    const survivors = await readApplicationVersions(request, child.id, projectId);
    expect(
      survivors.some((version) => version.id === child.versionId && version.status === 'draft'),
      `the withdrawal took the author's own sub-agent: ${JSON.stringify(survivors)}`,
    ).toBe(true);

    // …and the author's draft parent still delegates to it.
    const draftSubAgents = await subAgentsOf(request, parent.id, parent.versionId, projectId);
    expect(draftSubAgents.map((entry) => entry.applicationId)).toContain(child.id);
  } finally {
    await cleanUp(request, projectId, [parent, child]);
  }
});

test('re-publishing after a withdrawal makes the embedded copies again', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa5_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa5_child'), projectId);

  try {
    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: child.versionId },
      projectId,
    );
    expect(linked.status(), await refusal(linked)).toBe(201);

    const firstClone = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );
    const firstCopy = embeddedCopyOf(
      await subAgentsOf(request, parent.id, firstClone, projectId),
      child,
    );
    expect(firstCopy).toBeDefined();

    const withdrawn = await unpublish(request, projectId, firstClone);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);

    // A NEW name: the withdrawn clone survives as a draft and keeps the name
    // it was published under, so re-using it is refused for a reason that has
    // nothing to do with sub-agents.
    const secondClone = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel2'),
    );
    const secondCopy = embeddedCopyOf(
      await subAgentsOf(request, parent.id, secondClone, projectId),
      child,
    );
    expect(
      secondCopy,
      'the second publish embedded nothing — the withdrawal left the tree unrepeatable',
    ).toBeDefined();
    // A FRESH snapshot, not the one the withdrawal deleted.
    expect(secondCopy!.applicationId).not.toBe(firstCopy!.applicationId);

    const embedded = await readVersion(
      request,
      secondCopy!.applicationId,
      secondCopy!.versionId,
      projectId,
    );
    expect(embedded.status).toBe('embedded');
    expect(String(embedded.meta['parent_published_version_id'])).toBe(secondClone);
  } finally {
    await cleanUp(request, projectId, [parent, child]);
  }
});

test('two parents can each publish while sharing one sub-agent', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const first = await createPublishableAgent(request, autotestName('sa6_first'), projectId);
  const second = await createPublishableAgent(request, autotestName('sa6_second'), projectId);
  const shared = await createPublishableAgent(request, autotestName('sa6_shared'), projectId);

  try {
    for (const parent of [first, second]) {
      const linked = await attachSubAgent(
        request,
        parent.versionId,
        { applicationId: shared.id, versionId: shared.versionId },
        projectId,
      );
      expect(linked.status(), await refusal(linked)).toBe(201);
    }

    const firstClone = await publishAndGetClone(
      request,
      projectId,
      first.versionId,
      versionName('rel'),
    );
    const secondClone = await publishAndGetClone(
      request,
      projectId,
      second.versionId,
      versionName('rel'),
    );

    const firstCopy = embeddedCopyOf(
      await subAgentsOf(request, first.id, firstClone, projectId),
      shared,
    );
    const secondCopy = embeddedCopyOf(
      await subAgentsOf(request, second.id, secondClone, projectId),
      shared,
    );
    expect(firstCopy, 'the first parent embedded nothing').toBeDefined();
    expect(secondCopy, 'the second parent embedded nothing').toBeDefined();

    // EACH parent gets its OWN snapshot. One shared copy would make the second
    // withdrawal delete a copy the first release is still serving.
    expect(secondCopy!.applicationId).not.toBe(firstCopy!.applicationId);
    for (const [copy, parent] of [
      [firstCopy!, first],
      [secondCopy!, second],
    ] as const) {
      const embedded = await readVersion(request, copy.applicationId, copy.versionId, projectId);
      expect(String(embedded.meta['source_application_id'])).toBe(shared.id);
      expect(String(embedded.meta['parent_published_app_id'])).toBe(parent.id);
    }
  } finally {
    await cleanUp(request, projectId, [first, second, shared]);
  }
});

test('deleting a sub-agent detaches it from every parent that used it', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const first = await createPublishableAgent(request, autotestName('sa7_first'), projectId);
  const second = await createPublishableAgent(request, autotestName('sa7_second'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa7_child'), projectId);

  try {
    for (const parent of [first, second]) {
      const linked = await attachSubAgent(
        request,
        parent.versionId,
        { applicationId: child.id, versionId: child.versionId },
        projectId,
      );
      expect(linked.status(), await refusal(linked)).toBe(201);
      expect(
        (await subAgentsOf(request, parent.id, parent.versionId, projectId)).map(
          (entry) => entry.applicationId,
        ),
      ).toContain(child.id);
    }

    await deleteAgent(request, child.id, projectId);

    // BOTH parents, not only the one that happened to be looked at. A tool
    // entry pointing at a deleted agent is not an empty tool: it is a tool the
    // runtime tries to open and cannot, and the editor draws a card for an
    // agent that no longer exists.
    for (const parent of [first, second]) {
      await expect
        .poll(
          async () =>
            (await subAgentsOf(request, parent.id, parent.versionId, projectId)).some(
              (entry) => entry.applicationId === child.id,
            ),
          {
            timeout: 20_000,
            message: `${parent.name} still offers the deleted sub-agent as a tool`,
          },
        )
        .toBe(false);
    }
  } finally {
    // The child is listed here too: the delete route is idempotent, so a run
    // that failed before reaching it still cleans up, and one that reached it
    // costs a 204.
    await cleanUp(request, projectId, [first, second, child]);
  }
});

/* ── the structural guards ────────────────────────────────────────────────── */

test('a circular sub-agent graph cannot be published', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const first = await createPublishableAgent(request, autotestName('sa8_first'), projectId);
  const second = await createPublishableAgent(request, autotestName('sa8_second'), projectId);

  try {
    const forward = await attachSubAgent(
      request,
      first.versionId,
      { applicationId: second.id, versionId: second.versionId },
      projectId,
    );
    expect(forward.status(), await refusal(forward)).toBe(201);

    // The link that closes the loop. See the header: the baseline refused this
    // request and this service accepts it, refusing at publish time instead.
    // The status is asserted so that a link-time guard added later reports
    // itself here rather than leaving the case silently unbuildable.
    const backward = await attachSubAgent(
      request,
      second.versionId,
      { applicationId: first.id, versionId: first.versionId },
      projectId,
    );
    expect(backward.status(), await refusal(backward)).toBe(201);

    // The pre-publish check walks the graph with a visited set, so it TERMINATES
    // and says why. A checker that recursed without one would not answer at all.
    const validated = await publishValidate(request, projectId, first.versionId, {
      version_name: versionName('cyc'),
    });
    expect(validated.status(), await refusal(validated)).toBe(422);
    const result = (await validated.json()) as Record<string, unknown>;
    const critical = (result['critical_issues'] ?? []) as readonly Record<string, unknown>[];
    expect(critical).toHaveLength(1);
    expect(critical[0]?.['field']).toBe('sub_agents');
    expect(String(critical[0]?.['issue'])).toContain('circular');
    expect(critical[0]?.['source']).toBe('deterministic');
    // The short circuit: nothing else was checked, so the counts name the one
    // structural finding and nothing beside it.
    expect(result['counts']).toEqual({ critical: 1, warnings: 0, suggestions: 0 });
    expect(result['validation_token'], 'a failed check handed out an approval token').toBeNull();

    // …and the publish itself refuses for the same reason, so the check is not
    // an advisory a caller can skip by going straight to the publish route.
    const refused = await publish(request, projectId, first.versionId, {
      version_name: versionName('cyc'),
    });
    expect(refused.status(), await refusal(refused)).toBe(422);
    const refusedBody = (await refused.json()) as Record<string, unknown>;
    expect(refusedBody['error']).toBe('validation_failed');
    expect(JSON.stringify(refusedBody['validation_result'])).toContain('circular');

    // Nothing was published. A refusal that had already cloned the row answers
    // the same 422.
    const versions = await readApplicationVersions(request, first.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await cleanUp(request, projectId, [first, second]);
  }
});

test('a sub-agent chain deeper than three tiers is refused with one structural finding', async ({
  request,
}) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const chain: Agent[] = [];
  for (let tier = 0; tier < 5; tier += 1) {
    chain.push(await createPublishableAgent(request, autotestName(`sa9_t${tier}`), projectId));
  }

  try {
    for (let tier = 0; tier < chain.length - 1; tier += 1) {
      const linked = await attachSubAgent(
        request,
        chain[tier]!.versionId,
        { applicationId: chain[tier + 1]!.id, versionId: chain[tier + 1]!.versionId },
        projectId,
      );
      expect(linked.status(), await refusal(linked)).toBe(201);
    }

    // THE BOUNDARY, from the shallow end first: the chain that starts one tier
    // down is three hops deep and is accepted. Without this half the case would
    // pass against a checker that refused every chain.
    const shallow = await publishValidate(request, projectId, chain[1]!.versionId, {
      version_name: versionName('ok'),
    });
    expect(
      shallow.status(),
      `a three-tier chain was refused: ${await refusal(shallow)}`,
    ).toBe(200);

    const refused = await publishValidate(request, projectId, chain[0]!.versionId, {
      version_name: versionName('deep'),
    });
    expect(refused.status(), await refusal(refused)).toBe(422);
    const result = (await refused.json()) as Record<string, unknown>;
    const critical = (result['critical_issues'] ?? []) as readonly Record<string, unknown>[];

    // ONE finding, and no further checking: the tags recommendation and the
    // conversation-starters warning every agent here would otherwise raise are
    // absent, which is what says the check stopped rather than carried on
    // walking a structure it had already rejected.
    expect(result['counts']).toEqual({ critical: 1, warnings: 0, suggestions: 0 });
    expect(critical).toHaveLength(1);
    expect(critical[0]?.['field']).toBe('sub_agents');
    expect(String(critical[0]?.['issue'])).toContain('nesting depth');
    expect(critical[0]?.['source']).toBe('deterministic');
    // No `context`: the finding is about the SHAPE of the tree, not about one
    // sub-agent in it, and a context would name a node that is not at fault.
    expect(critical[0]?.['context']).toBeNull();
    expect(String(critical[0]?.['fix'] ?? ''), 'the refusal says nothing about the repair').not.toBe(
      '',
    );
    expect(result['validation_token']).toBeNull();
    expect(result['ai_validation_available']).toBe(false);
  } finally {
    await cleanUp(request, projectId, chain);
  }
});

/* ── pipelines are not agents ─────────────────────────────────────────────── */

test('an attached pipeline is neither checked nor embedded as a sub-agent', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa10_parent'), projectId);
  const agentChild = await createPublishableAgent(request, autotestName('sa10_child'), projectId);
  // A pipeline whose instructions are too short for the sub-agent rule the
  // quality gate applies to agents. If the gate treated it as a sub-agent it
  // would raise a critical finding here, which is exactly how the case tells
  // "skipped" from "happened to pass".
  const pipeline = await createAgentWithVersion(
    request,
    autotestName('sa10_pipe'),
    { agentType: 'pipeline', instructions: 'short' },
    projectId,
  );

  try {
    for (const node of [
      { applicationId: agentChild.id, versionId: agentChild.versionId },
      { applicationId: pipeline.id, versionId: pipeline.versionId },
    ]) {
      const linked = await attachSubAgent(request, parent.versionId, node, projectId);
      expect(linked.status(), await refusal(linked)).toBe(201);
    }

    // The gate: no finding is attributed to a sub-agent at all, because the one
    // node that would have raised one is a pipeline.
    const validated = await publishValidate(request, projectId, parent.versionId, {
      version_name: versionName('pipe'),
    });
    expect(validated.status(), await refusal(validated)).toBe(200);
    const result = (await validated.json()) as Record<string, unknown>;
    const everyFinding = [
      ...((result['critical_issues'] ?? []) as readonly Record<string, unknown>[]),
      ...((result['warnings'] ?? []) as readonly Record<string, unknown>[]),
    ];
    expect(
      everyFinding.filter((finding) => String(finding['context'] ?? '').startsWith('sub-agent:')),
      `a pipeline was checked as if it were a sub-agent: ${JSON.stringify(everyFinding)}`,
    ).toEqual([]);

    // The publish: the agent child is snapshotted, the pipeline is skipped
    // WITHOUT refusing the publish. A pipeline is a program, not an agent, so
    // there is nothing to embed and nothing to complain about.
    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );
    const published = await subAgentsOf(request, parent.id, cloneId, projectId);
    const copy = embeddedCopyOf(
      published.filter((entry) => entry.applicationId !== pipeline.id),
      agentChild,
    );
    expect(copy, `the agent sub-agent was not embedded: ${JSON.stringify(published)}`).toBeDefined();
    const embedded = await readVersion(request, copy!.applicationId, copy!.versionId, projectId);
    expect(String(embedded.meta['source_application_id'])).toBe(agentChild.id);

    // No snapshot of the pipeline exists: every embedded shell on this clone
    // came from the agent child.
    for (const entry of published) {
      if (entry.applicationId === agentChild.id || entry.applicationId === pipeline.id) continue;
      const shell = await readVersion(request, entry.applicationId, entry.versionId, projectId);
      expect(
        String(shell.meta['source_application_id']),
        'the publish embedded a copy of the attached pipeline',
      ).not.toBe(pipeline.id);
    }
  } finally {
    await cleanUp(request, projectId, [
      parent,
      agentChild,
      { id: pipeline.id, versionId: pipeline.versionId, name: 'pipeline' },
    ]);
  }
});

test('a pipeline itself cannot be published to the catalogue', async ({ request }) => {
  const projectId = await resolvePublishAuthorProjectId(request);
  const pipeline = await createAgentWithVersion(
    request,
    autotestName('sa11_pipe'),
    {
      agentType: 'pipeline',
      instructions:
        'This graph is long enough that the quality gate would pass it if the gate ever ran.',
    },
    projectId,
  );

  try {
    const refused = await publish(request, projectId, pipeline.versionId, {
      version_name: versionName('rel'),
    });
    // 400 and NAMED, not the generic validation 422: the refusal is about what
    // the entity IS, and the publish dialog renders it differently from a
    // quality finding the author can act on. Refused before the quality gate,
    // which is why the instructions above would otherwise have passed it.
    expect(refused.status(), await refusal(refused)).toBe(400);
    expect((await refused.json()) as Record<string, unknown>).toMatchObject({
      error: 'pipeline_not_publishable',
    });

    const versions = await readApplicationVersions(request, pipeline.id, projectId);
    expect(versions.every((version) => version.status !== 'published')).toBe(true);
  } finally {
    await withdrawAndDelete(request, projectId, pipeline.id).catch(() => {});
  }
});

/* ── the moderator's in-place publish ─────────────────────────────────────── */

test('a moderator publishing inside the catalogue embeds the sub-agents and keeps the originals as drafts', async ({
  request,
}) => {
  // The CATALOGUE project, resolved from the deployment's own settings. This is
  // the in-place flow: the published version IS the catalogue row, so there is
  // no twin and the public agent detail below reads the same schema.
  const projectId = await resolveCatalogueProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa12_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa12_child'), projectId);

  try {
    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: child.versionId },
      projectId,
    );
    expect(linked.status(), await refusal(linked)).toBe(201);

    const before = await readApplicationVersions(request, parent.id, projectId);
    const cloneId = await publishAndGetClone(
      request,
      projectId,
      parent.versionId,
      versionName('rel'),
    );

    // A SIBLING version on the same agent, not a second agent shell. The
    // in-place flow's whole point is that the moderator's agent gains a
    // published version rather than a duplicate record.
    const after = await readApplicationVersions(request, parent.id, projectId);
    expect(after).toHaveLength(before.length + 1);
    expect(after.find((version) => version.id === cloneId)?.status).toBe('published');
    expect(after.find((version) => version.id === parent.versionId)?.status).toBe('draft');

    // The embed still happens here, which is what makes the moderator's
    // release independent of the source agents.
    const copy = embeddedCopyOf(await subAgentsOf(request, parent.id, cloneId, projectId), child);
    expect(copy, 'the in-place publish embedded nothing').toBeDefined();
    const embedded = await readVersion(request, copy!.applicationId, copy!.versionId, projectId);
    expect(embedded.status).toBe('embedded');

    // …and the source sub-agent stays a draft.
    const childVersions = await readApplicationVersions(request, child.id, projectId);
    expect(childVersions.every((version) => version.status === 'draft')).toBe(true);

    // The withdrawal removes the published row and the copies, and preserves
    // the agent with the draft the moderator keeps editing.
    const withdrawn = await unpublish(request, projectId, cloneId);
    expect(withdrawn.status(), await refusal(withdrawn)).toBe(200);
    await expect
      .poll(
        async () => versionIsGone(request, copy!.applicationId, copy!.versionId, projectId),
        { timeout: 20_000, message: 'the embedded copy survived the moderator withdrawal' },
      )
      .toBe(true);

    const survivors = await readApplicationVersions(request, parent.id, projectId);
    expect(survivors.find((version) => version.id === parent.versionId)?.status).toBe('draft');
    const childSurvivors = await readApplicationVersions(request, child.id, projectId);
    expect(
      childSurvivors.some((version) => version.id === child.versionId),
      'the moderator withdrawal deleted the source sub-agent',
    ).toBe(true);
  } finally {
    await cleanUp(request, projectId, [parent, child]);
  }
});

test('the public agent detail names the project of every sub-agent tool, at each published level', async ({
  request,
}) => {
  const projectId = await resolveCatalogueProjectId(request);
  const parent = await createPublishableAgent(request, autotestName('sa13_parent'), projectId);
  const child = await createPublishableAgent(request, autotestName('sa13_child'), projectId);
  const grandchild = await createPublishableAgent(request, autotestName('sa13_grand'), projectId);

  try {
    for (const [parentVersionId, node] of [
      [parent.versionId, child],
      [child.versionId, grandchild],
    ] as const) {
      const linked = await attachSubAgent(
        request,
        parentVersionId,
        { applicationId: node.id, versionId: node.versionId },
        projectId,
      );
      expect(linked.status(), await refusal(linked)).toBe(201);
    }

    // TWO levels published, so "at each level" is a real claim: the child is a
    // catalogue entry of its own AND a sub-agent of the parent.
    await publishAndGetClone(request, projectId, child.versionId, versionName('rel'));
    await publishAndGetClone(request, projectId, parent.versionId, versionName('rel'));

    for (const agent of [parent, child]) {
      const detail = await request.get(
        `${API_BASE}/elitea_core/public_application/prompt_lib/${agent.id}`,
      );
      expect(detail.status(), await refusal(detail)).toBe(200);
      const body = (await detail.json()) as {
        version_details?: { tools?: readonly Record<string, unknown>[] };
      };
      const tools = body.version_details?.tools ?? [];
      const subAgents = tools.filter((tool) => tool['type'] === 'application');
      expect(
        subAgents.length,
        `${agent.name}'s public detail lists no sub-agent: ${JSON.stringify(tools)}`,
      ).toBeGreaterThan(0);
      for (const tool of subAgents) {
        // Without this a reader of the catalogue holds an agent id and no way
        // to say which project's schema it lives in — and every project has
        // its own id sequence, so the id alone resolves to the wrong agent
        // just as readily as to none.
        expect(
          String(tool['project_id'] ?? ''),
          `a sub-agent tool of ${agent.name} names no project: ${JSON.stringify(tool)}`,
        ).toBe(projectId);
      }
    }
  } finally {
    await cleanUp(request, projectId, [parent, child, grandchild]);
  }
});
