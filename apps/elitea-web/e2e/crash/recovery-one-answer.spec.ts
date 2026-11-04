/**
 * Crash recovery, browser half: whatever the harness breaks mid-turn, the user
 * ends up with ONE answer (or ONE failure reference), live and after a reload.
 *
 * Contract with scripts/crash-recovery/crashctl.py: JSON files in
 * CRASH_CONTROL_DIR — see ./crashControl.ts. No response mocks.
 */
import { mkdirSync, writeFileSync } from 'fs';
import path from 'path';

import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { controlDir, requireEnv, sha256, waitForControl, writeControl } from './crashControl';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

interface Released {
  expect: 'answer' | 'failure';
  answer_contains?: string[];
  error_code?: string;
  message_id?: string;
}

interface Outcome {
  ok: boolean;
  error?: string;
  markerCounts: number[];
  failureText: string;
}

async function checkOutcome(page: Page, released: Released): Promise<Outcome> {
  const outcome: Outcome = { ok: false, markerCounts: [], failureText: '' };
  const failure = page.getByTestId('failure-reference');
  try {
    if (released.expect === 'answer') {
      for (const marker of released.answer_contains ?? []) {
        await expect(page.getByText(marker), 'exactly one answer region carries each marker').toHaveCount(1, {
          timeout: 30_000,
        });
      }
      await expect(page.getByText(/^\.\.\.$/), 'no streaming placeholder is left behind').toHaveCount(0);
    } else {
      await expect(failure, 'exactly one failure reference').toHaveCount(1, { timeout: 30_000 });
      await expect(failure).toBeVisible();
      const text = (await failure.textContent()) ?? '';
      if (released.message_id !== undefined) expect(text).toContain(`Message ID: ${released.message_id}`);
      if (released.error_code !== undefined) expect(text).toContain(`Error code: ${released.error_code}`);
    }
    outcome.ok = true;
  } catch (error) {
    outcome.error = error instanceof Error ? error.message.split('\n')[0] : String(error);
  }
  for (const marker of released.answer_contains ?? []) outcome.markerCounts.push(await page.getByText(marker).count());
  if ((await failure.count()) > 0) outcome.failureText = (await failure.first().textContent()) ?? '';
  return outcome;
}

test('crash recovery: one answer, live and after reload', async ({ page }) => {
  const scenario = requireEnv('CRASH_SCENARIO');
  const base = requireEnv('PLAYWRIGHT_BASE_URL');
  const conversationId = requireEnv('CRASH_CONVERSATION_ID');
  const sendVia = process.env['CRASH_SEND_VIA'] ?? 'ui';
  const releaseTimeout = Number(process.env['CRASH_RELEASE_TIMEOUT_MS'] ?? '900000');

  await page.goto(`${base}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
  writeControl('browser-ready.json', { scenario, at_ms: Date.now() });

  if (sendVia === 'ui') {
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 15_000 });
    await input.fill(requireEnv('CRASH_PROMPT'));
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await page.getByTestId('chat-send-button').click();
    const response = await started;
    const request = (response.request().postDataJSON() ?? {}) as Record<string, unknown>;
    const reply = (await response.json().catch(() => ({}))) as Record<string, unknown>;
    writeControl('sent.json', {
      scenario,
      question_id: request['question_id'] ?? request['id'] ?? null,
      execution_id: reply['execution_id'] ?? null,
      response_message_id: reply['response_message_id'] ?? null,
      events_url: reply['events_url'] ?? null,
      status: response.status(),
    });
  } else {
    await waitForControl('sent.json', 120_000, page);
  }

  // Live view through the fault: the page stays open until the harness settles.
  const released = await waitForControl<Released>('released.json', releaseTimeout, page);

  const live = await checkOutcome(page, released);
  await page.reload();
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
  const reloaded = await checkOutcome(page, released);

  const outDir = path.join(controlDir(), 'browser');
  mkdirSync(outDir, { recursive: true });
  await page.screenshot({ path: path.join(outDir, 'after-reload.png'), fullPage: true });
  const main = page.locator('main');
  const domText = await ((await main.count()) > 0 ? main.first() : page.locator('body')).innerText();
  writeFileSync(
    path.join(outDir, 'result.json'),
    JSON.stringify({
      scenario,
      live_ok: live.ok,
      reload_ok: reloaded.ok,
      answer_marker_counts: { live: live.markerCounts, reload: reloaded.markerCounts },
      failure_reference_text_sha256: reloaded.failureText === '' ? null : sha256(reloaded.failureText),
      dom_text_sha256: sha256(domText),
      at_ms: Date.now(),
    }),
    'utf8',
  );

  expect(live.ok, `live view before reload: ${live.error ?? ''}`).toBe(true);
  expect(reloaded.ok, `after reload: ${reloaded.error ?? ''}`).toBe(true);
});
