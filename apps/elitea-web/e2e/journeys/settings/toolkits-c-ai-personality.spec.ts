/**
 * Wave-1 package toolkits-credentials/C-providers — the `personalization`
 * cluster (`S/port/pkgs/w1-tk-C-providers.md`). These 8 legacy cases live
 * under `sidebar-menu/personalization` upstream but exercise Settings › AI
 * Personality (`src/pages/settings/AIPersonality.tsx` +
 * `src/features/settings/ui/ai-personality/**`), so they are ported here,
 * not under `toolkits/`, with a `toolkits-c-` file prefix per the package
 * brief (avoids clashing with other agents' `settings.*` files).
 *
 * Two of the eight (ELITEA-2493/2494) describe a PER-CONVERSATION "Context
 * Management" panel that inherits/overrides the Settings persona. No such
 * panel exists: grepping `src/widgets/chat-box` and `src/pages/chat` for
 * `context.?management` finds nothing, and the only "Context Management" in
 * this app is `src/features/settings/ui/profile/ProfileContextManagement.tsx`
 * — a GLOBAL settings toggle for long-term-memory context budgeting, with no
 * link to `persona`/`personality_instructions` and nothing reachable from an
 * open chat conversation. Recorded NA in the ledger, not tested here.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * #929 (FIXED) — every autosave 400'd; root-caused and closed
 * ─────────────────────────────────────────────────────────────────────────
 * `AIPersonalityFormContent`/`SettingsFormProvider.tsx` PUT the WHOLE author
 * record on every autosave (persona select, or blurring the instructions
 * field), and `settingsProfileForm.ts`'s `buildAuthorUpdate` always included
 * `default_summarization.summary_model_project_id` — even for an account
 * that has never touched Settings › Memory. `serializeSummarization` falls
 * back to the ROUTE's `projectId` prop when the stored value is not a JS
 * string, and the OLD `buildAuthorUpdate` wrote that value straight through
 * as `summary_model_project_id` with no int conversion — but project ids
 * are strings throughout this app, while this ONE wire field is genuinely
 * typed int (`MemorySummarization.summary_model_project_id`, `zod.int()`;
 * `internal/domain/contextsettings/userdefaults.go`'s `*int`), so the
 * server rejected it outright:
 *
 *   PUT /social/author → 400
 *   {"error":"summary_model_project_id must be of type int",
 *    "field":"default_summarization.summary_model_project_id"}
 *
 * Fixed at the wire boundary, not the form's own type: `deserializeMemoryBlocks`
 * now calls a new `projectIdField` helper that sends `Number(model_project_id)`
 * when it parses to an integer and OMITS the key otherwise (the server keeps
 * the stored value for an absent key, per its own doc comment) — `model_
 * project_id` itself stays the string id everywhere else in Formik state and
 * in the UI. All six cases below (ELITEA-2495-2500) were blocked on this one
 * failure at the first `selectPersona` call; ELITEA-2495 is kept as the full
 * scenario (copy/paste across personas) and 2496-2500 remain DUP in the
 * ledger — same mechanism, not a distinct assertion once the save itself
 * works.
 *
 * NEW, SEPARATE finding while verifying the fix: with the 400 gone, this
 * case exposed an intermittent failure to fire the autosave PUT AT ALL
 * within the 20s window. Root-caused with console/network tracing (a
 * disposable debug build, not shipped): it is NOT a timing race in
 * `useFormikAutoSaveOnBlur`/`SingleSelect` — every genuine persona change
 * fires `onChange` -> `requestSubmit` -> the PUT within ~10ms, reliably,
 * every time it was tried. The actual mechanism: MUI's `Select`
 * (`SelectInput.js`'s `update()`) only calls `onChange` on an ACTUAL value
 * change — reselecting the option the account is ALREADY on is a correct,
 * intentional no-op (no event, no PUT, nothing to wait for). This account
 * (`/social/author`, one row per authenticated identity, no per-test scratch
 * available) is shared across every run of this spec, and the very first
 * thing the test does is `selectPersona(page, 'QA')` — so whenever a
 * PREVIOUS run left the account already on `qa` (including a previous run
 * that failed at this exact step, which leaves it on `qa` again), the next
 * run's first select is a no-op, `selectPersona` times out waiting for a PUT
 * the app had no reason to send, and the account is STILL on `qa` afterwards
 * — a stable failure attractor, not a ~50% client race. `selectPersona`
 * below is now self-healing: if the combobox already reads the target
 * label, it nudges through a different option first so the click that
 * matters is always a genuine change.
 *
 * ONE ACCOUNT, ONE WRITER: fixing the no-op above surfaced the SAME shared-
 * account shape `chat.contextBudget.spec.ts` carries this phrase for — every
 * repeat of this ONE test (`--repeat-each`) hits the identical `/social/
 * author` row, so one instance's write can land mid-way through another
 * instance's own read-edit-save sequence. `test.describe.configure({ mode:
 * 'serial' })` alone does NOT fix this: `--repeat-each` copies form separate
 * serial groups of one test each and Playwright still schedules those
 * groups onto different workers at the same time (measured directly —
 * `--workers=4` still ran up to 4 repeats concurrently with `serial`
 * configured). `settings.pat-expiry-notifications.spec.ts` already
 * documents the same finding for the same reason. The fix is the one that
 * spec uses: `fixtures/mutex.ts`'s cross-WORKER named lock, held for the
 * whole test body, so only one instance ever touches the account at a time.
 *
 * Fixing the concurrent collision surfaced a SEQUENTIAL one underneath it: a
 * completed run leaves `persona` on `nerdy` and `personality_instructions.
 * nerdy` holding the pasted copy, so the very next run's own "Nerdy starts
 * empty" assumption (the `toHaveValue('')` below) is false from its second
 * execution onward — reproduced with the mutex held and still failing.
 * Fixed the same way `chat.contextBudget.spec.ts` fixes its own
 * account-wide write: read the account's `persona`/`personality_
 * instructions` before touching anything, force the one precondition the
 * test actually needs (`nerdy` empty), and restore the ORIGINAL values in a
 * `finally` — this test does not own the account, it borrows it.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE REMAINING FAILURE WAS A REAL PRODUCT BUG (fixed in `SettingsFormProvider.tsx`)
 * ─────────────────────────────────────────────────────────────────────────
 * With the account isolated (mutex) and reset to a known state (above), one
 * shape still failed reliably (even single-worker, no concurrency at all):
 * the SECOND `blurInstructionsAndSave` — after `selectPersona('Nerdy')`,
 * paste, blur — never fired its PUT. Root-caused with console/network
 * tracing (a disposable debug build, not shipped):
 *
 * `SettingsFormProvider.tsx`'s `handleSubmit` awaits `updateCurrentAuthor`
 * then `queryClient.invalidateQueries(...)` (a REFETCH of the same
 * `useGetCurrentAuthor` query this form's `initialValues` is built from,
 * with `enableReinitialize` on) before calling `helpers.resetForm({ values
 * })` with `values` — a SNAPSHOT captured when THIS submit started. Two
 * network round trips is enough time for the user to make ANOTHER edit
 * (exactly this test's paste): when the refetch lands, Formik's own
 * `enableReinitialize` effect calls `resetForm()` using the fresh (but now
 * STALE relative to the interim edit) fetched data, and/or the explicit
 * `resetForm({ values })` overwrites with the even-staler submit-time
 * snapshot — either way, BOTH current values and the dirty baseline get set
 * to old data, silently discarding the interim edit with no error and no
 * later save ever firing for it. Confirmed byte-for-byte in the trace: the
 * paste landed, `dirty` briefly went `true`, then flipped back to `false`
 * within ~1ms of the stale reset, before the test's own blur click.
 *
 * Fixed at the root, not worked around here: `queryClient.invalidateQueries`
 * now passes `refetchType: 'none'` (invalidates the cache for other readers'
 * NEXT mount without forcing an immediate refetch of this already-mounted,
 * possibly-mid-edit form), and the explicit `resetForm({ values })` is
 * guarded to only fire when the live form still matches what was submitted
 * (`valuesUnchangedSince`, via a new `innerRef`) — otherwise it is left
 * alone, still dirty, for the interim edit's own autosave to save. See that
 * file's `handleSubmit` for the full trace and reasoning.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { acquireNamedLock } from '../../fixtures/mutex';

/** The subset of `GET /social/author` this file reads and carries forward. */
interface AuthorRecord {
  readonly name?: string;
  readonly description?: string;
  readonly avatar?: string;
  readonly personalization?: Record<string, unknown>;
  readonly default_context_management?: Record<string, unknown>;
  readonly default_summarization?: Record<string, unknown>;
}

async function readAuthor(request: APIRequestContext): Promise<AuthorRecord> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status(), 'the author profile must be readable, or nothing below means anything').toBe(200);
  return (await response.json()) as AuthorRecord;
}

/**
 * Writes `persona`/`personality_instructions` onto the author record,
 * carrying every other replace-outright field forward — the same idiom
 * `chat.contextBudget.spec.ts`'s `writeContextDefaults` uses for its own
 * account-wide field, and for the same reason (see this file's header).
 */
async function writePersonalization(
  request: APIRequestContext,
  author: AuthorRecord,
  persona: string,
  personalityInstructions: Readonly<Record<string, string>>,
): Promise<void> {
  const response = await request.put(`${API_BASE}/social/author`, {
    data: {
      name: author.name ?? '',
      description: author.description ?? '',
      avatar: author.avatar ?? '',
      personalization: { ...author.personalization, persona, personality_instructions: personalityInstructions },
      default_context_management: author.default_context_management ?? {},
      default_summarization: author.default_summarization ?? {},
    },
  });
  expect(response.status(), `the profile write must be accepted: ${(await response.text()).slice(0, 200)}`).toBe(200);
}

const AI_PERSONALITY_URL = `${BASE_URL}/app/settings/ai-personality`;
const RUN_ID = `${Date.now()}`;

async function gotoAiPersonality(page: Page): Promise<void> {
  await page.goto(AI_PERSONALITY_URL, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('ai-personality-form-content')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('persona-management-section')).toBeVisible({ timeout: 30_000 });
}

function personaCombobox(page: Page) {
  return page.getByTestId('persona-management-section').getByRole('combobox');
}

function instructionsField(page: Page) {
  return page.getByRole('textbox', { name: 'User instructions' });
}

/** Waits for the `/social/author` autosave PUT a click is expected to trigger, and asserts it succeeded. */
async function clickAndAwaitAuthorSave(page: Page, click: () => Promise<void>, context: string): Promise<void> {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PUT' && r.url().includes('/social/author'), { timeout: 20_000 }),
    click(),
  ]);
  expect(response.status(), `${context}: PUT /social/author answered ${response.status()}: ${await response.text()}`).toBeLessThan(300);
}

/**
 * Selects a persona from the dropdown and waits for the auto-save PUT the
 * select's own `onChange` fires.
 *
 * `/social/author` is one row per authenticated identity, shared across
 * every run of this spec (no per-test scratch identity exists for it), and
 * MUI's `Select` only calls `onChange` on an ACTUAL value change — clicking
 * the option the account is ALREADY on is a correct no-op with no PUT to
 * wait for. If a previous run left the account already on `label` (which a
 * previous run failing at exactly this step guarantees, since a no-op
 * changes nothing), nudge through a different option first so the click
 * that matters is always a genuine change.
 */
async function selectPersona(page: Page, label: string): Promise<void> {
  const combobox = personaCombobox(page);
  if (((await combobox.textContent()) ?? '').trim() === label) {
    await combobox.click();
    const decoy = page.getByRole('option').filter({ hasNotText: label }).first();
    await clickAndAwaitAuthorSave(page, () => decoy.click(), `ELITEA-2495: decoy persona nudge away from ${label}`);
  }
  await combobox.click();
  const option = page.getByRole('option').filter({ hasText: label });
  await expect(option).toHaveCount(1, { timeout: 10_000 });
  await clickAndAwaitAuthorSave(
    page,
    () => option.click(),
    'ELITEA-2495/2496/2497/2498/2499/2500: PUT /social/author',
  );
}

/**
 * Blurs the instructions field and waits for the auto-save PUT it triggers.
 * `AIPersonalityFormContent.tsx` wires `onBlur` on the section's own wrapper
 * `Box`, and React's synthetic blur bubbles, so clicking any stable sibling
 * inside that section (the "Default persona" label) blurs the textarea and
 * fires the save.
 */
async function blurInstructionsAndSave(page: Page): Promise<void> {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PUT' && r.url().includes('/social/author'), { timeout: 20_000 }),
    page.getByText('Default persona').click(),
  ]);
  expect(response.status(), await response.text()).toBeLessThan(300);
}

/*
 * ONE ACCOUNT, ONE WORKER AT A TIME (see this file's header). `--repeat-each`
 * runs this single test more than once, each instance against the identical
 * `/social/author` row, and `test.describe.configure({ mode: 'serial' })`
 * does not exclude those instances from each other — the mutex does.
 */
let releaseAuthor: (() => Promise<void>) | undefined;

test.beforeEach(async () => {
  // The mutex's own wait bound is 240s (`fixtures/mutex.ts`); queueing behind
  // up to 3 siblings under `--repeat-each` can outlast the DEFAULT 30s test
  // budget (measured: a run under host load queued long enough to hit even
  // a 120s budget here) before the test body's own work ever starts. Set
  // comfortably above the mutex's own outer bound.
  test.setTimeout(280_000);
  releaseAuthor = await acquireNamedLock('ai-personality-author');
});

test.afterEach(async () => {
  const release = releaseAuthor;
  releaseAuthor = undefined;
  await release?.();
});

test('ELITEA-2495: copying instructions from one persona to another leaves the source persona untouched', async ({ page }) => {
  /* onetest: ELITEA-2495 — copy/paste between two personas' own instruction slots: the source keeps its text, the destination gets the pasted text, and each round-trips through a reload. #929 fixed: `summary_model_project_id` now goes out as an int (`settingsProfileForm.ts`'s `projectIdField`), so every autosave PUT succeeds. */
  const MOD = process.platform === 'darwin' ? 'Meta' : 'Control';

  // This account is shared (see "ONE ACCOUNT, ONE WRITER" above): read what
  // is there before touching anything, so it can be put back afterwards.
  const author = await readAuthor(page.request);
  const originalPersona = (author.personalization?.['persona'] as string | undefined) ?? '';
  const originalInstructions = (author.personalization?.['personality_instructions'] as
    | Record<string, string>
    | undefined) ?? {};

  try {
    // The one precondition this test actually needs: Nerdy's own slot is
    // empty. A previous run of THIS test (or any interrupted one) can leave
    // it holding the pasted copy — force it, rather than assume it.
    await writePersonalization(page.request, author, originalPersona, { ...originalInstructions, nerdy: '' });

    await gotoAiPersonality(page);

    const sourceText = `${AUTOTEST_PREFIX}qa_instructions_${RUN_ID}`;
    await selectPersona(page, 'QA');
    await instructionsField(page).fill(sourceText);
    await blurInstructionsAndSave(page);

    // Select-all + copy out of the QA slot.
    await instructionsField(page).click();
    await page.keyboard.press(`${MOD}+a`);
    await page.keyboard.press(`${MOD}+c`);

    // Switch to a persona whose own slot starts empty, and paste into it.
    await selectPersona(page, 'Nerdy');
    await expect(instructionsField(page)).toHaveValue('');
    await instructionsField(page).click();
    await page.keyboard.press(`${MOD}+v`);
    await expect(instructionsField(page)).toHaveValue(sourceText);
    await blurInstructionsAndSave(page);

    // Reload and verify both slots independently: QA is unchanged, Nerdy now
    // carries the pasted copy.
    await gotoAiPersonality(page);
    await selectPersona(page, 'QA');
    await expect(instructionsField(page)).toHaveValue(sourceText);
    await selectPersona(page, 'Nerdy');
    await expect(instructionsField(page)).toHaveValue(sourceText);
  } finally {
    // Restored whatever happened above — this test does not own the account.
    await writePersonalization(page.request, author, originalPersona, originalInstructions);
  }
});
