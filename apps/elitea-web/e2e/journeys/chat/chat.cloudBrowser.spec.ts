/**
 * The streamed cloud browser pane (#1598, ADR-0049 decision 6) against a
 * FIXTURE stream.
 *
 * WHY A FIXTURE. A real view needs a cloud run that used the browser
 * toolkit, a browser supervisor and a browser session pod: none is in the
 * E2E stack (the session image and the supervisor's browser profile run
 * only on a cluster, `deployment/cloud-browser-kubernetes`). So this journey
 * creates a real conversation, serves its message list with one answer whose
 * run called `browser_open` (still streaming, so the row names its run in
 * `task_id`, as the real list does), and answers the view WebSocket with
 * `page.routeWebSocket` in the contract's shape: `hello`, a `frame` header,
 * its JPEG as one binary message, and the up messages it receives.
 *
 * What it proves end to end in a real browser: the tab appears for that
 * run, the socket goes to the run's own session name under `/api/v2`, the
 * frame is drawn and acked, take over and hand back (with a note) reach the
 * socket in the contract's shape, and a watch-only viewer has no controls.
 */
import { test, expect, type Page, type WebSocketRoute } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { openSidePanelTab } from '../../fixtures/sidePanel';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteConversation } from '../../fixtures/api';

const EXECUTION_ID = 'e2e0cloudbrowser0000000000000001';
/** `elitea-br-` + sha256("elitea.browser-session\0" + EXECUTION_ID)[:24], as Main and the worker derive it. */
const SESSION_PATTERN = /\/api\/v2\/browser_sessions\/elitea-br-[0-9a-f]{24}\/view$/;

/** A 1×1 JPEG. */
const JPEG = Buffer.from(
  '/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/yQALCAABAAEBAREA/8wABgAQEAX/2gAIAQEAAD8A0s8g/9k=',
  'base64',
);

function messageRows(conversationId: string) {
  return {
    items: [
      { id: '9001', uid: '00000000-0000-4000-8000-000000000001', role: 'user', content: 'Open the shop', conversation_id: conversationId, created_at: '2026-11-06T10:00:00Z' },
      {
        id: '9002', uid: '00000000-0000-4000-8000-000000000002', role: 'assistant', content: 'Opening the shop…', conversation_id: conversationId,
        created_at: '2026-11-06T10:00:01Z', is_streaming: true, task_id: EXECUTION_ID,
        metadata: { tool_calls: { c1: { tool_name: 'browser_open', toolkit_name: 'Cloud Browser', toolkit_type: 'browser', tool_run_id: 'c1', timestamp_start: '2026-11-06 10:00:01', finish_reason: 'stop' } } },
      },
    ],
    total: 2, page: 0, page_size: 50, total_pages: 1,
  };
}

async function serveTranscript(page: Page, conversationId: string): Promise<void> {
  await page.route(new RegExp(`/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}\\b`), (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(messageRows(conversationId)) }));
  await page.route(/\/elitea_core\/message_traces\/prompt_lib\//, (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ rows: [], total: 0 }) }));
  // The streaming row would reattach to its run's events: nothing streams here.
  await page.route(/\/api\/v2\/executions\/.*\/events/, (route) => route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }));
}

/** The fixture view: `hello`, then one frame per ack, recording what the viewer sent. */
function fixtureView(canControl: boolean, received: Record<string, unknown>[], urls: string[]) {
  return (ws: WebSocketRoute) => {
    urls.push(ws.url());
    let control = 'agent';
    const frame = (seq: number) => {
      ws.send(JSON.stringify({ type: 'frame', seq, width: 1280, height: 800, bytes: JPEG.length, url: 'https://shop.example/cart', title: 'Cart', control }));
      ws.send(JPEG);
    };
    ws.onMessage((message) => {
      const value = JSON.parse(String(message)) as Record<string, unknown>;
      received.push(value);
      if (value['type'] === 'take_over') control = 'user';
      if (value['type'] === 'hand_back') control = 'agent';
      if (value['type'] === 'take_over' || value['type'] === 'hand_back') frame(100 + received.length);
    });
    ws.send(JSON.stringify({ type: 'hello', session_id: 'fixture', can_control: canControl, lingering: false }));
    frame(1);
  };
}

test.describe('cloud browser pane (fixture stream)', () => {
  let conversationId = '';

  test.beforeEach(async ({ request }) => {
    conversationId = await createConversation(request, `${AUTOTEST_PREFIX}cloud-browser-${Date.now()}`);
  });

  test.afterEach(async ({ request }) => {
    if (conversationId !== '') await deleteConversation(request, conversationId);
  });

  test('watch, take over and hand back with a note', async ({ page }) => {
    const received: Record<string, unknown>[] = [];
    const urls: string[] = [];
    await page.routeWebSocket(SESSION_PATTERN, fixtureView(true, received, urls));
    await serveTranscript(page, conversationId);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await openSidePanelTab(page, 'cloud-browser');
    const pane = page.getByTestId('cloud-browser');
    await expect(pane).toHaveAttribute('data-phase', 'live', { timeout: 20_000 });
    expect(urls[0], 'the view is the run\'s own session under the API base').toMatch(SESSION_PATTERN);
    await expect(page.getByTestId('cloud-browser-address')).toHaveText('https://shop.example/cart');
    await expect(page.getByTestId('cloud-browser-pill')).toHaveText('Agent');
    await expect.poll(() => received.some((m) => m['type'] === 'ack' && m['seq'] === 1), { message: 'the drawn frame is acked' }).toBe(true);

    await page.getByTestId('cloud-browser-take-over').click();
    await expect(page.getByTestId('cloud-browser-pill')).toHaveText('You');
    await page.getByTestId('cloud-browser-canvas').click({ position: { x: 10, y: 10 } });
    expect(received.some((m) => m['type'] === 'pointer' && m['kind'] === 'down')).toBe(true);

    await page.getByTestId('cloud-browser-hand-back').click();
    await page.getByTestId('cloud-browser-note').fill('signed in, continue');
    await page.getByTestId('cloud-browser-hand-back-send').click();
    await expect(page.getByTestId('cloud-browser-pill')).toHaveText('Agent');
    expect(received).toContainEqual({ type: 'hand_back', note: 'signed in, continue' });
  });

  test('a viewer who may only watch gets no controls', async ({ page }) => {
    const received: Record<string, unknown>[] = [];
    await page.routeWebSocket(SESSION_PATTERN, fixtureView(false, received, []));
    await serveTranscript(page, conversationId);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await openSidePanelTab(page, 'cloud-browser');
    await expect(page.getByTestId('cloud-browser-status')).toContainText('Watching only', { timeout: 20_000 });
    await expect(page.getByTestId('cloud-browser-take-over')).toHaveCount(0);
    await page.getByTestId('cloud-browser-canvas').click({ position: { x: 10, y: 10 } });
    expect(received.every((m) => m['type'] === 'ack')).toBe(true);
  });
});
