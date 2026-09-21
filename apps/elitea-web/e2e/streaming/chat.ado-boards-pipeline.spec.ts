/**
 * The ADO boards toolkit's IMAGE PIPELINE — the onetest `toolkits-credentials`
 * cases ELITEA-2599..2602, 2604, 2605, 2607..2610 (elitea_issues#5889),
 * wave 4 package W1.
 *
 * ── The pipeline, in the order the cases walk it ───────────────────────────
 *   1. CONFIGURE — `process_images` is an argument of the tool, not a field of
 *      the toolkit; `get_comments` defaults it to false (2599, 2610).
 *   2. DETECT — `_embed_comment_image_descriptions` scans `renderedText`/`text`
 *      for `<img src>` and for `![](…)`, and `get_comments` forces
 *      `$expand=renderedText` so the HTML is there to scan (2600).
 *   3. DOWNLOAD + VALIDATE — `_fetch_validated_image` gates on the FILE NAME's
 *      extension, a 5 MB cap and a readable pixel size, and turns every failure
 *      into an `[image unavailable: <reason>]` note beside the image rather
 *      than an exception (2601, 2607, 2608).
 *   4. DESCRIBE — `parse_file_content` → `EliteAImageLoader` → the vision model,
 *      with the attachment's own bytes on the wire (2602).
 *   5. RETURN — the description is written as an `image-description` attribute
 *      on the `<img>` (or an `[image-description: …]` line under the markdown),
 *      leaving the comment text and the `src` URL untouched (2604, 2605).
 *   6. DEDUPLICATE — `dict.fromkeys(urls)` before the fetch, and the byte cache
 *      behind it (2609).
 *
 * ── Where the assertions are read ──────────────────────────────────────────
 * The fake ADO's `/__state` and `/__journal` for what was fetched and with
 * which credential; the mock LLM's journal (`images[]` = `{md5, format,
 * bytes}`) for what reached the model; the STORED answer for the response
 * shape, because the mock quotes each tool result verbatim on the resume.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { API_BASE, clearMockLlmJournal, readMockLlmJournal } from '../fixtures/api';
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

/** The seeded work items, by what each one is FOR (`deploy/ado-mock/server.py`). */
const REPEATED = 101;
const FORMATS = 102;
const MIXED = 103;
const BROKEN = 104;
const MANY = 105;

const SHORT_PROMPT = 'short caption';
const BOARDS_SETTINGS = { limit: 50 } as const;
const BOARDS_TOOLS = ['get_comments', 'get_work_item', 'get_image_by_url'] as const;

function call(tool: string, args: Readonly<Record<string, unknown>>): string {
  return `[[mock:call_tool ${tool} ${JSON.stringify(args)}]]`;
}

async function describedImages(page: Page, projectId: string) {
  return shownImages(await readMockLlmJournal(page, projectId));
}

/**
 * Every `[image unavailable: …]` note the answer carries, in order.
 *
 * Backslashes are stripped before the note is returned. The stored answer is a
 * tool result that was JSON-encoded on its way through the runtime, so a note
 * that reached an HTML ATTRIBUTE comes back with its apostrophes escaped
 * (`unsupported image format \'.txt\'`) while the same note written into
 * MARKDOWN text does not. Matching the reason rather than the escaping is what
 * keeps this reading the product instead of the transport.
 */
function unavailableNotes(answer: string): readonly string[] {
  return [...answer.matchAll(/\[image unavailable: ([^\]"]*)/g)].map((match) =>
    (match[1] ?? '').replaceAll('\\', ''),
  );
}

test.beforeEach(async ({ page }) => {
  await resetAdo(page);
  await clearMockLlmJournal(page);
});

/* onetest: ELITEA-2599 — `process_images` is offered on get_comments with default false, the SERVED schema
 * says so, and a call that omits it behaves as false while a call that sets it behaves as true. */
test('get_comments offers process_images and defaults it to false', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'param', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    // ── The SERVED schema, not the SDK source ──────────────────────────────
    // `GET /elitea_core/toolkits/prompt_lib/{project}` is what the toolkit form
    // draws from (`admin.guardrails.spec.ts` reads the same document), so a
    // parameter that exists only in the SDK and never reaches the client fails
    // here rather than passing on the strength of the snapshot file.
    const served = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${agent.projectId}`);
    expect(served.ok(), `the type catalogue must be readable: ${served.status()}`).toBe(true);
    const catalogue = (await served.json()) as Record<
      string,
      {
        properties?: {
          selected_tools?: {
            args_schemas?: Record<string, { properties?: Record<string, { default?: unknown }> }>;
          };
        };
      }
    >;
    const boards = catalogue['ado_boards'];
    expect(boards, 'the catalogue served to the client must offer ado_boards').toBeTruthy();
    const argsSchemas = boards?.properties?.selected_tools?.args_schemas ?? {};
    const getComments = argsSchemas['get_comments'];
    expect(getComments, 'get_comments must carry an argument schema in the served catalogue').toBeTruthy();
    const processImages = getComments?.properties?.['process_images'];
    expect(processImages, 'the served get_comments schema must offer process_images').toBeTruthy();
    expect(processImages?.default, 'the served default must be false').toBe(false);
    // The SAME parameter defaults the other way on `get_work_item`, which no
    // form surfaces. Pinned here because it is the reason "the ADO toolkit does
    // not describe images by default" is only half true (#985).
    expect(
      argsSchemas['get_work_item']?.properties?.['process_images']?.default,
      'get_work_item defaults process_images to TRUE — the asymmetry is load-bearing, not a typo',
    ).toBe(true);

    // ── And the behaviour the schema promises ──────────────────────────────
    await runPrompt(page, agent, `${call('get_comments', { work_item_id: REPEATED })} read the comments`);
    expect(
      await describedImages(page, agent.projectId),
      'omitting the parameter must behave as false',
    ).toEqual([]);

    await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} now read them with images on`,
    );
    const screenshot = (await readAdoState(page)).attachments['img-1'];
    expect(
      (await describedImages(page, agent.projectId)).map((image) => image.md5),
      'setting the parameter must turn the pipeline on for that call alone',
    ).toEqual([screenshot!.md5]);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2600 — detection: an HTML <img> and a markdown image are both found, a non-image
 * attachment and a malformed URL are filtered out with a note instead of an exception, an absent
 * attachment does not stop the ones beside it, and a comment with no image is left alone. */
test('images are detected in HTML and markdown, and non-images are filtered out', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'detect', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const state = await readAdoState(page);
    const png = state.attachments['img-1'];
    const gif = state.attachments['img-3'];

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: MIXED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read every comment`,
    );

    const images = await describedImages(page, agent.projectId);
    expect(
      images.map((image) => image.md5).sort(),
      'exactly the two REAL images must reach the model — the HTML one and the markdown one',
    ).toEqual([png!.md5, gif!.md5].sort());
    expect(
      images.map((image) => image.format).sort(),
      'the markdown image is the GIF and the HTML one the PNG',
    ).toEqual(['GIF', 'PNG']);

    // The non-images: named as unusable, never described, never returned as an
    // exception that would have cost the whole call.
    const notes = unavailableNotes(answer);
    expect(
      notes.some((note) => note.includes("unsupported image format '.pdf'")),
      `the PDF in image markdown must be filtered out by NAME, not described. Notes: ${JSON.stringify(notes)}`,
    ).toBe(true);
    expect(
      notes.some((note) => note.includes("unsupported image format '.txt'")),
      `a .txt attachment inside an <img> must be filtered out too. Notes: ${JSON.stringify(notes)}`,
    ).toBe(true);
    expect(
      notes.some((note) => note.includes('bad')),
      `the malformed URL must be noted rather than thrown. Notes: ${JSON.stringify(notes)}`,
    ).toBe(true);

    // The ABSENT attachment: 404 at ADO, a note in the comment, and the two
    // real images still described — that is "one failure does not block the
    // others" at the detection stage.
    const journal = await readAdoJournal(page);
    expect(
      journal.some((entry) => entry.status === 404 && entry.path.includes('/attachments/')),
      'the absent attachment must really have been asked for and really have 404ed',
    ).toBe(true);

    // The text-only comment: untouched, and the surrounding prose of every
    // comment preserved.
    expect(answer, 'a comment with no image must come back as it was').toContain('plain text only');
    expect(answer, 'the original comment prose must survive the rewrite').toContain('html tag');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2601, ELITEA-2607 — every download/validation failure is reported in place and none of
 * them stops the working image in the same batch: corrupt bytes, zero bytes, over the size cap, and a
 * forbidden attachment each get their own reason. */
test('a broken attachment is reported in place and never blocks the others', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'broken', {
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
        work_item_id: BROKEN,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read the broken ones`,
    );

    const notes = unavailableNotes(answer);
    // FOUR, not two: the SDK patches the description into BOTH comment fields
    // it scanned — `rendered_text` and `text` — so every note appears once per
    // field. Two undecodable images x two fields. Pinned at the exact number
    // rather than `>= 2` because the doubling is the response shape, and a
    // change to it is something this file should notice.
    expect(
      notes.filter((note) => note.includes('could not read image dimensions')).length,
      `corrupt bytes and zero bytes must each be named as undecodable, in both comment fields. Notes: ${JSON.stringify(notes)}`,
    ).toBe(4);
    expect(
      notes.some((note) => note.includes('exceeds the 5 MB processing limit')),
      'the oversized attachment must be refused by the cap, with the cap in the message',
    ).toBe(true);
    expect(
      notes.some((note) => note.includes('could not download the attachment')),
      'the forbidden attachment must be reported as a download failure',
    ).toBe(true);

    // PARTIAL SUCCESS is the point: the good image in the same comment list is
    // still described, and it is the ONLY thing the model was asked about.
    expect(
      (await describedImages(page, agent.projectId)).map((image) => image.md5),
      'four failures must not stop the fifth image from being described',
    ).toEqual([screenshot!.md5]);
    expect(answer, 'the working image must carry its description').toContain(screenshot!.md5.slice(0, 12));

    // Server-side proof of the refusal, with the credential present: a 403 that
    // carried no PAT would be a harness failure wearing the same shape.
    const journal = await readAdoJournal(page);
    const forbidden = journal.filter((entry) => entry.attachment === 'bad-5');
    expect(forbidden.length, 'the forbidden attachment must have been asked for').toBeGreaterThan(0);
    expect(forbidden[0]?.status, 'the fake ADO must have refused it with 403').toBe(403);
    expect(forbidden[0]?.auth_ok, 'and it must have been asked for WITH the toolkit credential').toBe(true);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2602, ELITEA-2604, ELITEA-2605 — the attachment's own bytes reach the vision model, and
 * the description comes back INSIDE the comment beside the preserved text and the preserved source URL,
 * where the agent then uses it in its own answer. */
test('the bytes reach the model and the description lands beside the untouched comment', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'structure', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const state = await readAdoState(page);
    const screenshot = state.attachments['img-1'];

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} tell me what the screenshot shows`,
    );

    // ── The wire ───────────────────────────────────────────────────────────
    const entries = await readMockLlmJournal(page, agent.projectId);
    const vision = entries.filter((entry) => shownImages([entry]).length > 0);
    expect(vision.length, 'exactly one vision request must have been made').toBe(1);
    const image = shownImages(vision)[0];
    expect(image?.md5, 'the model must be shown the seeded attachment, byte for byte').toBe(screenshot!.md5);
    expect(image?.bytes, 'and all of it — the size must match what ADO served').toBe(screenshot!.size);
    expect(image?.format, 'identified from the magic number, not from the file name').toBe('PNG');
    expect(
      vision[0]?.history.map((row) => row.text).join(' '),
      'the configured prompt must travel with the picture',
    ).toContain(SHORT_PROMPT);

    // ── The response structure ─────────────────────────────────────────────
    expect(answer, 'the original comment text must be preserved, not replaced').toContain('first sighting');
    expect(answer, 'the description must be attached to the image').toContain('image-description');
    expect(answer, 'the description must be the one the model produced').toContain(screenshot!.md5.slice(0, 12));
    expect(
      answer,
      'the source URL must stay intact so the model can follow up with get_image_by_url',
    ).toContain(`/_apis/wit/attachments/${screenshot!.id}`);
    expect(answer, 'the file name must remain readable for traceability').toContain(screenshot!.name);
    // A comment that held no image must be untouched by the rewrite.
    expect(answer, 'the imageless comments must come back unchanged').toContain('a comment with no image at all');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2608 — every format the SDK lists as directly LLM-supported reaches the model AS ITSELF,
 * identified from the magic number rather than from the URL's extension. */
test('PNG, JPEG, GIF and WebP each reach the model in their own format', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'formats', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const state = await readAdoState(page);
    const expected = ['img-1', 'img-2', 'img-3', 'img-4'].map((tag) => state.attachments[tag]!);

    await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: FORMATS,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} describe each of them`,
    );

    const images = await describedImages(page, agent.projectId);
    expect(images.length, 'all four formats must have been described').toBe(4);
    expect(
      images.map((image) => image.md5).sort(),
      'each file must arrive byte for byte — no re-encode, no substitution',
    ).toEqual(expected.map((attachment) => attachment.md5).sort());
    expect(
      images.map((image) => image.format).sort(),
      'and each must still be its own format on the wire',
    ).toEqual(['GIF', 'JPEG', 'PNG', 'WEBP']);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2609 — twelve distinct images in ONE comment are each described once, and the thirteenth
 * reference, which repeats the first, is deduplicated BEFORE the download rather than after it. */
test('many images in one comment are each described once, and a repeat is deduplicated', async ({ page }) => {
  test.setTimeout(420_000);
  const agent = await createAdoAgent(page, 'many', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
  });
  try {
    const state = await readAdoState(page);
    const generated = Object.entries(state.attachments)
      .filter(([tag]) => tag.startsWith('gen-'))
      .map(([, attachment]) => attachment.md5)
      .sort();
    expect(generated.length, 'the seed must hold twelve distinct generated images').toBe(12);

    await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: MANY,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} describe the wall of pictures`,
    );

    const images = await describedImages(page, agent.projectId);
    expect(
      images.map((image) => image.md5).sort(),
      'twelve distinct images, twelve vision calls — the repeat adds none',
    ).toEqual(generated);
    expect(
      new Set(images.map((image) => image.md5)).size,
      'and no image may be described twice',
    ).toBe(12);

    // Deduplication happens on the URL LIST before anything is fetched
    // (`dict.fromkeys`), so the repeated reference costs no download either.
    const after = await readAdoState(page);
    expect(
      after.downloads['gen-01'],
      'the image referenced twice in the same comment must be fetched once',
    ).toBe(1);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2610 — two toolkit instances in one agent each answer to their OWN call, which is as far
 * as configuration flexibility goes today: `process_images` is a tool argument and not a toolkit setting,
 * so an instance cannot carry a default at all. */
test('two toolkit instances each honour their own call', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'instances', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    settings: BOARDS_SETTINGS,
    second: { selectedTools: [...BOARDS_TOOLS], settings: BOARDS_SETTINGS },
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];

    // Both instances offer a tool named `get_comments`; the runtime
    // disambiguates by toolkit, and a scripted marker names the function the
    // model would have named. One call with images on, one with them off, in
    // the SAME turn.
    const answer = await runPrompt(
      page,
      agent,
      `${call('get_comments', {
        work_item_id: REPEATED,
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} ${call('get_comments', { work_item_id: REPEATED, process_images: false })} read it both ways`,
    );

    expect(
      (await describedImages(page, agent.projectId)).map((image) => image.md5),
      'the call that asked for images gets them; the one that did not, does not',
    ).toEqual([screenshot!.md5]);
    expect(answer, 'the described read must carry a description').toContain('image-description');
    expect(answer, 'and the raw read must still carry the untouched markup').toContain('first sighting');
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2610 — a toolkit INSTANCE configured with process_images must apply it to every call
 * that omits the argument, so two instances can differ without the model deciding. */
test('a toolkit instance can default process_images for its own calls', async ({ page }) => {
  test.fail(
    true,
    'ELITEA-2610: product gap — process_images is a tool ARGUMENT only; the ado_boards settings schema ' +
      'has no such field, so an instance cannot default it and the model decides every call (#985)',
  );
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'instdefault', {
    type: 'ado_boards',
    selectedTools: [...BOARDS_TOOLS],
    // What the case asks for: the INSTANCE turns image processing on, and a
    // call that names no argument inherits it. The create route applies
    // catalogue defaults and drops nothing, so this key reaches the stored row
    // and is simply never read.
    settings: { ...BOARDS_SETTINGS, process_images: true, image_description_prompt: SHORT_PROMPT },
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];
    await runPrompt(page, agent, `${call('get_comments', { work_item_id: REPEATED })} read the comments`);
    expect(
      (await describedImages(page, agent.projectId)).map((image) => image.md5),
      'a call that omits the argument must inherit the instance default',
    ).toEqual([screenshot!.md5]);
  } finally {
    await agent.dispose();
  }
});
