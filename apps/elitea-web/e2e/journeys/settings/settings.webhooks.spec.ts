/**
 * Journey: Settings: create, rotate, disable and delete a webhook (#876)
 *
 * Before this issue the outbound project webhook registry
 * (`internal/api/webhook/handler.go`'s five routes, mounted at
 * `/webhooks/prompt_lib/{projectID}`) was API-only — see
 * `entries/docs/content/how-tos/credentials-toolkits/webhooks-and-triggers.mdx`'s
 * "Webhook API" section, which carried a "Not available yet" note until this
 * change. This journey exercises the new `/settings/webhooks` page end to
 * end and cross-checks every write against the server, the same "assert via
 * the product's own read AND via a direct API call" bar `settings.secrets
 * .spec.ts` (J21) sets for the sibling settings tab.
 *
 * NOTE ON "triggering" THIS RESOURCE: a webhook registered here is a
 * destination Elitea calls OUT to (URL + events + secret) — the opposite
 * direction from a pipeline's own INBOUND trigger URL (`pipeline_triggers`,
 * covered by its own journeys), which a caller POSTs to in order to start a
 * run. POSTing to an outbound webhook's own `url` field does not reach
 * Elitea at all — it is the address of an external receiver, not a route on
 * this server — so this journey does not attempt that. What it proves
 * end-to-end instead is the full CRUD lifecycle the issue actually asks for:
 * create (secret revealed once), the row and its secret persisting on the
 * server, rotate (secret changes, url/events/active unchanged), disable
 * (active flips server-side), and delete (row gone from a fresh GET).
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const WEBHOOKS_PAGE = `${BASE_URL}/app/settings/webhooks`;
const WEBHOOKS_URL = `${API_BASE}/webhooks/prompt_lib/${DEFAULT_PROJECT_ID}`;
const webhookUrl = (id: string): string => `${WEBHOOKS_URL}/${encodeURIComponent(id)}`;

interface WebhookWire {
  readonly id: string;
  readonly url: string;
  readonly events: readonly string[];
  readonly secret: string;
  readonly active: boolean;
}

/** Unique per run/engine, so concurrent workers/engines never collide on the destination URL a row is found by. */
const engineSuffix = (projectName: string): string => `_${projectName.replace(/[^a-z0-9]+/gi, '').toLowerCase()}`;

/** IDs this worker created — swept in `afterAll` regardless of test outcome. */
const createdHere = new Set<string>();

async function listWebhooks(request: import('@playwright/test').APIRequestContext): Promise<WebhookWire[]> {
  const resp = await request.get(WEBHOOKS_URL);
  expect(resp.status(), `list webhooks: ${await resp.text()}`).toBe(200);
  const body = (await resp.json()) as { items: WebhookWire[] };
  return body.items;
}

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: STORAGE_STATE.member });
  try {
    for (const id of createdHere) {
      const resp = await context.request.delete(webhookUrl(id));
      // Idempotent cleanup: a row this test already deleted itself answers
      // 404, which is fine — the goal is "gone", not "this call deleted it".
      expect([204, 404], `cleanup: delete webhook ${id} answered ${resp.status()}`).toContain(resp.status());
    }
  } finally {
    await context.close();
  }
});

test('Settings/webhooks renders its real page chrome', async ({ page }) => {
  await page.goto(WEBHOOKS_PAGE);

  await expect(page.getByRole('heading', { name: 'Webhooks' }).or(page.getByText('Webhooks', { exact: true })).first())
    .toBeVisible({ timeout: 15_000 });
  const add = page.getByRole('button', { name: 'Register a new webhook' });
  await expect(add).toBeEnabled({ timeout: 15_000 });

  await checkA11y(page);
});

test('Settings: create, rotate, disable and delete a webhook', async ({ page }, testInfo) => {
  test.setTimeout(60_000);

  const destination = `https://example.com/${AUTOTEST_PREFIX}j-webhook${engineSuffix(testInfo.project.name)}_${Date.now()}`;

  /* ── create ──────────────────────────────────────────────────────────── */
  await page.goto(WEBHOOKS_PAGE);
  await page.getByRole('button', { name: 'Register a new webhook' }).click({ timeout: 10_000 });

  const dialog = page.getByTestId('webhook-form-dialog');
  await expect(dialog).toBeVisible({ timeout: 5_000 });
  await dialog.getByTestId('webhook-form-url').locator('input').fill(destination, { timeout: 3_000 });
  // #876's second half swapped the free-text comma field for an MUI
  // Autocomplete (multiple, freeSolo — see webhookEventCatalogue.ts): each
  // event is typed and committed with Enter, the standard MUI interaction
  // for adding a freeSolo chip, rather than filled as one comma-joined
  // string.
  const eventsInput = dialog.getByTestId('webhook-form-events').locator('input');
  await eventsInput.fill('application.created', { timeout: 3_000 });
  await eventsInput.press('Enter');
  await eventsInput.fill('execution.completed', { timeout: 3_000 });
  await eventsInput.press('Enter');

  const created = page.waitForResponse(
    (res) => res.request().method() === 'POST' && res.url().includes('/webhooks/prompt_lib/'),
    { timeout: 20_000 },
  );
  await dialog.getByRole('button', { name: 'Create', exact: true }).click({ timeout: 3_000 });
  const createWrite = await created;
  expect(createWrite.status(), await createWrite.text()).toBeLessThan(300);
  const createdWebhook = (await createWrite.json()) as WebhookWire;
  createdHere.add(createdWebhook.id);

  // The reveal-once secret dialog shows the exact value the POST carried.
  const secretDialog = page.getByTestId('webhook-secret-dialog');
  await expect(secretDialog).toBeVisible({ timeout: 5_000 });
  const revealedSecret = await page.getByTestId('webhook-secret-value').inputValue();
  expect(revealedSecret.length).toBeGreaterThan(0);
  expect(revealedSecret).toBe(createdWebhook.secret);
  await page.getByRole('button', { name: 'Done', exact: true }).click({ timeout: 3_000 });

  /* ── the row is real UI, not a stub ─────────────────────────────────── */
  const row = page.getByTestId(`webhook-row-${createdWebhook.id}`);
  await expect(row).toBeVisible({ timeout: 10_000 });
  await expect(row.getByText(destination)).toBeVisible();
  await expect(row.getByText('application.created')).toBeVisible();
  await expect(row.getByText('execution.completed')).toBeVisible();

  /* ── server agrees the webhook exists, with the fields the form sent ── */
  const afterCreate = await listWebhooks(page.request);
  const persisted = afterCreate.find((w) => w.id === createdWebhook.id);
  expect(persisted, `created webhook ${createdWebhook.id} not found in a fresh GET`).toBeDefined();
  expect(persisted!.url).toBe(destination);
  expect(persisted!.events).toEqual(['application.created', 'execution.completed']);
  expect(persisted!.active).toBe(true);
  expect(persisted!.secret).toBe(revealedSecret);

  /* ── rotate: the secret changes, everything else does not ───────────── */
  const rotated = page.waitForResponse(
    (res) => res.request().method() === 'PUT' && res.url().includes(`/webhooks/prompt_lib/${DEFAULT_PROJECT_ID}/${createdWebhook.id}`),
    { timeout: 20_000 },
  );
  await row.getByRole('button', { name: 'Rotate secret' }).click({ timeout: 5_000 });
  const rotateWrite = await rotated;
  expect(rotateWrite.status(), await rotateWrite.text()).toBeLessThan(300);

  await expect(page.getByTestId('webhook-secret-dialog')).toBeVisible({ timeout: 5_000 });
  const rotatedSecret = await page.getByTestId('webhook-secret-value').inputValue();
  expect(rotatedSecret).not.toBe(revealedSecret);
  await page.getByRole('button', { name: 'Done', exact: true }).click({ timeout: 3_000 });

  const afterRotate = await listWebhooks(page.request);
  const rotatedRow = afterRotate.find((w) => w.id === createdWebhook.id);
  expect(rotatedRow!.secret).toBe(rotatedSecret);
  expect(rotatedRow!.url).toBe(destination);
  expect(rotatedRow!.events).toEqual(['application.created', 'execution.completed']);

  /* ── disable: `active` flips server-side ─────────────────────────────── */
  const disabled = page.waitForResponse(
    (res) => res.request().method() === 'PUT' && res.url().includes(`/webhooks/prompt_lib/${DEFAULT_PROJECT_ID}/${createdWebhook.id}`),
    { timeout: 20_000 },
  );
  await row.getByRole('switch', { name: 'Toggle active' }).click({ timeout: 5_000 });
  const disableWrite = await disabled;
  expect(disableWrite.status(), await disableWrite.text()).toBeLessThan(300);

  const afterDisable = await listWebhooks(page.request);
  expect(afterDisable.find((w) => w.id === createdWebhook.id)!.active).toBe(false);

  /* ── delete: the row is gone from a fresh GET ─────────────────────────── */
  const deleted = page.waitForResponse(
    (res) => res.request().method() === 'DELETE' && res.url().includes(`/webhooks/prompt_lib/${DEFAULT_PROJECT_ID}/${createdWebhook.id}`),
    { timeout: 20_000 },
  );
  await row.getByRole('button', { name: 'Delete' }).click({ timeout: 5_000 });
  await page.getByRole('button', { name: 'Delete', exact: true }).click({ timeout: 5_000 }); // confirm dialog
  const deleteWrite = await deleted;
  expect(deleteWrite.status(), await deleteWrite.text()).toBeLessThan(300);
  createdHere.delete(createdWebhook.id);

  const afterDelete = await listWebhooks(page.request);
  expect(afterDelete.find((w) => w.id === createdWebhook.id)).toBeUndefined();

  await checkA11y(page);
});
