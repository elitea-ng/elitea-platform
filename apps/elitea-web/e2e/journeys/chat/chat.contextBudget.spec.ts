/**
 * The chat rail's Context Budget panel reports the budget the READER configured,
 * not a constant.
 *
 * Ported by use case from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/chat/test_context_management.py`):
 *
 *  - `TestContextManagementSettings::test_context_budget_reflects_profile_max_tokens[10k_tokens]`
 *  - `TestContextManagementSettings::test_context_budget_reflects_profile_max_tokens[32k_tokens]`
 *
 * The two legacy cases are one journey here. They are not two use cases — the
 * legacy suite parameterised them, and its own docstring says why the second
 * value exists: "Updated value propagates (not cached)". A second value only
 * means anything if the FIRST one was on screen first, so this journey sets
 * both, in order, against one conversation, and the "not cached" property is
 * then a real assertion rather than a second run of the first one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE PROFILE IS WRITTEN OVER THE API AND NOT THROUGH SETTINGS › MEMORY
 * ─────────────────────────────────────────────────────────────────────────────
 * This is a CHAT journey: the property under test is the resolution chain
 * `conversation strategy > the user's defaults > the constants`
 * (`contextsettings.Resolve`), read by
 * `GET /elitea_core/context_analytics/prompt_lib/{p}/{c}` and rendered by
 * `widgets/context-budget`. The Settings › Memory form that writes those
 * defaults is a different surface with its own owner, and driving it here would
 * have added a branch this journey cannot make deterministic — the context
 * toggle's state on arrival is whatever the last run left, and a test that
 * says "turn it on if it is off" has two bodies and proves neither.
 * `PUT /social/author` is the endpoint that form itself submits, so the write
 * below is the same write, without the branch.
 *
 * THE WRITE IS ACCOUNT-WIDE, AND IS RESTORED. `default_context_management` hangs
 * off the persona, not off a conversation, so while this journey runs, other
 * workers signed in as the same persona resolve the same budget. Nothing else
 * in the suite reads it (no other journey mentions `context_analytics`,
 * `context-budget-*` or `default_context_management`), and the original value —
 * including "the user had never saved one" — is put back in a `finally`.
 *
 * THE CARRY-FORWARD IS NOT OPTIONAL. `UpdateAuthor` upserts `title`,
 * `description`, `avatar` and `personalization` from the body outright, and only
 * the two memory blocks are COALESCEd against the stored row. A body carrying
 * just the context block would therefore blank this persona's display name and
 * avatar for every other journey. Everything is read back and re-sent, exactly
 * as `settingsProfileForm.ts`'s own `buildAuthorUpdate` does.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-ctx';

/** The two budgets the legacy suite parameterised over. Both are above `MinMaxContextTokens` (1000). */
const FIRST_BUDGET = 10_000;
const SECOND_BUDGET = 32_000;

/** The author record, in the subset `UpdateAuthor` replaces outright plus the block under test. */
interface AuthorRecord {
  readonly name?: string;
  readonly description?: string;
  readonly avatar?: string;
  readonly personalization?: unknown;
  readonly default_context_management?: Record<string, unknown>;
}

async function readAuthor(request: APIRequestContext): Promise<AuthorRecord> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status(), 'the persona must be readable, or nothing below means anything').toBe(200);
  return (await response.json()) as AuthorRecord;
}

/**
 * Writes `contextManagement` onto the author record, carrying every
 * replace-outright field forward.
 *
 * `undefined` restores the "never saved" state by sending an EMPTY block, not by
 * omitting one. Omitting it would keep whatever this journey last wrote: the
 * handler COALESCEs an absent block against the STORED row, which is what lets
 * Settings › AI Personality save without carrying Memory's fields — and what
 * would make an omission here a permanent 32 000-token leak on the shared
 * persona. `{}` survives `withoutClearedFields` (no null/empty members to
 * strip), decodes to a block with every field unset, and therefore resolves to
 * the contract's own constants, which is exactly what "never saved" resolves to.
 */
async function writeContextDefaults(
  request: APIRequestContext,
  author: AuthorRecord,
  contextManagement: Record<string, unknown> | undefined,
): Promise<void> {
  const response = await request.put(`${API_BASE}/social/author`, {
    data: {
      name: author.name ?? '',
      description: author.description ?? '',
      avatar: author.avatar ?? '',
      personalization: author.personalization ?? {},
      default_context_management: contextManagement ?? {},
    },
  });
  expect(response.status(), `the profile write must be accepted: ${(await response.text()).slice(0, 200)}`).toBe(200);
}

/** `formatNumberWithSpaces` groups thousands; the separator itself is normalised away below. */
function grouped(value: number): string {
  return String(value).replace(/\B(?=(\d{3})+(?!\d))/g, ' ');
}

/**
 * The panel's token line, with every run of whitespace collapsed to one space.
 *
 * The product groups digits with a NON-BREAKING space, and this reads the
 * rendered text rather than reproducing that character: an assertion that
 * hardcoded U+00A0 would fail the day the separator changed for a reason that
 * has nothing to do with the budget being reported.
 */
async function tokensLine(page: Page): Promise<string> {
  const text = await page.getByTestId('context-budget-tokens').textContent();
  return (text ?? '').replace(/\s+/g, ' ').trim();
}

test('the Context Budget panel reports the budget the profile sets, and follows it when it changes', async ({ page }) => {
  const author = await readAuthor(page.request);
  const original = author.default_context_management;

  const conversationId = await createConversation(
    page.request,
    `${AUTOTEST_PREFIX}budget${SUFFIX}-${Date.now()}`,
  );

  try {
    await writeContextDefaults(page.request, author, {
      ...original,
      enabled: true,
      max_context_tokens: FIRST_BUDGET,
    });

    // The server resolves the budget from those defaults for a conversation
    // that has never been configured — asserted before the browser is involved,
    // so a failure here says "the resolution chain" and not "the panel".
    const status = await page.request.get(
      `${API_BASE}/elitea_core/context_analytics/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    );
    expect(status.status()).toBe(200);
    expect((await status.json()) as { max_tokens?: number }).toMatchObject({ max_tokens: FIRST_BUDGET });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    // The panel lives at the foot of the participants rail, which `ChatPage`
    // mounts collapsed — collapsed it renders only a percentage, never the
    // budget (`ContextBudgetCollapsed`).
    const expand = page.getByRole('button', { name: 'Expand participants' });
    await expect(expand).toBeVisible({ timeout: 20_000 });
    await expand.click();
    await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 10_000 });

    const panel = page.getByTestId('context-budget-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });
    await expect
      .poll(async () => tokensLine(page), {
        timeout: 20_000,
        message: 'the panel must report the profile budget, not a constant',
      })
      .toContain(`/ ${grouped(FIRST_BUDGET)} tokens`);

    // `-` is what `formatTokensDisplay` prints for a budget of zero, i.e. for a
    // panel that resolved no strategy at all. It must not be what is on screen.
    expect(await tokensLine(page)).not.toContain('/ - tokens');

    await checkA11y(page);

    // ── the second value: the panel follows a change, it does not cache one ──
    await writeContextDefaults(page.request, author, {
      ...original,
      enabled: true,
      max_context_tokens: SECOND_BUDGET,
    });
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await page.getByRole('button', { name: 'Expand participants' }).click();
    await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 10_000 });

    await expect
      .poll(async () => tokensLine(page), {
        timeout: 20_000,
        message: 'a changed profile budget must reach the panel — this is the legacy suite’s "not cached" case',
      })
      .toContain(`/ ${grouped(SECOND_BUDGET)} tokens`);
    expect(await tokensLine(page)).not.toContain(`/ ${grouped(FIRST_BUDGET)} tokens`);
  } finally {
    // Restored whatever happened above, including back to "never saved".
    await writeContextDefaults(page.request, author, original);
    await deleteConversation(page.request, conversationId);
  }
});
