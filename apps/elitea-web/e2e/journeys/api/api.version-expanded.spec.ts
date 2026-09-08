/**
 * The EXPANDED version details — the projection the runtime reads, behind the
 * project's `X-SECRET` header.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's expanded-version cases: AVR-01 (with the project's
 * secret header, an agent version resolves each toolkit's credential in full so
 * the runtime can reach the service) and PUB-68 (the expanded definition tells
 * the runtime which project each sub-agent tool lives in). The refusal half of
 * AVR-01 — a header that is not the project's own — is asserted here too,
 * because a read that expands credentials is only safe while it refuses.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS A DIFFERENT READ, AND NOT A VARIANT OF THE EDITOR'S
 * ─────────────────────────────────────────────────────────────────────────────
 * `GET /elitea_core/version/…` is the agent editor's reload. `PATCH` on the
 * SAME path is the SDK's `get_app_version_details` — a body-less read that
 * carries the header, which pylon shaped that way and this service kept
 * (`internal/api/v2/applications/handler.go`, `GetVersionExpanded`). The two
 * are built by different code over the same row and they answer DIFFERENTLY on
 * purpose: the editor is shown a credential REFERENCE, the runtime is given the
 * credential. So every case below asserts on both, and the pair is the
 * assertion — a handler that expanded nothing would satisfy either read alone.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE SECRET, AND WHY IT IS RESOLVED RATHER THAN WRITTEN DOWN
 * ─────────────────────────────────────────────────────────────────────────────
 * The header value is per project and random: provisioning seals one into every
 * new project's vault. A journey holding a literal would need one configured
 * per environment and, worse, its "a wrong header is refused" case would keep
 * passing everywhere the literal had gone stale. `resolveProjectSecretHeader`
 * reads the project's own value and mints one when the project has none — see
 * its own note for why the journeys' stack is a project that has none.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every agent, toolkit and credential is `autotest_*` and is removed
 * in a `finally`, toolkit before credential: the toolkit references the
 * credential by title, and a credential removed first leaves a toolkit whose
 * reference resolves to nothing.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachSubAgent,
  createAgentWithVersion,
  createGithubToolkit,
  deleteAgent,
  deleteGithubToolkit,
  readVersion,
  readVersionExpanded,
  resolveProjectSecretHeader,
  subAgentToolsOf,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

import type { APIRequestContext } from '@playwright/test';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const AGENT_INSTRUCTIONS =
  'You are a repository assistant. Answer questions about the code the caller names.';

async function createAgent(
  request: APIRequestContext,
  name: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  return createAgentWithVersion(
    request,
    name,
    { instructions: AGENT_INSTRUCTIONS },
    DEFAULT_PROJECT_ID,
  );
}

/** The expanded read's `tools`, whatever it served. */
function toolsOf(expanded: Record<string, unknown>): readonly Record<string, unknown>[] {
  const tools = expanded['tools'];
  return Array.isArray(tools) ? (tools as Record<string, unknown>[]) : [];
}

/** The version-detail URL, for the raw requests the refusal cases make. */
function versionURL(applicationId: string, versionId: string): string {
  return (
    `${API_BASE}/elitea_core/version/prompt_lib/${DEFAULT_PROJECT_ID}/` +
    `${applicationId}/${versionId}`
  );
}

/* ── AVR-01: the credential arrives resolved ──────────────────────────────── */

test('the expanded read resolves a toolkit credential in full, where the editor read sees only the reference', async ({
  request,
}) => {
  const name = autotestName('avr_expand');
  // Not a credential-shaped literal from anywhere: minted per run, and the
  // endpoint it authenticates against is the deliberately unroutable
  // placeholder the toolkit fixture uses.
  const accessToken = `autotest-token-${Math.random().toString(36).slice(2, 12)}`;
  const agent = await createAgent(request, name);
  let toolkit: Awaited<ReturnType<typeof createGithubToolkit>> | undefined;

  try {
    toolkit = await createGithubToolkit(request, DEFAULT_PROJECT_ID, name, {
      credentialData: { access_token: accessToken },
    });

    // Attach the toolkit to the version, which is what puts the credential
    // reference on the row the two reads then project differently.
    //
    // Through the TOOLKIT route, not a version write: the version PUT reads no
    // `tools` key at all (`applications/handler.go`), so a journey that sent
    // one would get its 201 and attach nothing.
    const attached = await request.put(
      `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkit.toolkitId}`,
      {
        data: {
          has_relation: true,
          entity_version_id: agent.versionId,
          entity_id: agent.id,
          entity_type: 'agent',
          selected_tools: [],
        },
      },
    );
    expect(attached.status(), await attached.text()).toBe(201);

    // ── the EDITOR's read: a reference, and no credential at all ──────────
    const stored = await readVersion(request, agent.id, agent.versionId);
    const editorTool = stored.tools.find((tool) => tool['type'] === 'github');
    const editorSettings =
      ((editorTool?.['config'] ?? editorTool?.['settings']) as Record<string, unknown>) ?? {};
    const editorReference = (editorSettings['github_configuration'] ?? {}) as Record<
      string,
      unknown
    >;
    expect(
      editorReference['elitea_title'],
      `the editor read lost the credential reference: ${JSON.stringify(stored.tools)}`,
    ).toBe(toolkit.credentialTitle);
    // The editor is shown the REFERENCE and nothing else. A read that already
    // carried the token here would be handing the browser a secret it has no
    // use for.
    expect(editorReference['access_token']).toBeUndefined();

    // ── the RUNTIME's read: the credential, resolved and unsecreted ───────
    const secret = await resolveProjectSecretHeader(request, DEFAULT_PROJECT_ID);
    const expanded = await readVersionExpanded(
      request,
      agent.id,
      agent.versionId,
      secret,
      DEFAULT_PROJECT_ID,
    );
    const runtimeTool = toolsOf(expanded).find((tool) => tool['type'] === 'github');
    expect(runtimeTool, `no github toolkit in ${JSON.stringify(toolsOf(expanded))}`).toBeDefined();
    const runtimeSettings = (runtimeTool?.['settings'] ?? {}) as Record<string, unknown>;
    const credential = (runtimeSettings['github_configuration'] ?? {}) as Record<string, unknown>;

    // The five keys the resolution adds beside the reference: without them the
    // worker knows a title and cannot say which project or type it names.
    expect(credential['elitea_title']).toBe(toolkit.credentialTitle);
    expect(credential['configuration_type']).toBe('github');
    expect(String(credential['configuration_project_id'])).toBe(DEFAULT_PROJECT_ID);
    expect(credential['configuration_uuid'], 'the resolved credential names no row').toBeTruthy();
    expect(credential['private']).toBe(false);

    // …and the credential's own stored data, with the SEALED field opened.
    // This is the whole point of the route: the runtime authenticates with it.
    expect(credential['base_url']).toBe('https://autotest.invalid/api');
    expect(
      credential['access_token'],
      'the expanded read served the vault reference instead of the secret it stands for',
    ).toBe(accessToken);

    // It was sealed at rest. Asserted through the credential row itself,
    // because "the expanded read returned the token" is also what a service
    // that never sealed it would answer.
    const row = await request.get(
      `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${toolkit.credentialId}`,
    );
    expect(row.status(), await row.text()).toBe(200);
    const rowData = ((await row.json()) as { data?: Record<string, unknown> }).data ?? {};
    expect(String(rowData['access_token'])).toMatch(/^\{\{secret\./);
    expect(String(rowData['access_token'])).not.toContain(accessToken);
  } finally {
    await deleteAgent(request, agent.id, DEFAULT_PROJECT_ID);
    await deleteGithubToolkit(request, DEFAULT_PROJECT_ID, toolkit);
  }
});

/* ── AVR-01: and it refuses anything but the project's own header ─────────── */

test('a wrong or missing secret header is refused and discloses nothing about the version', async ({
  request,
}) => {
  const agent = await createAgent(request, autotestName('avr_refuse'));

  try {
    const url = versionURL(agent.id, agent.versionId);

    // A header that is not the project's. The refusal names the header rather
    // than the version, and it is a 400 rather than a 404: the caller's problem
    // is what they sent, and telling them the version is missing would send
    // them to repair the wrong thing.
    const wrong = await request.patch(url, { headers: { 'X-SECRET': 'not-the-vault-value' } });
    expect(wrong.status(), await wrong.text()).toBe(400);
    const wrongBody = (await wrong.json()) as Record<string, unknown>;
    expect(wrongBody).toMatchObject({ error: 'Invalid secret header' });
    // Nothing about the agent leaks through the refusal.
    expect(wrongBody['instructions']).toBeUndefined();
    expect(wrongBody['llm_settings']).toBeUndefined();
    expect(wrongBody['tools']).toBeUndefined();

    // No header at all is the same refusal. An empty value is what a caller
    // that sets none sends, so the two must not part company: a route that
    // accepted the empty string would authenticate every unconfigured client.
    const missing = await request.patch(url);
    expect(missing.status(), await missing.text()).toBe(400);
    expect((await missing.json()) as Record<string, unknown>).toMatchObject({
      error: 'Invalid secret header',
    });

    // The project's own value still passes, so the refusals above are the
    // header's doing and not a route that refuses everyone.
    const secret = await resolveProjectSecretHeader(request, DEFAULT_PROJECT_ID);
    const accepted = await request.patch(url, { headers: { 'X-SECRET': secret } });
    expect(accepted.status(), await accepted.text()).toBe(200);
    expect(((await accepted.json()) as Record<string, unknown>)['instructions']).toBe(
      AGENT_INSTRUCTIONS,
    );
  } finally {
    await deleteAgent(request, agent.id, DEFAULT_PROJECT_ID);
  }
});

/* ── PUB-68: every sub-agent tool names the project it lives in ───────────── */

test('the expanded read names the project each sub-agent tool lives in', async ({ request }) => {
  const parent = await createAgent(request, autotestName('pub68_parent'));
  const child = await createAgent(request, autotestName('pub68_child'));

  try {
    const linked = await attachSubAgent(
      request,
      parent.versionId,
      { applicationId: child.id, versionId: child.versionId },
      DEFAULT_PROJECT_ID,
    );
    expect(linked.status(), await linked.text()).toBe(201);

    const secret = await resolveProjectSecretHeader(request, DEFAULT_PROJECT_ID);
    const expanded = await readVersionExpanded(
      request,
      parent.id,
      parent.versionId,
      secret,
      DEFAULT_PROJECT_ID,
    );
    const tools = toolsOf(expanded);
    const subAgents = subAgentToolsOf(tools);
    expect(subAgents, `no sub-agent in ${JSON.stringify(tools)}`).toHaveLength(1);
    expect(subAgents[0]?.applicationId).toBe(child.id);
    expect(subAgents[0]?.versionId).toBe(child.versionId);

    // THE POINT OF THE CASE. An agent id alone does not say where the agent
    // is: every project has its own schema and its own id sequence, so a
    // runtime handed `application_id` and nothing else cannot open the child.
    // `project_id` is the key that makes the reference resolvable, and it was
    // served only on the pylon-era row shape — a table no schema this service
    // builds has — so it reached no caller at all.
    const subAgentTool = tools.find((tool) => tool['type'] === 'application');
    expect(
      subAgentTool?.['project_id'],
      `the sub-agent tool names no project: ${JSON.stringify(subAgentTool)}`,
    ).toBeDefined();
    expect(String(subAgentTool?.['project_id'])).toBe(DEFAULT_PROJECT_ID);
  } finally {
    await deleteAgent(request, parent.id, DEFAULT_PROJECT_ID);
    await deleteAgent(request, child.id, DEFAULT_PROJECT_ID);
  }
});
