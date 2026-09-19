/**
 * P13 rejudge — `toolkits-credentials` folder, the "attached toolkit has a
 * private-credential problem" family (ELITEA-1082, 1083, 1084, 1085, 1086,
 * 1098, 1100).
 *
 * `e2e/journeys/toolkits/toolkits.generic-credential-secrets.spec.ts` already
 * ledgers a closely related but DIFFERENT set of ids (ELITEA-1088/1090/1091/
 * 1093/1097) as a confirmed product gap: on the TOOLKIT's own edit page,
 * `ToolkitCredentialPicker` (`pages/toolkits/lib/credentialPicker.tsx`)
 * hardcodes `mismatch={{ mismatchedPrivateCredential: false, … }}`, so
 * `CredentialsSelect` never renders the styled `CredentialWarningBanner` for
 * a real mismatch — only the old plain "Your configuration does not match
 * any available configurations." text.
 *
 * This file covers the DIFFERENT surface these six ids ask about: does any
 * warning/notification appear when the SAME kind of toolkit is ATTACHED to
 * an Agent (or Chat, or Pipeline) rather than opened on its own edit page.
 * Repo-wide evidence settles it without needing a live Pipeline/Chat drive
 * too: `<CredentialWarningBanner` (the ONE component that ever renders the
 * "Credential setup required:" text) appears in exactly two places in all of
 * `src/` — its own unit test, and `CredentialMismatchFooter.tsx` (which
 * `CredentialsSelect.tsx` renders, i.e. the SAME toolkit-edit-page surface
 * the gap above already covers). `ToolCard.types.ts`'s own doc comment
 * describes a `validationBanner` slot an Agent/Pipeline tool card COULD fill
 * with this banner ("the caller renders the whole banner and hands it in
 * fully composed") — but no caller anywhere in `src/features/agents` or
 * `src/features/pipelines` ever fills it. So the banner never reaches the
 * Agent tool card, the Pipeline tool card, or a Chat participant card
 * either — one demonstration (the Agent surface, which has a robust,
 * already-proven attach helper) stands for all three, and this file's own
 * repo-wide grep is the evidence for the other two.
 *
 * ELITEA-1084's contrast claim ("no warning when credentials match, proving
 * the warning logic is conditional") is unfalsifiable given the above: there
 * is no warning path AT ALL, matched or mismatched, so the contrast the case
 * wants to observe cannot exist either way. ELITEA-1083 ("credential source
 * clearly indicated") is folded into the same finding: `CredentialsSelect`
 * has no private-vs-project source indicator to inspect once the mismatch
 * banner itself is confirmed absent.
 *
 * ELITEA-1087 (private credentials are prioritised over project credentials
 * at RESOLUTION time) is NOT this file's territory — proving which
 * credential a tool call actually used needs a real tool execution (the
 * case's own last step: "Execute a toolkit tool to confirm credential usage
 * … via audit logs or tool behavior"), i.e. the chat-stream lane. Ledgered
 * STREAM-DEFERRED.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  attachToolkitThroughPicker,
  createAgentThroughForm,
  deleteAgent,
} from '../../fixtures/api';

const RUN_ID = String(Date.now()).slice(-6);

const createdCredentialIds: string[] = [];
const createdToolkitIds: string[] = [];

test.afterAll(async ({ browser }) => {
  const ctx = await browser.newContext();
  for (const id of createdToolkitIds) {
    await ctx.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  for (const id of createdCredentialIds) {
    await ctx.request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  await ctx.close();
});

/** A real `github` credential, then removed by the caller to leave a dangling reference. */
async function createGithubCredential(request: APIRequestContext, eliteaTitle: string): Promise<string> {
  const created = await request.post(`${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`, {
    data: { elitea_title: eliteaTitle, label: eliteaTitle, type: 'github', data: { base_url: 'https://autotest.invalid/api' } },
  });
  expect(created.status(), await created.text()).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  if (id !== '') createdCredentialIds.push(id);
  return eliteaTitle;
}

async function createGithubToolkitReferencing(
  request: APIRequestContext,
  name: string,
  credentialTitle: string,
): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      type: 'github',
      settings: {
        github_configuration: { elitea_title: credentialTitle, private: false },
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        selected_tools: [],
      },
    },
  });
  expect(created.status(), await created.text()).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(id).not.toBe('');
  createdToolkitIds.push(id);
  return id;
}

/* onetest: ELITEA-1082, ELITEA-1083, ELITEA-1084, ELITEA-1085, ELITEA-1086, ELITEA-1098, ELITEA-1100
 * (#917-adjacent; fixed in #937) — attaching a toolkit whose credential reference no longer resolves to
 * an Agent's Tools panel now shows the styled "Credential setup required:" banner on the tool card.
 *
 * `ToolCard.types.ts` always declared a `validation.banner` slot for exactly this and NO caller ever
 * filled it, so the banner was reachable from the toolkit's own edit page and nowhere else. It is filled
 * now from `features/agents/ui/AgentToolRow.tsx`, off the toolkit's own stored `{elitea_title, private}`
 * reference checked against the project that would hold that credential. The SAME component renders the
 * PIPELINE tool card (`pages/pipelines/ui/EditPipelineToolsPanel.tsx` mounts the same `AgentToolsPanel`),
 * so ELITEA-1098 is covered by construction — there is one card, not two.
 */
test('ELITEA-1082/1083/1085/1086/1098/1100: an attached toolkit with a missing credential warns on the Agent tool card', async ({
  page,
}) => {
  test.setTimeout(120_000);

  const goneCredentialTitle = `${AUTOTEST_PREFIX}p13cred_gone_${RUN_ID}`;
  await createGithubCredential(page.request, goneCredentialTitle);
  await createGithubToolkitReferencing(
    page.request,
    `${AUTOTEST_PREFIX}p13tkmismatch_${RUN_ID}`,
    goneCredentialTitle,
  );
  const goneCredentialId = createdCredentialIds.at(-1);
  expect(goneCredentialId).toBeDefined();
  const deleted = await page.request.delete(
    `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${goneCredentialId}`,
  );
  expect(deleted.ok(), await deleted.text()).toBe(true);
  createdCredentialIds.pop();

  const agent = await createAgentThroughForm(page, `${AUTOTEST_PREFIX}p13ag_credwarn_${RUN_ID}`);
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });

  await attachToolkitThroughPicker(page, `${AUTOTEST_PREFIX}p13tkmismatch_${RUN_ID}`);

  try {
    await expect(page.getByText('Credential setup required:')).toBeVisible({ timeout: 20_000 });
    // ELITEA-1083 — the banner names the credential the toolkit is looking
    // for, which is the only thing that makes it actionable.
    await expect(page.getByText(goneCredentialTitle, { exact: false })).toBeVisible();
  } finally {
    await deleteAgent(page.request, agent.agentId);
  }
});

/*
 * onetest: ELITEA-1084 — the contrast case, and the one that discriminates.
 *
 * The case asks for "no warning when credentials match, proving the warning logic is conditional". It was
 * unfalsifiable while there was no warning path at all; it is the load-bearing assertion now. A broken
 * credential READ (wrong project, wrong section, a 4xx swallowed into an empty list) makes EVERY attached
 * toolkit look unresolved, which passes the test above for entirely the wrong reason. This one fails on it.
 */
test('ELITEA-1084: an attached toolkit whose credential still EXISTS shows no warning', async ({ page }) => {
  test.setTimeout(120_000);

  const liveCredentialTitle = `${AUTOTEST_PREFIX}p13cred_live_${RUN_ID}`;
  await createGithubCredential(page.request, liveCredentialTitle);
  await createGithubToolkitReferencing(page.request, `${AUTOTEST_PREFIX}p13tkmatch_${RUN_ID}`, liveCredentialTitle);

  const agent = await createAgentThroughForm(page, `${AUTOTEST_PREFIX}p13ag_credok_${RUN_ID}`);
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });

  await attachToolkitThroughPicker(page, `${AUTOTEST_PREFIX}p13tkmatch_${RUN_ID}`);

  try {
    // The card really mounted — otherwise the absence below is vacuous.
    await expect(page.getByTestId('agent-toolkit-card').first()).toBeVisible({ timeout: 30_000 });
    // Give the credential read the same window the positive case gets, so
    // "not yet fetched" cannot masquerade as "resolved".
    await page.waitForTimeout(3_000);
    await expect(page.getByText('Credential setup required:')).toHaveCount(0);
  } finally {
    await deleteAgent(page.request, agent.agentId);
  }
});
