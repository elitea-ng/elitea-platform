/**
 * DWIKI-012 — the wiki chat, through the facade to the provider's `ask`;
 * DWIKI-012b, the same round trip in research mode through `deep_research`;
 * DWIKI-016, a wiki page attached to the question as context;
 * DWIKI-017, the conversation surviving a reload and a different browser.
 *
 * DWIKI-012c, the answer arriving as it is written (#701).
 *
 * The SPI has TWO event channels now — progress, and the answer's own
 * fragments — so DWIKI-012c can assert what DWIKI-012's acceptance always
 * asked for and no transport could deliver. Both fixture runners stream the
 * answer they are about to return, paced like their progress steps, so the
 * journey watches a partial answer without a model.
 *
 * WHERE THESE RUN. `PROVIDER_BACKED_JOURNEYS` names this file, which puts it
 * in the `deepwiki-stack` project — but it does NOT take it out of the
 * ordinary ones: `chromium` and `webkit` ignore only the admission and
 * real-engine journeys, so both tests below also run against the E2E stack,
 * whose facade composes the same fixture runner. Anything asserted here must
 * therefore hold on both stacks; both currently answer from
 * `run/fixture.go`.
 */
import { expect, test, type Page } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { SEEDED, openDeepWiki } from './helpers';

/** Opens the drawer on the read-only wiki and returns it. */
async function openChatDrawer(page: Page) {
  await openDeepWiki(page, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
  await page.getByRole('button', { name: 'Ask about this repository' }).click();
  const drawer = page.getByTestId('wiki-chat-drawer');
  await expect(drawer).toBeVisible();
  return drawer;
}

/**
 * Pins which conversation a browser resumes, before the app boots.
 *
 * `addInitScript` and not `evaluate`: the drawer reads the key the first time
 * it renders, so a value written after navigation arrives too late and the
 * drawer has already minted one of its own.
 */
async function seedConversationKey(page: Page, key: string) {
  await page.addInitScript(
    ([project, toolkit, value]) => {
      localStorage.setItem(`el.deepwiki.chat.conversation.${project}.${toolkit}`, value);
    },
    [SEEDED.projectId, SEEDED.readOnly.toolkitId, key] as const,
  );
}

/** Asks one question and waits for the fixture's answer to land. */
async function ask(page: Page, question: string) {
  const drawer = page.getByTestId('wiki-chat-drawer');
  await drawer.getByPlaceholder('Ask about this repository').fill(question);
  await drawer.getByRole('button', { name: 'Send' }).click();
  await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText(
    `Fixture answer to: ${question}`,
    { timeout: 60_000 },
  );
}

test.describe('DeepWiki chat', () => {
  // A provider round trip through the facade, plus the fixture's paced steps:
  // longer than Playwright's 30s default, which killed a passing answer mid-poll.
  test.setTimeout(120_000);

  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-012: the wiki chat answers a question about the wiki, with its sources', async ({ page }) => {
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
    await page.getByRole('button', { name: 'Ask about this repository' }).click();
    const drawer = page.getByTestId('wiki-chat-drawer');
    await expect(drawer).toBeVisible();

    const question = 'Where do the wiki pages live?';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText(`Fixture answer to: ${question}`, {
      timeout: 60_000,
    });
    await expect(drawer.getByTestId('wiki-chat-messages')).toContainText('wiki_pages/overview/getting-started.md');
    await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
  });

  test('DWIKI-012b: research mode shows the plan the run is working through, and its report', async ({
    page,
  }) => {
    // The research panel renders ONLY when a run has a plan, and renders
    // nothing when it has none — which is right for `ask` and looks exactly
    // like a panel that was never wired up. That is why this journey exists
    // and why the fixture runner publishes a `todo_update` event
    // (run/fixture.go `ResearchTodos`): without one, no end-to-end test can
    // tell the two apart.
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
    await page.getByRole('button', { name: 'Ask about this repository' }).click();
    const drawer = page.getByTestId('wiki-chat-drawer');
    await expect(drawer).toBeVisible();

    // The mode picks the TOOL: `research` sends `deep_research`, and the
    // drawer polls that tool's own path. Asking in `ask` mode would answer
    // and produce no plan at all.
    await drawer.getByRole('button', { name: 'Research', exact: true }).click();

    const question = 'How is the wiki assembled?';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    const todos = drawer.getByTestId('wiki-chat-todos');
    await expect(todos).toBeVisible({ timeout: 60_000 });
    // The three fixture steps, and the status vocabulary the panel translates.
    await expect(todos).toContainText('Plan the research');
    await expect(todos).toContainText('Read the relevant pages');
    await expect(todos).toContainText('Write the report');
    await expect(todos).toContainText('Done');
    await expect(todos).toContainText('In progress');
    await expect(todos).toContainText('Pending');

    // The plan is not the answer: the report has to land as well, or a run
    // that published a plan and then died would read as a success.
    await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText('Research report (general)', {
      timeout: 60_000,
    });
    await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText(`Question: ${question}`);
    await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
  });

  test('DWIKI-012c: the answer appears as it is written, and the preview gives way to it', async ({
    page,
  }) => {
    // WHAT ONLY THIS CAN SEE. The reducer's accumulator and the preview it
    // feeds were both built and both tested before the transport could carry
    // a single token (#701): the browser was ready for frames nothing sent.
    // Only a run through the real provider can tell "streaming works" from
    // "streaming is implemented on one side".
    //
    // THIS JOURNEY OWNS ITS CONVERSATION, for DWIKI-017's reason: left to
    // itself the drawer ADOPTS the user's most recent stored conversation,
    // and CI runs with `E2E_REUSE_STACK=1`, so an earlier run's answers
    // would already be on screen. The counted assertions below — no answer
    // yet while the preview is up, exactly one when it lands — are the two
    // that adoption breaks, and it breaks them into a failure that reads
    // like broken streaming.
    await seedConversationKey(page, `dwiki-012c-${String(Date.now())}`);
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
    await page.getByRole('button', { name: 'Ask about this repository' }).click();
    const drawer = page.getByTestId('wiki-chat-drawer');
    await expect(drawer).toBeVisible();

    const question = 'Which bucket holds the pages?';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    // The preview, WHILE the run is still going. Both fixture runners cut the
    // answer into three fragments and pace them like a progress step, so it
    // is on screen for seconds, not for a frame.
    const streaming = drawer.getByTestId('wiki-chat-streaming');
    await expect(streaming).toBeVisible({ timeout: 60_000 });
    // It carries the ANSWER's text. A preview showing a progress line would
    // be the degraded reading — a token relayed as a thinking event — and it
    // would still be visible here.
    await expect(streaming).toContainText('Fixture answer');
    // And the turn has not settled: an answer already on screen would mean
    // the preview was read after the fact and proves nothing about streaming.
    await expect(drawer.getByTestId('wiki-chat-answer')).toHaveCount(0);

    // Then the answer lands, and the preview goes: the finished answer
    // replaces it, and leaving it would show the same text twice.
    await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText(
      `Fixture answer to: ${question}`,
      { timeout: 60_000 },
    );
    await expect(drawer.getByTestId('wiki-chat-streaming')).toHaveCount(0);
    await expect(drawer.getByTestId('wiki-chat-answer')).toHaveCount(1);
    await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
  });

  test('DWIKI-016: a page attached as context reaches the invocation, resolved to its text', async ({
    page,
  }) => {
    /**
     * THE ONE ASSERTION THAT CAN TELL THE FEATURE FROM ITS PARTS. The picker
     * sends page IDS; the host resolves them against the pinned version's
     * manifest and prepends the page BODIES to the question. Every unit test
     * on either side of that can pass while the two are not joined — the
     * defect class this repository keeps meeting (#597).
     *
     * The fixture `ask` echoes the question it was handed
     * (`run/fixture.go::fixtureAsk`), so the answer is a verbatim window onto
     * what the engine would have received. The sentence asserted below exists
     * ONLY in the seeded page body (`scripts/e2e-stack.sh`, router.md) — the
     * browser never had it, so it can only have arrived by the server reading
     * that page.
     */
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
    await page.getByRole('button', { name: 'Ask about this repository' }).click();
    const drawer = page.getByTestId('wiki-chat-drawer');
    await expect(drawer).toBeVisible();

    await drawer.getByRole('button', { name: 'Attach wiki pages' }).click();
    await page.getByText('architecture / router', { exact: true }).click();
    await page.keyboard.press('Escape');

    // The chip is the reader's own record of what the next question carries.
    await expect(drawer.getByTestId('wiki-chat-context-chips')).toContainText('architecture / router');

    const question = 'Which file assembles the router?';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    const answer = drawer.getByTestId('wiki-chat-answer').last();
    // The RESOLVED page text, and the source header that frames it.
    await expect(answer).toContainText('The HTTP router is assembled in', { timeout: 60_000 });
    await expect(answer).toContainText('wiki_pages/architecture/router.md');
    // The question is still the question: context is PREPENDED, not
    // substituted, and the engine's `Current question: ` hand-off is what
    // keeps the two tellable apart in a transcript.
    await expect(answer).toContainText(`Current question: ${question}`);
    await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
  });
});

/**
 * DWIKI-017 — the wiki chat's history is the SERVER'S, not this browser's.
 *
 * The drawer used to keep its conversation in `localStorage`, so it was gone
 * on another device, in another browser and on a cleared profile. Both turns
 * are now written by elitea-main — the question when the invoke is accepted,
 * the answer when the terminal poll is drained — into the ordinary tenant
 * chat tables.
 *
 * WHAT MAKES THIS A JOURNEY AND NOT A UNIT TEST. Three things have to line up
 * across two processes: the facade has to observe an invoke it only sees as a
 * proxy, it has to tee a poll the browser drains, and the drawer has to find
 * the conversation again through the ordinary chat listing with the right
 * filters. Every one of those has a unit test on each side, and none of those
 * tests can see the wiring — the defect class this repository keeps meeting
 * (#597).
 *
 * THE SECOND CONTEXT IS THE ASSERTION THAT MATTERS. A reload alone would pass
 * against the old localStorage drawer, because a reload keeps localStorage. It
 * is a fresh browser context — nothing carried over but the sign-in — that
 * tells "stored on the server" apart from "stored in this profile".
 */
test.describe('DeepWiki chat history', () => {
  test.setTimeout(180_000);

  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-017: a wiki conversation survives a reload and a fresh browser', async ({
    page,
    browser,
  }) => {
    /*
     * THIS JOURNEY OWNS ITS CONVERSATION, and seeds the key that says so.
     *
     * Left to itself the drawer ADOPTS the user's most recent stored
     * conversation whenever this browser has never opened the wiki before —
     * which is the feature, and which makes a shared stack non-deterministic:
     * CI runs with `E2E_REUSE_STACK=1`, so DWIKI-012's conversations from an
     * earlier run are already there, and this journey would append its two
     * turns to one of them. Everything positional then shifts, the
     * conversation's NAME is somebody else's first question, and the failure
     * reads as a broken feature rather than as a test asserting on a
     * transcript it did not write.
     *
     * Seeding a key makes `resolve()` report no mint, so no adoption happens
     * and the conversation below is this run's alone. Adoption keeps its own
     * coverage in the drawer's unit tests, where a stack full of other
     * journeys' rows cannot move it.
     */
    const stamp = Date.now();
    const conversationKey = `dwiki-017-${stamp}`;
    await seedConversationKey(page, conversationKey);

    await openChatDrawer(page);

    // Two turns, so the transcript's ORDER is observable. One turn would pass
    // against a reader that returned the newest group and stopped.
    const first = `Where do the wiki pages live? ${stamp}`;
    const second = `And who writes them? ${stamp}`;
    await ask(page, first);
    await ask(page, second);

    /* ── the same browser, after a reload ── */
    await page.reload({ waitUntil: 'domcontentloaded' });
    let drawer = await openChatDrawer(page);
    // `exact`, because the fixture's answer QUOTES the question: a substring
    // match resolves to the question bubble AND the answer bubble, and a
    // locator that matches two things proves neither.
    await expect(drawer.getByText(first, { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(drawer.getByText(second, { exact: true })).toBeVisible();

    /*
     * ── a fresh browser context: same user, ONE short key carried ──
     *
     * The key is the browser's handle on WHICH conversation, and nothing else:
     * not a message, not an answer, not a timestamp. Everything that appears
     * below therefore came off the server.
     */
    const fresh = await browser.newContext({ storageState: STORAGE_STATE.member });
    try {
      const other = await fresh.newPage();
      await seedConversationKey(other, conversationKey);
      await openDeepWiki(other, `/app/deepwiki/${SEEDED.readOnly.toolkitId}`);
      await other.getByRole('button', { name: 'Ask about this repository' }).click();
      const otherDrawer = other.getByTestId('wiki-chat-drawer');
      await expect(otherDrawer).toBeVisible();

      // The whole point: this profile has never held the conversation, so
      // anything on screen came off the server.
      await expect(otherDrawer.getByText(first, { exact: true })).toBeVisible({ timeout: 30_000 });
      await expect(otherDrawer.getByText(second, { exact: true })).toBeVisible();

      // The ANSWERS came back too, not only the questions — matched by their
      // own text rather than by position. `.first()` used to say this, and it
      // was wrong the moment the drawer started restoring earlier turns: on a
      // reused stack the first answer on screen can belong to a conversation
      // this test never wrote.
      await expect(
        otherDrawer.getByTestId('wiki-chat-answer').filter({ hasText: `Fixture answer to: ${first}` }),
      ).toHaveCount(1);
      await expect(otherDrawer.getByTestId('wiki-chat-answer').last()).toContainText(
        `Fixture answer to: ${second}`,
      );
    } finally {
      await fresh.close();
    }

    /* ── "Clear" starts a NEW conversation; it does not erase the old one ── */
    drawer = page.getByTestId('wiki-chat-drawer');
    await drawer.getByRole('button', { name: 'Clear the conversation' }).click();
    await expect(drawer.getByText(first, { exact: true })).toHaveCount(0);

    /*
     * And the cleared conversation is STILL THERE. A "Clear" that deleted
     * tenant data would pass every assertion above and lose the history that
     * was the point of keeping.
     *
     * Found by its KEY, not by its name or its position. On a reused stack
     * this listing holds every conversation every earlier run left behind, so
     * "the newest row" and "within the first page" are both things this test
     * cannot know — and the drawer's own query is the one under test, so it
     * is the one asked.
     */
    const origin = new URL(page.url()).origin;
    const stored = await page.request.get(
      `${origin}/api/v2/elitea_core/conversations/prompt_lib/${SEEDED.projectId}` +
        `?source=deepwiki&entity_name=toolkit&entity_meta_id=${SEEDED.readOnly.toolkitId}` +
        `&hidden=only&mine=true&limit=100`,
    );
    expect(stored.ok(), `listing wiki conversations: ${stored.status()}`).toBe(true);
    const body = (await stored.json()) as {
      rows?: { name?: string; meta?: { wiki_chat_key?: string } }[];
    };
    const cleared = (body.rows ?? []).find((row) => row.meta?.wiki_chat_key === conversationKey);
    expect(cleared, 'the cleared conversation is still stored').toBeDefined();
    expect(cleared?.name, 'and still named after the question that opened it').toBe(first);
  });
});

/**
 * DWIKI-018 — a reader-uploaded file reaches the invocation, prepended in
 * front of the question (#873).
 *
 * The fixture `ask` echoes `arguments["question"]` verbatim
 * (`run/fixture.go::fixtureAsk`), and that argument is derived AFTER
 * `ApplyExtraContext` has already folded the attachment's content into it —
 * so the answer below is a direct window onto what the engine received, the
 * same technique DWIKI-016 uses for a wiki-page attachment.
 */
test.describe('DeepWiki chat file attachments', () => {
  test.setTimeout(120_000);

  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-018: an attached file’s content is prepended before the question', async ({ page }) => {
    const drawer = await openChatDrawer(page);

    await drawer.getByTestId('wiki-chat-attach-input').setInputFiles({
      name: 'notes.md',
      mimeType: 'text/plain',
      buffer: Buffer.from('The onboarding checklist lives in a file nobody indexed.'),
    });
    await expect(drawer.getByTestId('wiki-chat-attach-chips')).toContainText('notes.md');

    const question = 'What does the attached note say?';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    const answer = drawer.getByTestId('wiki-chat-answer').last();
    await expect(answer).toContainText('Given these attached files:', { timeout: 60_000 });
    await expect(answer).toContainText('--- file: notes.md ---');
    await expect(answer).toContainText('The onboarding checklist lives in a file nobody indexed.');
    // Prepended, not substituted: the reader's own question is still there.
    await expect(answer).toContainText(question);
    await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
  });

  test('removing an attachment before sending leaves no trace of it in the invocation', async ({ page }) => {
    const drawer = await openChatDrawer(page);

    await drawer.getByTestId('wiki-chat-attach-input').setInputFiles({
      name: 'scratch.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('this must never reach the provider'),
    });
    await expect(drawer.getByTestId('wiki-chat-attach-chips')).toContainText('scratch.txt');
    // NOT `getByTestId('CancelIcon')`: that's MUI's own auto-testid on the
    // Chip's DEFAULT delete icon (`createSvgIcon.js`), gated on
    // `NODE_ENV !== 'production'` — absent from the production build this
    // stack serves. `WikiFileAttach` now gives the delete icon its own
    // stable testid for exactly this reason.
    await drawer.getByTestId('wiki-chat-attach-chips').getByTestId('wiki-chat-attach-chip-remove').click();
    await expect(drawer.getByTestId('wiki-chat-attach-chips')).toHaveCount(0);

    const question = 'A question with nothing attached';
    await drawer.getByPlaceholder('Ask about this repository').fill(question);
    await drawer.getByRole('button', { name: 'Send' }).click();

    const answer = drawer.getByTestId('wiki-chat-answer').last();
    await expect(answer).toContainText(`Fixture answer to: ${question}`, { timeout: 60_000 });
    await expect(answer).not.toContainText('this must never reach the provider');
  });
});

/**
 * DWIKI-019 — the session list: every stored wiki chat for this toolkit is
 * listable, resumable, and deletable, not just the one the browser last
 * held a key to (#873).
 */
test.describe('DeepWiki chat sessions', () => {
  test.setTimeout(180_000);

  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-019: past sessions can be listed, resumed, and deleted', async ({ page }) => {
    const stamp = Date.now();
    await seedConversationKey(page, `dwiki-019-${stamp}`);
    let drawer = await openChatDrawer(page);

    const first = `first session question ${stamp}`;
    await ask(page, first);

    // "Clear" opens a NEW session; the one just asked stays stored.
    await drawer.getByRole('button', { name: 'Clear the conversation' }).click();
    const second = `second session question ${stamp}`;
    await ask(page, second);

    await drawer.getByTestId('wiki-chat-sessions-button').click();
    const options = page.getByTestId('wiki-chat-session-option');
    await expect(options.filter({ hasText: first })).toHaveCount(1);
    await expect(options.filter({ hasText: second })).toHaveCount(1);

    // Resume the FIRST session — the one this browser is not currently on.
    await options.filter({ hasText: first }).click();
    await expect(drawer.getByText(first, { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(drawer.getByText(second, { exact: true })).toHaveCount(0);

    // Delete the session now open. The drawer starts a fresh one rather
    // than showing a transcript for a conversation that no longer exists.
    await drawer.getByTestId('wiki-chat-sessions-button').click();
    await options.filter({ hasText: first }).getByTestId('wiki-chat-session-delete').click();
    const confirmModal = page.getByTestId('wiki-chat-session-delete-modal');
    await confirmModal.getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(page.getByText('Ask a question about this repository')).toBeVisible();

    // And it is gone from the list — not merely cleared off screen.
    drawer = page.getByTestId('wiki-chat-drawer');
    await drawer.getByTestId('wiki-chat-sessions-button').click();
    await expect(page.getByTestId('wiki-chat-session-option').filter({ hasText: first })).toHaveCount(0);
    await expect(page.getByTestId('wiki-chat-session-option').filter({ hasText: second })).toHaveCount(1);
  });
});
