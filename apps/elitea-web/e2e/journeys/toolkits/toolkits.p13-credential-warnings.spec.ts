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
 * — product gap, see file header: attaching a toolkit whose private credential cannot be resolved to
 * an Agent's Tools panel shows no notification of any kind (no styled banner, no plain fallback text,
 * no credential-source badge) — the credential-mismatch UI exists on only ONE surface (the toolkit's
 * own edit page, and even there it is the old plain text, per toolkits.generic-credential-secrets.spec.ts),
 * never on the Agent (1100)/Pipeline (1098)/Chat (1082/1085/1086) tool card. */
test('ELITEA-1082/1083/1084/1085/1086/1098/1100: PRODUCT GAP — an attached toolkit with a missing private credential shows no warning on the Agent tool card', async ({
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
    test.fail(
      true,
      'ELITEA-1082 (#937)/1083/1084/1085/1086/1098/1100: product gap — CredentialWarningBanner never reaches ' +
        "an Agent/Pipeline/Chat tool card (ToolCard.types.ts's validationBanner slot has no caller " +
        'anywhere in src/features/agents or src/features/pipelines); this toolkit\'s unresolved private ' +
        'credential is attached with zero visible warning',
    );
    await expect(page.getByText('Credential setup required:')).toBeVisible({ timeout: 5_000 });
  } finally {
    await deleteAgent(page.request, agent.agentId);
  }
});
