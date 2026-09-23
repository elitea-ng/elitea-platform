/**
 * The ADO WIKI toolkit's image pass — the onetest `toolkits-credentials` case
 * ELITEA-2590 (elitea_issues#5844), with the cache and off-path claims of
 * ELITEA-2592/2595/2603/2606 asserted on the wiki side too, wave 4 package W1.
 *
 * ── Why the wiki is a separate mechanism from boards ──────────────────────
 * ADO Wiki writes EVERY attachment as image markdown — `![name](/.attachments/
 * guid.ext)` — whatever the file actually is, so a PDF and a screenshot are
 * indistinguishable by syntax. `_process_images` therefore gates on the URL's
 * EXTENSION (`_url_has_image_extension`) and, for anything that passes, fetches
 * the bytes through the wiki's GIT REPOSITORY rather than through the work-item
 * attachment API: `ReposApiWrapper(repository_id=wiki.repository_id,
 * base_branch='wikiMaster').download_file(...)`. That is three more endpoints
 * than the boards path uses (`get_wiki`, `get_repository`, `stats/branches`,
 * `items`), and the fake ADO serves all of them.
 *
 * The substitution is also different in kind: the wiki replaces the image's
 * URL with the description — `![name](<description>)` — where boards adds an
 * `image-description` attribute beside the untouched `src`. A case about "the
 * PDF markdown is left as-is" is therefore a claim about a string this file can
 * read verbatim out of the returned page.
 *
 * ── ELITEA-2591 is deliberately absent ─────────────────────────────────────
 * It asks for the fallback when image processing is requested with NO LLM
 * configured. There is no path to that state in this product: the SDK injects
 * the executing agent's model into every toolkit unconditionally
 * (`elitea_sdk/tools/__init__.py`, `settings['llm'] = llm`), and an agent
 * cannot be saved without a model. Writing a test that arranged `llm=None`
 * would be testing the fixture. Ledgered NA with that evidence rather than
 * skipped.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { clearMockLlmJournal, readMockLlmJournal } from '../fixtures/api';
import {
  ADO_LEG_REASON,
  ADO_WIKI,
  createAdoAgent,
  onPythonLeg,
  readAdoJournal,
  readAdoState,
  resetAdo,
  runPrompt,
  shownImages,
} from '../fixtures/adoMock';

test.skip(!onPythonLeg(), ADO_LEG_REASON);

const SHORT_PROMPT = 'short caption';
const WIKI_SETTINGS = { default_wiki_identifier: ADO_WIKI } as const;
const WIKI_TOOLS = ['get_wiki', 'get_wiki_page', 'get_wiki_page_by_path', 'get_wiki_page_by_id'] as const;

function call(tool: string, args: Readonly<Record<string, unknown>>): string {
  return `[[mock:call_tool ${tool} ${JSON.stringify(args)}]]`;
}

async function describedImages(page: Page, projectId: string) {
  return shownImages(await readMockLlmJournal(page, projectId));
}

/** Which `.attachments/` files the wiki's git repository was actually asked for. */
async function fetchedAttachments(page: Page): Promise<readonly string[]> {
  return (await readAdoJournal(page))
    .filter((entry) => entry.git_path !== undefined)
    .map((entry) => entry.git_path ?? '');
}

test.beforeEach(async ({ page }) => {
  await resetAdo(page);
  await clearMockLlmJournal(page);
});

/* onetest: ELITEA-2590 — a PDF wearing image markdown is left completely alone — not fetched, not
 * described, and not annotated with an error — while the real image on the same page is described. */
test('a non-image attachment in image markdown is left untouched beside a described image', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'wikipdf', {
    type: 'ado_wiki',
    selectedTools: [...WIKI_TOOLS],
    settings: WIKI_SETTINGS,
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_wiki_page_by_path', {
        page_name: '/Images',
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read the Images page`,
    );

    // ── The image half ─────────────────────────────────────────────────────
    const images = await describedImages(page, agent.projectId);
    expect(
      images.map((image) => image.md5),
      'exactly the real picture must reach the vision model',
    ).toEqual([screenshot!.md5]);
    expect(images[0]?.format, 'and it must arrive as the PNG the wiki repository holds').toBe('PNG');
    expect(
      answer,
      'the wiki path replaces the image URL with the description, so the digest must appear in its place',
    ).toContain(`![screenshot.png](`);
    expect(answer, 'and the description must be the one the model produced').toContain(
      screenshot!.md5.slice(0, 12),
    );

    // ── The PDF half, which is the case ────────────────────────────────────
    expect(
      answer,
      'the PDF markdown must be byte-identical to the page source — not described, not rewritten',
    ).toContain('![spec.pdf](/.attachments/spec.pdf)');
    expect(
      answer,
      'and no error text may be injected about it: an unusable attachment on the wiki path is SILENT',
    ).not.toContain('image unavailable');

    // ── Server-side: the PDF was never even fetched ────────────────────────
    const fetched = await fetchedAttachments(page);
    expect(fetched, 'the image must have been read out of the wiki git repository').toContain(
      '.attachments/screenshot.png',
    );
    expect(
      fetched,
      'the non-image must be filtered out BEFORE the download, by its extension',
    ).not.toContain('.attachments/spec.pdf');
    expect(
      (await readAdoJournal(page)).filter((entry) => entry.status === 401),
      'nothing may have reached the fake ADO without the PAT',
    ).toEqual([]);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2592, ELITEA-2603 — the wiki wrapper caches on the same key as boards, so one attachment
 * referenced twice on a page is described once while still being fetched for each reference. */
test('an attachment referenced twice on a page is described once', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'wikirepeat', {
    type: 'ado_wiki',
    selectedTools: [...WIKI_TOOLS],
    settings: WIKI_SETTINGS,
  });
  try {
    const screenshot = (await readAdoState(page)).attachments['img-1'];

    const answer = await runPrompt(
      page,
      agent,
      `${call('get_wiki_page_by_path', {
        page_name: '/Repeat',
        process_images: true,
        image_description_prompt: SHORT_PROMPT,
      })} read the Repeat page`,
    );

    expect(
      (await describedImages(page, agent.projectId)).map((image) => image.md5),
      'both references are the same bytes, so the model must be asked once',
    ).toEqual([screenshot!.md5]);

    const digest = screenshot!.md5.slice(0, 12);
    expect(
      answer.split(digest).length - 1,
      'and BOTH references must come back carrying that one description',
    ).toBe(2);

    // The wiki path has no URL-level dedup — each markdown reference is its own
    // item — so the repository is read twice and only the description is saved.
    const state = await readAdoState(page);
    const reads = Object.entries(state.downloads)
      .filter(([key]) => key.endsWith('.attachments/screenshot.png'))
      .map(([, count]) => count);
    expect(reads, 'the file itself is read per reference; the CACHE is what is reused').toEqual([2]);
  } finally {
    await agent.dispose();
  }
});

/* onetest: ELITEA-2595, ELITEA-2606 — process_images off on the wiki path returns the page exactly as it
 * is stored, and reads nothing out of the wiki repository at all. */
test('process_images off returns the wiki page verbatim and reads no attachment', async ({ page }) => {
  test.setTimeout(360_000);
  const agent = await createAdoAgent(page, 'wikioff', {
    type: 'ado_wiki',
    selectedTools: [...WIKI_TOOLS],
    settings: WIKI_SETTINGS,
  });
  try {
    const answer = await runPrompt(
      page,
      agent,
      `${call('get_wiki_page_by_path', { page_name: '/Images', process_images: false })} read the page raw`,
    );

    expect(
      await describedImages(page, agent.projectId),
      'the off-path must make no vision call',
    ).toEqual([]);
    expect(
      answer,
      'the image markdown must come back exactly as the page stores it',
    ).toContain('![screenshot.png](/.attachments/screenshot.png)');
    expect(answer, 'and so must the document markdown').toContain('![spec.pdf](/.attachments/spec.pdf)');
    expect(
      await fetchedAttachments(page),
      'with images off, the wiki repository must not be read at all',
    ).toEqual([]);
  } finally {
    await agent.dispose();
  }
});
