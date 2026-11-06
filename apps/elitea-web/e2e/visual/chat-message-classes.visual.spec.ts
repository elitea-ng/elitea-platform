/**
 * Snapshot coverage for an OPEN CONVERSATION — `/chat/:conversationId` — with
 * one row of every message class ADR-0050 decision 7e names (issue #1606):
 * a person's question, an agent answering people (persona name, role and the
 * agent's own icon as the still), agent-to-agent talk, a temporary sub-agent
 * (dashed ring and the "temporary" marker), a heartbeat ("woke up") and a
 * platform line.
 *
 * ── WHY THIS FILE SERVES THE CONVERSATION'S OWN READS ──────────────────────
 *
 * The route was EXEMPT in `scripts/check-visual-coverage.mjs` because seeding
 * a conversation per run grows the shared tenant's conversation list, so two
 * runs never rendered the same screen. The list now lives in the sidebar
 * (`shell-chats`), which `volatileRegions()` masks, so only the TRANSCRIPT
 * still needed a fixed seed — and a real seed cannot produce these rows at
 * all: `author_kind`, `visibility` and `cause` are written by the turn engine,
 * not by any public write a test could make (client contract 1.38).
 *
 * So, as `pipeline-editor.visual.spec.ts` does for the same reason, this file
 * creates NO server state. It fulfils the three GETs the page builds the
 * transcript from — the conversation detail, its message page and that page's
 * trace summaries — for one conversation id no tenant holds, and lets every
 * other request reach the real stack.
 *
 *   - PINNED: the real composition root — the app shell, `/chat/$conversationId`
 *     routing, `useChatPageData`'s row adapter, `convertMessagesToChatHistory`,
 *     `answerAuthor`, `ChatMessageList`, `ApplicationAnswer` and the
 *     `AssistantCaption` / `AgentAvatar` pair for every class.
 *   - NOT PINNED: that elitea-main writes these fields. That is the Go
 *     handler and conformance suites' job (#1605).
 *
 * The motion pack is not in the shot: a persisted row has no live
 * `agent_state`, so every avatar is the still — which is also what reduced
 * motion shows. The motions are asserted in `AgentAvatar.test.tsx`.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { keepThemeOffTheServer } from '../fixtures/serverTheme';
import { SNAPSHOT_TOLERANCE, settle, shellSettled, volatileRegions } from './lib/settle';

/** No tenant holds this id; the three reads below are served for it. */
const CONVERSATION_ID = '990101';
const CONVERSATION_NAME = 'Visual baseline: message classes';

const ICON_PATH = '/visual-fixtures/mira-avatar.png';
/** A 1×1 PNG in the brand accent family, scaled into the avatar. */
const ICON_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==',
  'base64',
);

const routeFor = (resource: string): RegExp =>
  new RegExp(`/api/v2/elitea_core/${resource}/prompt_lib/[^/?#]+/${CONVERSATION_ID}(?:[/?#]|$)`);

const PARTICIPANTS = [
  { id: '1', entity_name: 'user', kind: 'human', entity_meta: { id: 1 }, meta: { user_name: 'Visual Reader' } },
  {
    id: '2',
    entity_name: 'application',
    kind: 'agent',
    entity_meta: { id: 41, name: 'release-bot' },
    entity_settings: { icon_meta: { url: ICON_PATH, name: 'mira-avatar.png' } },
    persona: { display_name: 'Mira', role: 'Release manager' },
  },
  { id: '3', entity_name: 'application', kind: 'agent', entity_meta: { id: 42, name: 'CI Doctor' } },
  { id: '4', entity_name: 'application', kind: 'ephemeral_agent', entity_meta: { id: 43, name: 'CI Sleuth' } },
  { id: '5', entity_name: 'system', kind: 'system', entity_meta: {}, meta: { name: 'Elitea' } },
];

interface Row {
  readonly id: number;
  readonly role: 'user' | 'assistant';
  readonly author: number;
  readonly kind: string;
  readonly content: string;
  readonly addressedTo?: readonly number[];
  readonly visibility?: string;
  readonly cause?: string;
}

/** One row per class, in transcript order. */
const ROWS: readonly Row[] = [
  { id: 1, role: 'user', author: 1, kind: 'human', content: 'Is the 1.62 release ready to tag?', cause: 'human_message' },
  { id: 2, role: 'assistant', author: 2, kind: 'agent', content: 'Almost. I asked CI Doctor about the one red job.', addressedTo: [1] },
  { id: 3, role: 'assistant', author: 2, kind: 'agent', content: 'CI Doctor, why is ci-web-e2e red on main?', addressedTo: [3], cause: 'delegation' },
  { id: 4, role: 'assistant', author: 4, kind: 'ephemeral_agent', content: 'A flaky wait in the upload journey; the fix is in review.', cause: 'delegation' },
  { id: 5, role: 'assistant', author: 2, kind: 'agent', content: 'The fix merged overnight. The release is ready to tag.', cause: 'heartbeat' },
  { id: 6, role: 'assistant', author: 5, kind: 'system', content: 'Mira handed the floor back to you.', cause: 'system' },
];

function messageRows(): Record<string, unknown>[] {
  return ROWS.map((row) => ({
    id: row.id,
    uid: `visual-message-${row.id}`,
    conversation_id: Number(CONVERSATION_ID),
    role: row.role,
    content: row.content,
    created_at: `2026-11-06T09:0${row.id}:00Z`,
    author_participant_id: row.author,
    author_kind: row.kind,
    addressed_to: row.addressedTo ?? [],
    visibility: 'all',
    ...(row.cause !== undefined ? { cause: row.cause } : {}),
    ...(row.role === 'assistant' ? { reply_to_id: 1, question_id: 1 } : {}),
  }));
}

async function serveConversation(page: Page): Promise<void> {
  const json = (body: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
  const onlyGet = (body: () => unknown) => async (route: Parameters<Parameters<Page['route']>[1]>[0]) => {
    if (route.request().method() !== 'GET') return route.fallback();
    return route.fulfill(json(body()));
  };
  await page.route(
    routeFor('conversation'),
    onlyGet(() => ({ id: Number(CONVERSATION_ID), name: CONVERSATION_NAME, is_private: true, participants: PARTICIPANTS, meta: {} })),
  );
  await page.route(
    routeFor('messages'),
    onlyGet(() => ({ items: messageRows(), total: ROWS.length, page: 0, page_size: 50, total_pages: 1 })),
  );
  await page.route(routeFor('message_traces'), onlyGet(() => ({ rows: [] })));
  await page.route(`**${ICON_PATH}`, (route) => route.fulfill({ status: 200, contentType: 'image/png', body: ICON_PNG }));
}

async function openConversation(page: Page): Promise<void> {
  await serveConversation(page);
  await page.goto(`${BASE_URL}/app/chat/${CONVERSATION_ID}`, { waitUntil: 'domcontentloaded' });
  await shellSettled(page);
  // Landmarks that exist only once the served transcript rendered: the
  // persona role is read off participant 2, the markers off rows 4 and 5.
  await expect(page.getByTestId('chat-message-author-role').first()).toHaveText('Release manager', { timeout: 20_000 });
  const markers = page.getByTestId('chat-message-author-marker');
  await expect(markers.filter({ hasText: 'temporary' })).toHaveCount(1);
  await expect(markers.filter({ hasText: 'woke up' })).toHaveCount(1);
  await expect(page.getByText(ROWS[ROWS.length - 1]?.content ?? '')).toBeVisible();
  // Every avatar is the still: a persisted row carries no live agent_state.
  await expect(page.locator('[data-testid="chat-message-avatar"]:not([data-motion="still"])')).toHaveCount(0);
  await expect(page.getByTestId('agent-avatar-motion')).toHaveCount(0);
}

test('@visual chat-message-classes', async ({ page }) => {
  // @covers /chat/:conversationId
  await openConversation(page);
  await settle(page);

  await expect(page).toHaveScreenshot('chat-message-classes.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/**
 * The light scheme, switched through the app's real control for the reason
 * `routes.visual.spec.ts` gives (issue #61). Duplicated rather than imported:
 * importing a `*.spec.ts` re-registers its tests.
 */
test('@visual chat-message-classes-light', async ({ page }) => {
  await keepThemeOffTheServer(page);
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  const light = page.getByRole('button', { name: 'Light', exact: true });
  await expect(light).toBeVisible({ timeout: 20_000 });
  await light.click();
  await expect(light).toHaveAttribute('aria-pressed', 'true');
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-el-scheme')), { timeout: 10_000 })
    .toBe('light');

  await openConversation(page);
  expect(await page.evaluate(() => document.documentElement.getAttribute('data-el-scheme'))).toBe('light');
  await settle(page);

  await expect(page).toHaveScreenshot('chat-message-classes-light.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});
