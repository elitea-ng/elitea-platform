/**
 * THE AGENT'S INBOUND WEBHOOK, FROM THE EDITOR TO A REAL SENDER (legacy issue
 * 6656).
 *
 * Legacy shipped webhooks for ordinary agents and then lost them: every agent
 * call answered 400 "Webhook secret not configured". The Go stack opened the
 * pipeline's inbound trigger to agent versions, and the agent editor got a
 * Triggers section. This file drives both halves: the section mints the
 * credential in the browser, and `e2e/fixtures/pipelineTriggers.ts`'s
 * ANONYMOUS sender calls the URL the way a webhook sender does — with no
 * session cookie, because the inbound route is mounted above the Auth group.
 *
 * ── WHAT THIS STACK CAN AND CANNOT SAY ──────────────────────────────────
 *
 * The journeys stack composes no worker, so an ACCEPTED credential answers
 * 503 with the runtime's own sentence, and a refused one answers 401 with the
 * shared refusal. `expectCredentialAccepted` admits the 202 a stack with a
 * runtime answers too. See `pipelines.webhook-trigger.spec.ts` for the full
 * reasoning; it is the same route.
 */
import { createHmac } from 'node:crypto';

import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent, type CreatedAgent } from '../../fixtures/api';
import {
  GITHUB_SIGNATURE_HEADER,
  expectCredentialAccepted,
  expectCredentialRefused,
  readInboundTrigger,
  revokeInboundTrigger,
  sendWebhook,
  webhookSender,
} from '../../fixtures/pipelineTriggers';

const created: CreatedAgent[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const agent = created.pop();
    if (agent === undefined) continue;
    await revokeInboundTrigger(page.request, { projectId: DEFAULT_PROJECT_ID, versionId: agent.versionId });
    await deleteAgent(page.request, agent.id);
  }
});

async function agentWithEditorOpen(page: Page, label: string): Promise<CreatedAgent> {
  const name = `${AUTOTEST_PREFIX}agent-wh-${label}-${String(Date.now() % 1e9)}-${String(Math.floor(Math.random() * 1e4))}`;
  const agent = await createAgent(page.request, name);
  created.push(agent);
  await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('edit-application-triggers-panel')).toBeVisible();
  return agent;
}

test('the agent editor mints a webhook a sender can call, and revoking it stops the sender', async ({ page }) => {
  test.setTimeout(90_000);
  const agent = await agentWithEditorOpen(page, 'bearer');
  const scope = { projectId: DEFAULT_PROJECT_ID, versionId: agent.versionId };

  // A new agent has no trigger, and says so rather than showing an empty URL.
  await expect(page.getByTestId('agent-trigger-absent')).toBeVisible();
  await page.getByTestId('agent-trigger-rotate').click();

  // The credential is shown once, after the create.
  const secretRow = page.getByTestId('agent-trigger-secret');
  await expect(secretRow).toBeVisible();
  const secret = (await secretRow.textContent())?.trim() ?? '';
  expect(secret.length, 'the create shows the minted secret').toBeGreaterThan(20);
  await expect(page.getByTestId('agent-trigger-url')).toContainText('/api/v2/pipeline_trigger/');

  // The plain read never carries it.
  const stored = await readInboundTrigger(page.request, scope);
  expect(stored.configured).toBe(true);
  expect(stored.secret, 'the plain read must not carry the credential').toBe('');

  const sender = await webhookSender();
  try {
    await expectCredentialAccepted(
      await sendWebhook(sender, stored, { secret, body: '{"input":"Summarise the release notes"}' }),
      'the agent webhook with the secret the editor showed',
    );
    await expectCredentialRefused(
      await sendWebhook(sender, stored, { secret: 'not-the-secret', body: '{"input":"x"}' }),
      'the agent webhook with a wrong secret',
    );

    // Hide, then revoke from the editor. The revoked state is drawn, not "absent".
    await page.getByTestId('agent-trigger-hide').click();
    await expect(secretRow).toBeHidden();
    await page.getByTestId('agent-trigger-revoke').click();
    await expect(page.getByTestId('agent-trigger-revoked')).toBeVisible();

    await expectCredentialRefused(
      await sendWebhook(sender, stored, { secret, body: '{"input":"x"}' }),
      'the agent webhook after the editor revoked it',
    );
  } finally {
    await sender.dispose();
  }
});

test('a GitHub-type agent webhook is created from the editor and verifies the signed payload', async ({ page }) => {
  test.setTimeout(90_000);
  const agent = await agentWithEditorOpen(page, 'github');

  await page.locator('#agent-trigger-mode').click();
  await page.getByRole('option', { name: 'GitHub (signed payload)' }).click();
  await expect(page.getByTestId('agent-trigger-mode-hint')).toContainText(GITHUB_SIGNATURE_HEADER);
  await page.getByTestId('agent-trigger-rotate').click();

  const secretRow = page.getByTestId('agent-trigger-secret');
  await expect(secretRow).toBeVisible();
  const secret = (await secretRow.textContent())?.trim() ?? '';
  await expect(page.getByTestId('agent-trigger-url')).toContainText('/github');

  const stored = await readInboundTrigger(page.request, { projectId: DEFAULT_PROJECT_ID, versionId: agent.versionId });
  expect(stored.authMode, 'the GitHub type stores the signature mode').toBe('hmac_sha256');

  // A GitHub event carries no `input`: the agent reads the payload itself.
  const body = '{"action":"opened","pull_request":{"number":7,"title":"Fix the build"}}';
  const signature = `sha256=${createHmac('sha256', secret).update(body).digest('hex')}`;
  const sender = await webhookSender();
  try {
    await expectCredentialAccepted(
      await sender.post(stored.url, {
        headers: { 'content-type': 'application/json', [GITHUB_SIGNATURE_HEADER]: signature },
        data: body,
      }),
      'a correctly signed GitHub delivery to an agent',
    );
    await expectCredentialRefused(
      await sender.post(stored.url, {
        headers: { 'content-type': 'application/json', [GITHUB_SIGNATURE_HEADER]: 'sha256=00' },
        data: body,
      }),
      'a tampered signature',
    );
    await expectCredentialRefused(
      await sendWebhook(sender, stored, { secret, body }),
      'the signing secret presented as a bearer',
    );
  } finally {
    await sender.dispose();
  }
});
