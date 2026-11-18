/**
 * A refused or narrowed delegation shows in the chat (ADR-0044 Amendment 1,
 * A1.2 rule 5; delegation epic 2156 story S5) against a FIXTURE transcript and
 * a FIXTURE stream.
 *
 * WHY A FIXTURE. The refusal needs a run whose agent calls a specialist the
 * run may not use; the offline mock model decides nothing, and the enforcing
 * switch is off by default until the migration of existing compositions (S6).
 * So this journey creates a real conversation and serves its message list with
 * one answer, and the answer's run events with the frames the Rust worker
 * emits (`testdata/client-frames/rust.json`), in the shape Main serves them
 * (`can_review` and the people to ask, stamped per viewer).
 *
 * What it proves in a real browser: the notice sits in the answer, "Agent1
 * could not use git from Agent2. This delegation is not approved.", an editor
 * gets Review delegation pointing at the calling agent, a viewer who may not
 * review is told who to ask, a refusal the person's own access caused offers no
 * action, and a stored answer and a live stream draw the same notice.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteConversation } from '../../fixtures/api';

const USER_ID = '00000000-0000-4000-8000-000000002161';
const ANSWER_ID = '00000000-0000-4000-8000-000000002162';
const EXECUTION_ID = 'e2e0delegationnotice00000000001';

const edge = { caller_id: 2, caller_name: 'Agent2', callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' };

const noGrant = (extra: Record<string, unknown>) => ({
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  code: 'DELEGATION_GRANT_MISSING',
  edge,
  missing: [{ kind: 'toolkit', name: 'git' }],
  ...extra,
});
const principal = {
  id: 'call-2',
  kind: 'refused',
  reason: 'principal',
  code: 'DELEGATION_GRANT_MISSING',
  edge: { ...edge, callee_id: 3, callee_name: 'Agent3' },
  missing: [{ kind: 'toolkit', name: 'payroll' }],
  can_review: false,
};

function transcript(conversationId: string, notices: unknown[], streaming: boolean) {
  return {
    items: [
      { id: '9101', uid: USER_ID, role: 'user', content: 'Commit the change', conversation_id: conversationId, created_at: '2026-11-17T10:00:00Z' },
      {
        id: '9102',
        uid: ANSWER_ID,
        role: 'assistant',
        content: streaming ? '' : 'I could not ask Agent1 to commit.',
        conversation_id: conversationId,
        created_at: '2026-11-17T10:00:01Z',
        ...(streaming ? { is_streaming: true, task_id: EXECUTION_ID } : {}),
        metadata: streaming ? {} : { delegation_notices_v1: notices },
      },
    ],
    total: 2,
    page: 0,
    page_size: 50,
    total_pages: 1,
  };
}

async function serve(page: Page, conversationId: string, notices: unknown[], streaming: boolean, events = ''): Promise<void> {
  await page.route(new RegExp(`/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}\\b`), (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(transcript(conversationId, notices, streaming)) }),
  );
  await page.route(/\/elitea_core\/message_traces\/prompt_lib\//, (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ rows: [], total: 0 }) }),
  );
  await page.route(/\/api\/v2\/executions\/.*\/events/, (route) => route.fulfill({ status: 200, contentType: 'text/event-stream', body: events }));
}

function sse(conversationId: string, frames: { type: string; notice: unknown }[]): string {
  return frames
    .map(
      ({ type, notice }, index) =>
        `id: ${String(index + 1)}\nevent: execution.node_event\ndata: ${JSON.stringify({
          type,
          stream_id: conversationId,
          message_id: ANSWER_ID,
          content: null,
          sio_event: 'chat_predict',
          response_metadata: { delegation_v1: notice },
        })}\n\n`,
    )
    .join('');
}

test.describe('delegation notice in the chat (fixture)', () => {
  let conversationId = '';

  test.beforeEach(async ({ request }) => {
    conversationId = await createConversation(request, `${AUTOTEST_PREFIX}delegation-notice-${Date.now()}`);
  });

  test.afterEach(async ({ request }) => {
    if (conversationId !== '') await deleteConversation(request, conversationId);
  });

  test('an editor of the caller sees the refusal with Review delegation', async ({ page }) => {
    await serve(page, conversationId, [noGrant({ can_review: true })], false);
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);

    const card = page.getByTestId('delegation-notice').first();
    await expect(card).toContainText('Agent1 could not use git from Agent2. This delegation is not approved.', { timeout: 20_000 });
    const review = card.getByRole('link', { name: 'Review delegation' });
    await expect(review).toBeVisible();
    await expect(review).toHaveAttribute('href', /\/agents\/all\/2\?review_delegation=1$/);
  });

  test('a viewer who may not review is told who to ask, and a principal refusal offers no action', async ({ page }) => {
    await serve(page, conversationId, [noGrant({ can_review: false, editors: ['Ada Lovelace'] }), principal], false);
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);

    await expect(page.getByTestId('delegation-notice')).toHaveCount(2, { timeout: 20_000 });
    await expect(page.getByTestId('delegation-editors')).toHaveText('Ask Ada Lovelace to review this delegation.');
    await expect(page.getByRole('link', { name: 'Review delegation' })).toHaveCount(0);
    await expect(page.getByTestId('delegation-notice').nth(1)).toContainText('Agent3 could not use payroll. You do not have access to it.');
  });

  test('a drifted grant reads as used without the new toolkit', async ({ page }) => {
    const narrowed = {
      id: 'call-3',
      kind: 'narrowed',
      reason: 'stale',
      edge,
      grant_id: 5,
      missing: [{ kind: 'toolkit', name: 'Database' }],
      can_review: true,
    };
    await serve(page, conversationId, [narrowed], false);
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('delegation-notice')).toContainText('Agent1 ran without Database. It was added after this delegation was approved.', {
      timeout: 20_000,
    });
  });

  test('the live stream draws the same notice as the stored answer', async ({ page }) => {
    const events = sse(conversationId, [{ type: 'delegation_refused', notice: noGrant({ can_review: true }) }]);
    await serve(page, conversationId, [], true, events);
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);

    const card = page.getByTestId('delegation-notice').first();
    await expect(card).toContainText('Agent1 could not use git from Agent2. This delegation is not approved.', { timeout: 20_000 });
    await expect(card.getByRole('link', { name: 'Review delegation' })).toBeVisible();
  });
});
