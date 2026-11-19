/**
 * The whole skills flow, end to end (skills epic 2169, story S8 / #2184;
 * ADR-0051 amendment 1 and ADR-0050 amendment 1): a skill is a capability that a
 * conversation pins, never a participant, and text from another project is quoted
 * data until the project trusts that exact version.
 *
 * ONE journey, in the order a person meets it:
 *
 *   1. a skill is published to the Marketplace and its listing is LIKED;
 *   2. TRY it opens a chat with the listing pinned by reference, untrusted, and
 *      forks nothing;
 *   3. ADD it (the fork) puts a copy in the project, and the skills list says the
 *      copy is untrusted, from the row itself;
 *   4. a model chat is started and the copy is PINNED from the Skills section, which
 *      sits apart from the Participants;
 *   5. a message is sent with the `/` list's Skills entry and the `/skill` picker:
 *      the start body carries the skill by id, and the model is given the text as
 *      QUOTED data (`<untrusted source="skill:NAME">`);
 *   6. the skill is REVIEWED and TRUSTED in its editor, and the next turn gives the
 *      same text as an INSTRUCTION (`<active_instruction`).
 *
 * WHAT MAKES IT DISCRIMINATING. Every claim is read from the server or from what
 * the model was handed, never from the screen alone: the pins and their trust from
 * the conversation-skills route, the trust of the copy from `getSkillTrust` and from
 * the list row, the start request's own body, and the mock model's journal (its
 * system prompt, the only observable for what the runtime did to an instruction).
 * A turn is settled by the STORED assistant row, with `is_error` present and false
 * (`expectStoredAssistantAnswer`), never by a streamed token: the poll that accepted
 * an absent `is_error` settled on the first token and leaked running turns.
 *
 * LEG. The worker is the native Rust runtime on every stack that runs `chat-stream`
 * (the Python worker is retired), so there is one leg; the model is the offline mock
 * (`E2E_CHAT_MODEL` names a real one, and a real model is not asked to echo: the
 * `contains` checks then stay on what the runtime stores, not on the model's words).
 *
 * WHY IT LIVES HERE: every turn needs the full standalone stack (the `chat-stream`
 * project, `scripts/chat-stream-e2e.sh`), serially.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  clearMockLlmJournal,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  resolveCatalogueProjectId,
} from '../fixtures/api';
import { openParticipants } from '../fixtures/sidePanel';

const API = `${BASE_URL}/api/v2`;

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** One pin as `listConversationSkills` answers it (the part this journey reads). */
interface Pin {
  readonly id: number;
  readonly kind: string;
  readonly skill_id?: number;
  readonly name: string;
  readonly trust: { readonly state: string };
  readonly available: boolean;
}

function token(tag: string): string {
  return `${tag}${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

async function pinsOf(page: Page, projectId: string, conversationId: string): Promise<readonly Pin[]> {
  const response = await page.request.get(`${API}/elitea_core/conversation_skills/prompt_lib/${projectId}/${conversationId}`);
  expect(response.status(), 'the conversation-skills route must be mounted').toBe(200);
  return ((await response.json()) as { rows: readonly Pin[] }).rows;
}

/** What the model was handed on the turn that just ran: the system prompt and the history, as text. */
async function modelInput(page: Page, projectId: string): Promise<string> {
  const entries = await readMockLlmJournal(page, projectId);
  expect(entries.length, 'the mock model must have served the turn').toBeGreaterThan(0);
  return entries.map((entry) => `${entry.instructions}\n${entry.history.map((row) => row.text).join('\n')}`).join('\n----\n');
}

/** Select the seeded model in the composer, as every turn-driving journey does. */
async function selectModel(page: Page): Promise<void> {
  await page.getByTestId('model-selector-button').click();
  const option = page.getByRole('option').filter({ hasText: MODEL_NAME }).first();
  await expect(option, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await option.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });
}

/** Send what the composer holds and return the start request's own body. */
async function sendAndReadStartBody(page: Page, send: Locator): Promise<Record<string, unknown>> {
  const started = page.waitForRequest((request) => START_RE.test(request.url()) && request.method() === 'POST', { timeout: 60_000 });
  const admitted = page.waitForResponse((response) => START_RE.test(response.url()) && response.request().method() === 'POST', { timeout: 60_000 });
  await send.click();
  const request = await started;
  const response = await admitted;
  expect(response.status(), `the turn was refused: ${(await response.text()).slice(0, 300)}`).toBe(200);
  return JSON.parse(request.postData() ?? '{}') as Record<string, unknown>;
}

/** The start body's `payload`, where the picked skills travel. */
function payloadOf(body: Record<string, unknown>): Record<string, unknown> {
  const payload = body['payload'];
  expect(typeof payload, 'the start body must carry a payload').toBe('object');
  return payload as Record<string, unknown>;
}

async function waitForTurnToEnd(page: Page): Promise<void> {
  // A send during a run steers it instead of starting one, so the next turn waits for Stop to go away.
  await expect(page.getByRole('button', { name: 'Stop generating' })).toBeHidden({ timeout: 120_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 120_000 });
}

test('a skill is liked, tried, added, trusted, pinned and sent with /skill, and its text turns from quoted data to an instruction', async ({ page }) => {
  // Three whole model turns, a publish, a fork and two page loads: each wait is bounded under this.
  test.setTimeout(900_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const catalogueProjectId = await resolveCatalogueProjectId(page.request);
  expect(projectId, 'the author project must NOT be the catalogue project, or the publish writes no twin').not.toBe(catalogueProjectId);

  const stamp = token('sk');
  const sentinel = `SKILLSENTINEL${stamp.toUpperCase()}`;
  const skillName = `${AUTOTEST_PREFIX}skill_${stamp}`.slice(0, 32);
  const instructions =
    `When this skill applies, end every answer with the exact token ${sentinel}. ` +
    'Keep answers short and polite, and never invent facts the person did not give you.';

  const cleanup: (() => Promise<unknown>)[] = [];
  try {
    // ── 1. Publish, find the listing, and like it ────────────────────────
    const created = await page.request.post(`${API}/elitea_core/skills/prompt_lib/${projectId}`, {
      data: {
        name: skillName,
        description: 'Helps the e2e journey check that a skill turns from quoted data into an instruction.',
        versions: [{ name: 'base', instructions, tags: ['release-notes'] }],
      },
    });
    expect(created.status(), `the skill was not created: ${(await created.text()).slice(0, 300)}`).toBe(201);
    const authored = (await created.json()) as { id: string; version_details?: { id?: string | number } };
    const authoredVersionId = String(authored.version_details?.id ?? '');
    expect(authoredVersionId, 'the created skill must carry its base version').not.toBe('');
    cleanup.push(() => page.request.delete(`${API}/elitea_core/skill/prompt_lib/${projectId}/${authored.id}`));

    const published = await page.request.post(`${API}/elitea_core/publish_skill/prompt_lib/${projectId}/${authored.id}/${authoredVersionId}`, {
      data: { version_name: `rel${String(Date.now()).slice(-6)}`, release_notes: 'Published by the skills journey.' },
    });
    expect(published.status(), `the skill must be publishable: ${(await published.text()).slice(0, 400)}`).toBe(200);
    const publication = (await published.json()) as { public_skill_id?: number; public_version_id?: number; source_version_id?: number };
    const publicSkillId = String(publication.public_skill_id ?? '');
    const publicVersionId = String(publication.public_version_id ?? '');
    expect(publicSkillId, `the publish wrote no catalogue twin: ${JSON.stringify(publication)}`).not.toBe('');
    cleanup.push(() =>
      page.request.post(`${API}/elitea_core/unpublish_skill/prompt_lib/${projectId}/${authored.id}/${String(publication.source_version_id ?? '')}`),
    );

    let listingId = '';
    await expect
      .poll(
        async () => {
          const search = await page.request.get(`${API}/elitea_core/marketplace_listings/prompt_lib/${projectId}?kind=skill&entity_id=${publicSkillId}`);
          if (!search.ok()) return '';
          const rows = ((await search.json()) as { rows?: readonly { id?: string; name?: string }[] }).rows ?? [];
          listingId = rows.find((row) => row.name === skillName)?.id ?? '';
          return listingId;
        },
        { timeout: 30_000, message: 'the published skill never reached the Marketplace search route' },
      )
      .not.toBe('');

    const listingPage = await page.context().newPage();
    let trySkillConversationId = '';
    try {
      await listingPage.goto(`${BASE_URL}/app/marketplace/skills/${listingId}`);
      const like = listingPage.getByTestId(`marketplace-like-skills-${publicSkillId}`);
      await expect(like, 'the listing must offer a like').toBeVisible({ timeout: 30_000 });
      await expect(like).toHaveAttribute('aria-pressed', 'false');
      // The heart writes the like through `/social/like/.../skill/{twin id}`. The browser addresses the
      // project named by `VITE_PUBLIC_PROJECT_ID`, which on the standalone stacks is a frontend-only id
      // (99) the personas are not members of (the route answers 403 there) and not the catalogue project
      // the server counts in. So the heart is asserted on what the browser SENT, and the count on the
      // catalogue project below.
      const liking = listingPage.waitForRequest((r) => /\/social\/like\/prompt_lib\/\d+\/skill\//.test(r.url()) && r.method() === 'POST', { timeout: 15_000 });
      await like.click();
      const likeRequest = await liking;
      expect(new URL(likeRequest.url()).pathname.endsWith(`/skill/${publicSkillId}`), 'a skill is liked as the skill entity, by its public twin id').toBe(true);
      // The server's count: the like of the catalogue project's own entity is what the listing read counts.
      // A persona that is not a member of the catalogue project cannot like in it (403); the count is then
      // covered by the Postgres cases next to the listing read, and this journey says so rather than passing.
      const counted = await page.request.post(`${API}/social/like/prompt_lib/${catalogueProjectId}/skill/${publicSkillId}`);
      if (counted.status() === 403) {
        test.info().annotations.push({ type: 'like-count', description: 'the persona is not a member of the catalogue project; the count is not asserted here' });
      } else {
        expect(counted.status(), 'the like of the public twin must be accepted').toBe(200);
        await expect
          .poll(async () => {
            const detail = await page.request.get(`${API}/elitea_core/marketplace_listing/prompt_lib/${projectId}/${listingId}`);
            const listing = ((await detail.json()) as { listing?: { likes?: number; is_liked?: boolean } }).listing;
            return `${String((listing?.likes ?? 0) >= 1)}:${String(listing?.is_liked ?? false)}`;
          }, { timeout: 15_000, message: 'the like was not counted by the listing read' })
          .toBe('true:true');
      }

      // ── 2. Try it: a chat with the listing pinned by reference ─────────
      await listingPage.getByTestId('marketplace-try-skill').click();
      await listingPage.waitForURL(/\/app\/chat\/(\d+)/, { timeout: 30_000 });
      trySkillConversationId = /\/app\/chat\/(\d+)/.exec(listingPage.url())?.[1] ?? '';
      expect(trySkillConversationId).not.toBe('');
      cleanup.push(() => page.request.delete(`${API}/elitea_core/conversation/prompt_lib/${projectId}/${trySkillConversationId}`));
      const tryPins = await pinsOf(page, projectId, trySkillConversationId);
      expect(tryPins.map((pin) => [pin.kind, pin.trust.state]), 'Try pins the listing by reference, untrusted, and forks nothing').toEqual([['listing', 'untrusted']]);
      await openParticipants(listingPage);
      const trySection = listingPage.getByTestId('conversation-skills');
      await expect(trySection.getByText(skillName)).toBeVisible({ timeout: 20_000 });
      await expect(trySection.getByTestId(`conversation-skill-untrusted-${tryPins[0]?.id ?? 0}`)).toBeVisible();
    } finally {
      await listingPage.close();
    }

    // ── 3. Add it: the fork is a copy in this project, untrusted ──────────
    const forked = await page.request.post(`${API}/elitea_core/attach_public_skill/prompt_lib/${projectId}`, {
      data: { public_skill_id: Number(publicSkillId), public_version_id: Number(publicVersionId), fork_only: true },
    });
    expect(forked.status(), `the skill must be addable: ${(await forked.text()).slice(0, 300)}`).toBe(200);
    const copyId = String(((await forked.json()) as { skill_id?: number }).skill_id ?? '');
    expect(copyId, 'Add must answer the project copy').not.toBe('');
    expect(copyId, 'the copy is a new skill, not the one that was published').not.toBe(authored.id);
    cleanup.push(() => page.request.delete(`${API}/elitea_core/skill/prompt_lib/${projectId}/${copyId}`));

    const rows = await page.request.get(`${API}/elitea_core/skills/prompt_lib/${projectId}?page=1&page_size=100&query=${encodeURIComponent(stamp)}`);
    const listed = ((await rows.json()) as { items: readonly { id: string; name: string; trust?: { foreign: boolean; state: string } }[] }).items;
    const copyRow = listed.find((row) => row.id === copyId);
    const ownRow = listed.find((row) => row.id === authored.id);
    expect(copyRow?.trust, 'the list row of the copy carries its trust state, with no per-row request').toMatchObject({ foreign: true, state: 'untrusted' });
    expect(ownRow?.trust, 'a skill written here needs no trust').toMatchObject({ foreign: false, state: 'not_required' });

    // The copy keeps the published skill's name, and a `~name` is resolved by name, so the original goes
    // now (a published skill is unpublished first): the copy does not depend on it, and one name must
    // mean one skill below.
    const unpublished = await page.request.post(
      `${API}/elitea_core/unpublish_skill/prompt_lib/${projectId}/${authored.id}/${String(publication.source_version_id ?? '')}`,
    );
    expect(unpublished.ok(), `the original was not unpublished: ${unpublished.status()}`).toBe(true);
    const removed = await page.request.delete(`${API}/elitea_core/skill/prompt_lib/${projectId}/${authored.id}`);
    expect(removed.ok(), `the original skill was not removed: ${removed.status()}`).toBe(true);

    // The skills list says so, as a badge on the card.
    await page.goto(`${BASE_URL}/app/skills/all`);
    await expect(page.getByText(skillName).first()).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText('Untrusted').first(), 'the skills list badges a skill the project has not trusted').toBeVisible();

    // ── 4. A model chat, and the copy pinned from the Skills section ─────
    const conversationCreates: string[] = [];
    page.on('response', (response) => {
      if (CONVERSATIONS_RE.test(new URL(response.url()).pathname) && response.request().method() === 'POST') conversationCreates.push(response.url());
    });
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    await selectModel(page);

    const opening = token('open');
    const openSend = await fillComposer(page, `autotest echo exactly: ${opening}`);
    const createdConversation = page.waitForResponse((r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST', { timeout: 45_000 });
    const openStart = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 45_000 });
    await openSend.click();
    const conversation = (await (await createdConversation).json()) as { id?: string; uuid?: string };
    const conversationId = conversation.id ?? '';
    expect(conversationId).toMatch(/^\d+$/);
    cleanup.push(() => page.request.delete(`${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`));
    expect((await openStart).status(), 'the opening turn must be admitted').toBe(200);
    await expectStoredAssistantAnswer(page, projectId, conversationId, { timeout: 120_000, contains: opening, message: 'the opening answer was never stored' });
    await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    await waitForTurnToEnd(page);
    expect(await pinsOf(page, projectId, conversationId), 'nothing is pinned yet').toEqual([]);

    // Pin the copy from the Skills section: apart from the Participants.
    await openParticipants(page);
    const section = page.getByTestId('conversation-skills');
    await expect(section).toBeVisible({ timeout: 20_000 });
    await page.getByTestId('conversation-skills-add').click();
    const pinned = page.waitForResponse((r) => /\/conversation_skills\/prompt_lib\//.test(r.url()) && r.request().method() === 'POST', { timeout: 20_000 });
    await page.getByTestId(`conversation-skills-option-${copyId}`).click();
    expect((await pinned).status(), 'the pin must be created').toBe(201);
    const pins = await pinsOf(page, projectId, conversationId);
    expect(pins.map((pin) => [pin.kind, pin.skill_id, pin.trust.state, pin.available])).toEqual([['project', Number(copyId), 'untrusted', true]]);
    await expect(section.getByText(skillName)).toBeVisible({ timeout: 10_000 });
    await expect(section.getByTestId(`conversation-skill-untrusted-${pins[0]?.id ?? 0}`), 'the section says the pinned copy is untrusted').toBeVisible();
    await expect(page.getByTestId('participants-container').getByText(skillName), 'a skill is not a participant').toHaveCount(0);

    // ── 5. Send with the "/" list's Skills entry and /skill ──────────────
    const input = page.getByTestId('chat-message-input');
    await input.pressSequentially('/');
    const entry = page.getByTestId('slash-skills-entry');
    await expect(entry, 'the "/" list starts with Skills').toBeVisible({ timeout: 10_000 });
    await entry.click();
    await expect(input, 'the entry writes /skill and hands over to the skill picker').toHaveValue('/skill ');
    await input.pressSequentially(skillName);
    await expect(page.getByText(skillName).last()).toBeVisible();
    await input.press('Enter');
    await expect(input, 'a pick writes the canonical ~name, never the text /skill').toHaveValue(`~${skillName} `, { timeout: 10_000 });
    const quoted = token('quoted');
    await input.pressSequentially(`autotest echo exactly: ${quoted}`);
    await clearMockLlmJournal(page);
    const quotedBody = await sendAndReadStartBody(page, page.getByTestId('chat-send-button'));
    expect(payloadOf(quotedBody)['skills'], 'the start body (payload.skills) names the picked skill by id').toEqual([{ source: 'project', id: Number(copyId) }]);
    expect(JSON.stringify(quotedBody), 'the skill text never travels from the client').not.toContain(sentinel);
    await expectStoredAssistantAnswer(page, projectId, conversationId, { timeout: 120_000, contains: quoted, message: 'the /skill turn was never answered' });
    const quotedInput = await modelInput(page, projectId);
    expect(quotedInput, 'an untrusted skill reaches the model as quoted data').toContain(`<untrusted source="skill:${skillName}">`);
    expect(quotedInput).toContain(sentinel);
    expect(quotedInput, 'and not as an active instruction').not.toContain('<active_instruction');
    await waitForTurnToEnd(page);

    // ── 6. Review and trust it in the editor: quoted data becomes an instruction ──
    const editor = await page.context().newPage();
    try {
      await editor.goto(`${BASE_URL}/app/skills/all/${copyId}`);
      const panel = editor.getByTestId('skill-trust-panel');
      await expect(panel, 'the editor of a skill from another project says it is untrusted').toContainText('quoted data', { timeout: 30_000 });
      await expect(editor.getByTestId('skill-instructions-input')).toHaveValue(instructions, { timeout: 20_000 });
      const trusting = editor.waitForResponse((r) => /\/skill_trust\/prompt_lib\//.test(r.url()) && r.request().method() === 'POST', { timeout: 20_000 });
      await editor.getByTestId('skill-trust-grant').click();
      expect((await trusting).status(), 'trusting the exact version it read must be accepted').toBe(200);
      await expect(editor.getByTestId('skill-trust-revoke')).toBeVisible({ timeout: 15_000 });
    } finally {
      await editor.close();
    }
    const status = await page.request.get(`${API}/elitea_core/skill_trust/prompt_lib/${projectId}/${copyId}`);
    expect(((await status.json()) as { state?: string }).state).toBe('trusted');
    expect((await pinsOf(page, projectId, conversationId))[0]?.trust.state, 'the pin follows the trust').toBe('trusted');

    // The pin still applies, with no mention this time: the text is an instruction now.
    const instructed = token('instructed');
    const instructedSend = await fillComposer(page, `autotest echo exactly: ${instructed}`);
    await clearMockLlmJournal(page);
    const instructedBody = await sendAndReadStartBody(page, instructedSend);
    expect(payloadOf(instructedBody)['skills'], 'a pin travels no id: the server reads it').toBeUndefined();
    await expectStoredAssistantAnswer(page, projectId, conversationId, { timeout: 120_000, contains: instructed, message: 'the turn after trust was never answered' });
    const trustedInput = await modelInput(page, projectId);
    expect(trustedInput, 'a trusted skill is an active instruction').toContain('<active_instruction');
    expect(trustedInput).toContain(sentinel);
    expect(trustedInput, 'and no longer quoted data').not.toContain(`<untrusted source="skill:${skillName}">`);

    expect(conversationCreates, 'every turn continued the one conversation').toHaveLength(1);
  } finally {
    for (const undo of cleanup.reverse()) await undo().catch(() => undefined);
  }
});
