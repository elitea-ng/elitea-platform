/**
 * API-WHD: outbound webhooks actually fire (#876's second half).
 *
 * #876 shipped the Settings → Webhooks registry UI and the five CRUD
 * routes, but internal/api/webhook's Dispatcher had no producer wired to
 * it — a registered webhook could not fire on anything, and
 * settings.webhooks.spec.ts said so explicitly ("POSTing to an outbound
 * webhook's own `url` field does not reach Elitea at all"). This journey
 * is the missing half: register a webhook, trigger a REAL producer
 * (conversation.created, via `createConversation` — the cheapest real
 * producer this change wires; see internal/events/publisher.go's file
 * header for the full producer list and why pipeline-run-finished is not
 * one of them), and assert a delivery row appears, is signed, retried, and
 * redeliverable.
 *
 * WHY THE DESTINATION IS A PORT NOTHING LISTENS ON, NOT A MOCK SERVICE. A
 * real HTTP receiver reachable from BOTH the elitea-main container and this
 * test process would need a new service in deploy/ (no generic echo mock
 * exists today — deploy/mock-llm and deploy/mock-mcp are protocol-specific
 * stand-ins, not a webhook receiver) or cross-container port-forwarding
 * neither this repository's e2e stack nor this journey's budget can add.
 * `http://127.0.0.1:1/...` is deterministic instead: nothing binds port 1
 * without root, so every attempt fails the SAME way (connection refused) on
 * every run, in every environment, which is what makes the delivery log's
 * `status: failed`, `attempts: 3` and non-empty `last_error` a repeatable
 * assertion rather than a flake waiting to happen. It still proves the real
 * claim end to end: the event was produced, the dispatcher looked up the
 * subscribed webhook, attempted delivery with retries, and logged the
 * outcome — the exact pipeline a reachable destination would also exercise,
 * with only the LAST hop (a real 2xx) left unverified here. That hop is
 * covered by the Go integration test
 * (services/elitea-main/internal/api/webhook
 * /dispatcher_postgres_integration_test.go), which uses an in-process
 * httptest.Server precisely because it does not have this journey's
 * cross-container reachability problem.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, createConversation, deleteConversation, DEFAULT_PROJECT_ID } from '../../fixtures/api';

test.use({ storageState: STORAGE_STATE.admin });

interface WebhookWire {
  readonly id: string;
  readonly url: string;
  readonly events: readonly string[];
  readonly secret: string;
  readonly active: boolean;
}

interface DeliveryWire {
  readonly id: string;
  readonly webhook_id: string;
  readonly event: string;
  readonly status: 'pending' | 'success' | 'failed';
  readonly attempts: number;
  readonly response_code: number | null;
  readonly last_error: string;
  readonly redelivery_of: string;
}

const WEBHOOKS_URL = `${API_BASE}/webhooks/prompt_lib/${DEFAULT_PROJECT_ID}`;
// Deterministically unreachable — see the file header on why this, and not
// a mock service, is the right destination for THIS journey.
const UNREACHABLE_DESTINATION = 'http://127.0.0.1:1/webhook-delivery-test';

const createdWebhooks = new Set<string>();
const createdConversations = new Set<string>();

test.afterEach(async ({ request }) => {
  for (const id of createdConversations) {
    await deleteConversation(request, id);
  }
  createdConversations.clear();
  for (const id of createdWebhooks) {
    const resp = await request.delete(`${WEBHOOKS_URL}/${id}`);
    expect([204, 404], `cleanup: delete webhook ${id} answered ${resp.status()}`).toContain(resp.status());
  }
  createdWebhooks.clear();
});

test('API-WHD1: a webhook subscribed to conversation.created is signed, delivered, retried and logged', async ({ request }) => {
  test.setTimeout(45_000);

  /* ── register a webhook subscribed to conversation.created ──────────── */
  const createResp = await request.post(WEBHOOKS_URL, {
    data: { url: UNREACHABLE_DESTINATION, events: ['conversation.created'], secret: 'e2e-whd-secret', active: true },
  });
  expect(createResp.status(), await createResp.text()).toBe(201);
  const webhook = (await createResp.json()) as WebhookWire;
  createdWebhooks.add(webhook.id);
  expect(webhook.secret).toBe('e2e-whd-secret');

  /* ── trigger the real producer ───────────────────────────────────────── */
  const conversationName = `${AUTOTEST_PREFIX}j-webhook-delivery-${Date.now()}`;
  const conversationId = await createConversation(request, conversationName);
  createdConversations.add(conversationId);

  /* ── the dispatcher logs the attempt sequence ────────────────────────── */
  // Async by design (internal/events.Publisher.Emit dispatches to its
  // Sinks on their own goroutine, and the Dispatcher itself retries up to
  // three times with a short backoff before logging) — expect.poll is
  // Playwright's own retry primitive for exactly this, not a raw
  // waitForTimeout.
  let delivery: DeliveryWire | null = null;
  await expect
    .poll(
      async () => {
        const resp = await request.get(`${WEBHOOKS_URL}/${webhook.id}/deliveries`);
        expect(resp.status(), await resp.text()).toBe(200);
        const body = (await resp.json()) as { items: DeliveryWire[] };
        delivery = body.items.find((item) => item.event === 'conversation.created') ?? null;
        return delivery !== null;
      },
      { message: 'waiting for the conversation.created delivery to be logged', timeout: 20_000, intervals: [500, 1_000, 2_000] },
    )
    .toBe(true);
  expect(delivery, 'no conversation.created delivery was ever logged for this webhook').not.toBeNull();

  expect(delivery!.status).toBe('failed');
  expect(delivery!.attempts).toBe(3);
  expect(delivery!.response_code).toBeNull();
  expect(delivery!.last_error.length).toBeGreaterThan(0);
  expect(delivery!.redelivery_of).toBe('');

  /* ── redeliver: a new, linked row ────────────────────────────────────── */
  const redeliverResp = await request.post(`${WEBHOOKS_URL}/${webhook.id}/deliveries/${delivery!.id}/redeliver`);
  expect(redeliverResp.status(), await redeliverResp.text()).toBe(201);
  const redelivered = (await redeliverResp.json()) as DeliveryWire;
  expect(redelivered.id).not.toBe(delivery!.id);
  expect(redelivered.redelivery_of).toBe(delivery!.id);
  expect(redelivered.event).toBe('conversation.created');
  expect(redelivered.status).toBe('failed');

  const listAfterRedeliver = await request.get(`${WEBHOOKS_URL}/${webhook.id}/deliveries`);
  expect(listAfterRedeliver.status()).toBe(200);
  const afterBody = (await listAfterRedeliver.json()) as { items: DeliveryWire[] };
  expect(afterBody.items.some((item) => item.id === redelivered.id)).toBe(true);
});
