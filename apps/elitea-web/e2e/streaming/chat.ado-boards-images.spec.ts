/**
 * The ADO boards toolkit's IMAGE CACHE — the onetest `toolkits-credentials`
 * cases ELITEA-2592..2596 and 2603 (elitea_issues#5844), wave 4 package W1.
 *
 * ── Why these cases had nowhere to run, and what changed ────────────────────
 * Wave 3 ledgered all nineteen ADO image cases LIVE-ONLY: they are about what
 * the toolkit FETCHED from Azure DevOps and how often it then asked a vision
 * model, and this repo had neither an ADO-shaped backend in `deploy/` nor an
 * `e2e/live` ADO lane. `deploy/ado-mock/` is that backend now — the routes the
 * `azure-devops` python client actually addresses (it asks by LOCATION GUID, so
 * the mock's `OPTIONS /_apis` catalogue is the route contract), a PAT it
 * refuses to answer without, and seeded work items whose comments carry the
 * same attachment more than once.
 *
 * ── The mechanism these five cases turn on ─────────────────────────────────
 * `AzureDevOpsApiWrapper._image_cache` is an `ImageDescriptionCache`: an
 * in-memory LRU held by the WRAPPER INSTANCE, keyed on
 * `md5(image bytes) [+ md5(prompt)]` and NOT on the file name or the URL
 * (`elitea_sdk/runtime/langchain/document_loaders/image_cache.py`). So:
 *   * the same bytes twice in one execution are described once (2592, 2603);
 *   * the same bytes reached through `get_comments` and through
 *     `get_work_item` share one cache, because one wrapper serves both (2593);
 *   * a different prompt is a different key, so it describes again (2594);
 *   * the wrapper is rebuilt per execution, so a NEW TURN describes again —
 *     the cache is in memory only and nothing persists it (2596);
 *   * with `process_images` off nothing is fetched or described at all (2595).
 *
 * ── Where every count is read ──────────────────────────────────────────────
 * THE DOWNLOAD COUNT AND THE MODEL COUNT ARE DIFFERENT NUMBERS, and the
 * difference is the whole subject. The SDK re-downloads the attachment for
 * every occurrence and caches only the DESCRIPTION, so "three downloads, one
 * vision call" is the passing shape rather than a contradiction. Downloads are
 * counted on the fake ADO's `/__state`; vision calls are counted in the mock
 * LLM's request journal, whose `images[]` records the md5 of the bytes that
 * actually left the platform. Neither can be faked by the answer text, which
 * is written by the same model that was shown the picture.
 *
 * ── Why the python leg ────────────────────────────────────────────────────
 * See `ADO_LEG_REASON`: the families are SDK-only, and the image pipeline under
 * test is the SDK's loader stack.
 */
import { expect, test } from '@playwright/test';

import { clearMockLlmJournal, readMockLlmJournal } from '../fixtures/api';
import {
  ADO_LEG_REASON,
  createAdoAgent,
  onPythonLeg,
  readAdoJournal,
  readAdoState,
  resetAdo,
  runPrompt,
  shownImages,
} from '../fixtures/adoMock';

test.skip(!onPythonLeg(), ADO_LEG_REASON);

/** The seeded work item whose one image appears in three of its five comments. */
const REPEATED = 101;
const SHORT_PROMPT = 'short caption';
const DETAILED_PROMPT = 'detailed accessibility description';

/** Enough headroom that `limit` never truncates a seeded comment list (it defaults to 5). */
const BOARDS_SETTINGS = { limit: 50 } as const;
const BOARDS_TOOLS = ['get_comments', 'get_work_item', 'get_image_by_url'] as const;

/** One `[[mock:call_tool …]]` marker. Several in one prompt are ONE turn with several calls. */
function call(tool: string, args: Readonly<Record<string, unknown>>): string {
  return `[[mock:call_tool ${tool} ${JSON.stringify(args)}]]`;
}

/** Every image the MODEL was shown during this test, in request order. */
async function describedImages(page: import('@playwright/test').Page, projectId: string) {
  return shownImages(await readMockLlmJournal(page, projectId));
}

test.beforeEach(async ({ page }) => {
  await resetAdo(page);
  await clearMockLlmJournal(page);
});

/* onetest: ELITEA-2592, ELITEA-2603 — one image repeated across three comments is described ONCE, and a
 * second `get_comments` in the same execution asks the model nothing further, while every occurrence is
 * still downloaded and still carries a description. */
test('a repeated image is described once per execution, however many times it is read', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'repeat', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const state = await readAdoState(page);
    const screenshot = state.attachments['img-1'];
    expect(screenshot, 'the fake ADO must seed the repeated screenshot').toBeTruthy();

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} ${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read the comments twice`,
    );

    const images = await describedImages(page, agent.projectId);
    expect(
      images.map((image) => image.md5),
      'the identical bytes must reach the vision model exactly once — the cache is keyed on them',
    ).toEqual([screenshot!.md5]);
    expect(images[0]?.format, 'the bytes the model saw must still be the seeded PNG').toBe('PNG');
    expect(images[0]?.bytes, 'the model must be shown the file, not a placeholder').toBe(screenshot!.size);

    // The DOWNLOAD side, which is deliberately NOT the same number and not the
    // occurrence count either. The boards path dedupes on the URL LIST before
    // it fetches anything (`_describe_image_urls` opens with
    // `dict.fromkeys(urls)`), so the three occurrences inside one call collapse
    // to one download — and the second CALL downloads again, because the URL
    // list is rebuilt per call while only the DESCRIPTION survives in the
    // cache. Two is therefore the exact shape: one fetch per call, one model
    // call for both. (The wiki path has no such list — see
    // `chat.ado-wiki-images.spec.ts`, where two references cost two reads.)
    const after = await readAdoState(page);
    expect(
      after.downloads['img-1'] ?? 0,
      'one fetch per call — the occurrences are deduplicated before the download, the DESCRIPTION across calls',
    ).toBe(2);

    // And each occurrence really did come back described: the mock's vision
    // reply names the digest, so the answer carries it once per occurrence.
    const digest = screenshot!.md5.slice(0, 12);
    const occurrences = answer.split(digest).length - 1;
    expect(
      occurrences,
      `every one of the three occurrences must carry the cached description (md5 ${digest})`,
    ).toBeGreaterThanOrEqual(3);

    const journal = await readAdoJournal(page);
    expect(
      journal.some((entry) => entry.attachment === 'img-1' && entry.auth_ok && entry.status === 200),
      'the attachment must have been downloaded with the toolkit credential',
    ).toBe(true);
    expect(
      journal.filter((entry) => entry.status === 401),
      'nothing may have reached the fake ADO without the PAT',
    ).toEqual([]);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2593 — the same bytes reached through get_comments and through get_work_item share one
 * cache, because one wrapper instance serves both entry points. */
test('the description cache is shared between the comment and work-item entry points', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'entrypoints', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} ${call('get_work_item', {
        id: REPEATED,
        expand: 'relations',
        parse_attachments: true,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read the comments and then the work item`,
    );

    const images = await describedImages(page, agent.projectId);
    expect(
      images.map((image) => image.md5),
      'the work item path must reuse the description the comment path generated',
    ).toEqual([screenshot!.md5]);

    // Both results are in the one answer (the mock quotes each tool result), so
    // the shared description has to appear on both sides of it.
    const digest = screenshot!.md5.slice(0, 12);
    expect(answer, 'the comment result must carry the description').toContain(digest);
    expect(
      answer.split(digest).length - 1,
      'the work item result must carry the SAME description, not a second one',
    ).toBeGreaterThanOrEqual(4);
    expect(
      answer,
      'the work item read must have happened at all — its relation names the attached file',
    ).toContain('AttachedFile');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2594 — the prompt is part of the cache key, so the same image with new instructions is
 * described again rather than answered from the first prompt's description. */
test('a different image_description_prompt describes the same image again', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'promptkey', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];

    await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} ${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: DETAILED_PROMPT,
      })} caption it twice, differently`,
    );

    const entries = await readMockLlmJournal(page, agent.projectId);
    const visionRequests = entries.filter((entry) => shownImages([entry]).length > 0);
    expect(
      visionRequests.length,
      'a new prompt is a new cache key — the image must reach the model a second time',
    ).toBe(2);
    expect(
      shownImages(visionRequests).map((image) => image.md5),
      'both calls must be about the SAME bytes; only the instructions changed',
    ).toEqual([screenshot!.md5, screenshot!.md5]);

    // The instructions themselves, off the wire. `history` is the mock's record
    // of what the request carried, and a vision request's first text part is
    // the prompt the toolkit was told to use.
    const prompts = visionRequests.map((entry) => entry.history.map((row) => row.text).join(' '));
    expect(prompts[0], 'the first call must carry the first prompt').toContain(SHORT_PROMPT);
    expect(prompts[1], 'the second call must carry the NEW prompt').toContain(DETAILED_PROMPT);
    expect(prompts[1], 'the second call must not be a replay of the first').not.toBe(prompts[0]);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2595, ELITEA-2606 — with process_images off (explicitly, and by omission) nothing is
 * downloaded, nothing is described, and the comment comes back in its pre-feature shape. */
test('process_images off fetches no attachment and asks the model nothing', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'off', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', { work_item_id: REPEATED, process_images: false })} ${call('get_comments', {
        work_item_id: REPEATED,
      })} read the comments with images off and then with the default`,
    );

    expect(
      await describedImages(page, agent.projectId),
      'the off-path must make no vision call at all — in either the explicit or the default form',
    ).toEqual([]);

    const state = await readAdoState(page);
    expect(
      Object.keys(state.downloads),
      'nothing may be downloaded from ADO when image processing is off',
    ).toEqual([]);

    expect(answer, 'the raw comment markup must still be returned').toContain('<img');
    expect(
      answer,
      'no description may be injected into a comment when the feature is off',
    ).not.toContain('image-description');
    expect(
      answer,
      'the original comment text must be untouched',
    ).toContain('first sighting');
    // `get_comments` forces `$expand=renderedText` ONLY when it is going to
    // scan for images. Off, it leaves the caller's expand alone, which is the
    // pre-feature response shape this case is about.
    const journal = await readAdoJournal(page);
    const commentReads = journal.filter((entry) => entry.expand !== undefined);
    expect(commentReads.length, 'both comment reads must have reached ADO').toBe(2);
    expect(
      commentReads.map((entry) => entry.expand),
      'the off-path must not ask ADO to render the comment HTML',
    ).toEqual(['none', 'none']);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2596 — the cache lives in the execution and nowhere else: a second TURN describes the
 * same image again, which is what "in-memory only, not persisted" means here. */
test('a new turn re-describes the image — the cache does not outlive the execution', async ({ page }) => {
  test.setTimeout(420_000);
  const agent = await createAdoAgent(page, 'session', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];
    const prompt = `${call('get_comments', {
      work_item_id: REPEATED,
      process_images: true,
      image_description_prompt: SHORT_PROMPT,
    })} read the comments`;

    await runPrompt(page, agent, prompt);
    const afterFirst = await describedImages(page, agent.projectId);
    expect(
      afterFirst.map((image) => image.md5),
      'the first turn must describe the image once',
    ).toEqual([screenshot!.md5]);

    // A SECOND turn in the same conversation, with the same agent and the same
    // toolkit row. Everything persistent is identical; only the wrapper — and
    // with it the cache — is new.
    await runPrompt(page, agent, prompt);
    const afterSecond = await describedImages(page, agent.projectId);
    expect(
      afterSecond.map((image) => image.md5),
      'the second turn must ask the model again: nothing persists the description',
    ).toEqual([screenshot!.md5, screenshot!.md5]);
  } finally {
    await agent.dispose();
  }
});
