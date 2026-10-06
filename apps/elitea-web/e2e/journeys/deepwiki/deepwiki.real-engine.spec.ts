import { expect, test } from '@playwright/test';
import { SEEDED, listKeys, openDeepWiki, readObject, replaceEditorText } from './helpers';

/**
 * DWIKI-014 — the REAL analysis engine, end to end through the product
 * (ADR-0023 acceptance; ADR-0026 for the native engine).
 *
 * Runs in the `deepwiki-real-engine` project only, against the standalone
 * stack scripts/deepwiki-real-engine.sh brings up, with the deterministic
 * LLM stub behind the gateway. Two engines (E2E_REAL_ENGINE_KIND):
 *
 *   legacy (default) — the Python `-engine` image behind the Go host, a git
 *     daemon serving the seeded repository `acme/e2e-generated`;
 *   native — the Rust-native engine, cloning a real public repository from
 *     github.com (E2E_REAL_ENGINE_REPOSITORY / _BRANCH; the runner script
 *     resolved the branch head and checked it against the pin, and passes
 *     it in E2E_REAL_ENGINE_COMMIT).
 *
 * Nothing is canned on the wiki side: the engine clones, indexes, plans a
 * structure, writes pages, and the host composes and uploads.
 *
 * What is asserted is the pipeline, not the prose (the stub's page text is
 * canned): the run completes, a manifest and at least one page land under
 * the engine's own wiki id, the page is markdown the browser renders, and
 * the wiki chat answers over the generated index. On the native engine the
 * run must also prove the wiki is the analysed repository's: the manifest
 * names that repository, branch and commit, the title is derived from the
 * repository name, and the structure is the cluster planner's over the
 * repository's own code graph (pages named from its symbols), not the
 * stub's canned structure.
 */
const NATIVE = process.env.E2E_REAL_ENGINE_KIND === 'native';
// `||`, not `??`: chat-stream-e2e.sh forwards a variable by name, and an
// empty value means "unset" here.
const REPOSITORY = process.env.E2E_REAL_ENGINE_REPOSITORY || SEEDED.mutable.repository;
const BRANCH = process.env.E2E_REAL_ENGINE_BRANCH || 'main';
const EXPECTED_COMMIT = (process.env.E2E_REAL_ENGINE_COMMIT || '').toLowerCase();
const CODE_TOOLKIT = Number(process.env.E2E_REAL_ENGINE_CODE_TOOLKIT || '9010');
// A real repository is minutes of clone, parse and embedding more than the
// sample corpus.
const GENERATION_TIMEOUT = (NATIVE ? 30 : 15) * 60 * 1000;
const TOOLKIT_PATH = `/app/deepwiki/${SEEDED.mutable.toolkitId}`;
// A wiki of a real repository can hold more objects than the default page.
const KEY_LIMIT = 1000;

/**
 * `normalize_wiki_id(owner/repo:branch)` — services/elitea-deepwiki-engine
 * src/wiki/compose.rs: lower-case, every character outside [a-z0-9-] a
 * hyphen, runs of hyphens collapsed, trimmed; `owner--repo--branch`.
 */
function wikiIdOf(repository: string, branch: string): string {
  const part = (text: string) =>
    text
      .toLowerCase()
      .replace(/[^a-z0-9-]/g, '-')
      .replace(/-+/g, '-')
      .replace(/^-+|-+$/g, '');
  const segments = repository
    .split('/')
    .map(part)
    .filter((segment) => segment.length > 0);
  return `${segments.join('--')}--${part(branch)}`;
}

const WIKI_ID = wikiIdOf(REPOSITORY, BRANCH);

/**
 * The models the engine asks the platform for. The seed names `gpt-4o-mini`
 * (a real-world default the fixture runner never reads); this stack serves
 * the mock's models, seeded into the toolkit's project by
 * scripts/deepwiki-real-engine.sh (SEED_EXTRA_PROJECTS). They are written
 * through the product's own settings panel, the way an operator points a
 * toolkit at a model — `llm_model` reaches the facade as the chat model and
 * `embedding_model` reaches the engine through the host.
 */
const LLM_MODEL = process.env.E2E_REAL_ENGINE_LLM_MODEL || 'vllm/E2E-MOCK-MODEL';
const EMBEDDING_MODEL = process.env.E2E_REAL_ENGINE_EMBEDDING_MODEL || 'vllm/E2E-MOCK-EMBEDDING';

/**
 * The page file names the stub's canned structure (llm_stub.py STRUCTURE)
 * would produce. The cluster planner the web app asks for never sends the
 * structure prompt, so none of them may appear on the native run.
 */
const CANNED_PAGE_FILES = ['getting-started.md', 'note-storage.md', 'bearer-tokens.md'];

test.describe.configure({ mode: 'serial' });

test('DWIKI-014: the real engine generates a wiki that lands and renders', async ({ page }) => {
  test.setTimeout(GENERATION_TIMEOUT + 60_000);
  if (NATIVE) {
    expect(EXPECTED_COMMIT, 'the runner script passes the commit it resolved').toMatch(/^[0-9a-f]{40}$/);
  } else {
    expect(WIKI_ID).toBe(SEEDED.mutable.wikiId);
  }
  await openDeepWiki(page, TOOLKIT_PATH);

  // Point the toolkit at the repository and the models this stack serves,
  // through the panel. First, so that the delete below removes the wiki of
  // THIS repository.
  await page.getByTestId('wiki-settings-toggle').click();
  await expect(page.getByTestId('wiki-settings-panel')).toBeVisible();
  await replaceEditorText(
    page,
    JSON.stringify({
      repository: REPOSITORY,
      branch: BRANCH,
      llm_model: LLM_MODEL,
      embedding_model: EMBEDDING_MODEL,
      code_toolkit: CODE_TOOLKIT,
    }),
  );
  await expect(page.getByTestId('wiki-settings-save')).toBeEnabled();
  await page.getByTestId('wiki-settings-save').click();
  await expect(page.getByTestId('wiki-settings-saved')).toBeVisible({
    timeout: 15_000,
  });
  await page.reload({ waitUntil: 'networkidle' });

  // A wiki left by an earlier run (a kept stack) is deleted through the
  // product first, so what is asserted below is THIS run's output only.
  if ((await listKeys(page, WIKI_ID, KEY_LIMIT)).length > 0) {
    await page.getByTestId('wiki-delete').click();
    await page
      .getByRole('dialog')
      .getByRole('button', { name: /delete/i })
      .click();
    await expect(page.getByText(/No wiki has been generated/i)).toBeVisible({
      timeout: 30_000,
    });
    expect(await listKeys(page, WIKI_ID, KEY_LIMIT)).toEqual([]);
  }

  await page.getByTestId('wiki-generate').click();
  await expect(page.getByTestId('wiki-generation-status')).toHaveAttribute('data-status', 'running', {
    timeout: 30_000,
  });
  // Poll the status so a FAILED run fails the test at once, with the log,
  // instead of waiting out the whole generation budget.
  await expect
    .poll(async () => page.getByTestId('wiki-generation-status').getAttribute('data-status'), {
      timeout: GENERATION_TIMEOUT,
      intervals: [5_000],
      message: 'the generation must complete',
    })
    .not.toMatch(/^(running|queued|pending)$/);
  const status = await page.getByTestId('wiki-generation-status').getAttribute('data-status');
  const log = await page.getByTestId('wiki-generation-log').innerText();
  expect(status, `generation ended in '${status}':\n${log}`).toBe('completed');
  expect(log, 'the host uploaded what the engine produced').toMatch(/Uploaded \d+ wiki objects/);

  const keys = await listKeys(page, WIKI_ID, KEY_LIMIT);
  const manifests = keys.filter((key) => /\/wiki_manifest_[^/]+\.json$/.test(key));
  const pages = keys.filter((key) => key.includes('/wiki_pages/') && key.endsWith('.md'));
  expect(manifests, 'exactly one manifest for the generated version').toHaveLength(1);
  expect(pages.length, 'the engine wrote at least one page').toBeGreaterThan(0);
  expect(keys).toContain(`${WIKI_ID}/repository_context.txt`);

  const manifest = await readObject(page, manifests[0] as string);
  expect(manifest.status).toBe(200);
  const parsed = JSON.parse(manifest.text) as {
    wiki_id?: string;
    wiki_title?: string;
    repository?: string;
    branch?: string;
    commit_hash?: string | null;
    pages?: string[];
  };
  expect(parsed.wiki_id).toBe(WIKI_ID);
  expect(parsed.pages?.length).toBe(pages.length);

  const first = await readObject(page, pages[0] as string);
  expect(first.status).toBe(200);
  expect(first.text.trim().length, 'a page carries content, not an empty file').toBeGreaterThan(20);

  // The page the browser opens below: the first page on the legacy run (as
  // before), the wiki's README index on the native run, whose heading is
  // the title derived from the analysed repository.
  let openKey = pages[0] as string;
  let wikiTitle = '';

  if (NATIVE) {
    // The wiki is the analysed repository's, at the commit the runner
    // resolved (and, by default, checked against the pin).
    expect(parsed.repository?.toLowerCase(), 'the manifest names the analysed repository').toBe(
      REPOSITORY.toLowerCase(),
    );
    expect(parsed.branch).toBe(BRANCH);
    const commit = (parsed.commit_hash ?? '').toLowerCase();
    expect(commit, 'the manifest names the commit that was cloned').toMatch(/^[0-9a-f]{7,40}$/);
    expect(EXPECTED_COMMIT.startsWith(commit), `manifest commit ${commit} vs resolved ${EXPECTED_COMMIT}`).toBe(true);

    // The cluster planner titles a wiki after the repository name
    // (`derive_wiki_title`); the stub's canned structure would title it
    // `notes-service`.
    wikiTitle = `${REPOSITORY.split('/').pop() ?? REPOSITORY} — Technical Documentation`;
    expect(parsed.wiki_title).toBe(wikiTitle);

    const structures = keys.filter(
      (key) => key.startsWith(`${WIKI_ID}/analysis/`) && /\/wiki_structure[^/]*\.json$/.test(key),
    );
    expect(structures, 'the structure artifact landed').toHaveLength(1);
    const structureObject = await readObject(page, structures[0] as string);
    expect(structureObject.status).toBe(200);
    const structure = JSON.parse(structureObject.text) as {
      wiki_title?: string;
      sections?: { section_name?: string; pages?: { page_name?: string }[] }[];
    };
    expect(structure.wiki_title).toBe(wikiTitle);
    const pageFiles = (structure.sections ?? []).flatMap((section) =>
      (section.pages ?? []).map((entry) => entry.page_name ?? ''),
    );
    expect(pageFiles.length, 'the structure lists the generated pages').toBeGreaterThan(0);
    // Every structure page is a page object that landed.
    for (const file of pageFiles) {
      expect(
        pages.some((key) => key.endsWith(`/${file}`)),
        `structure page ${file} landed`,
      ).toBe(true);
    }
    // Pages named from the repository's own symbols: the stub names a
    // cluster page "Working with <symbol> and <symbol>" from the symbols the
    // engine extracted and put in the naming prompt.
    expect(
      pageFiles.some((file) => /^working-with-[a-z0-9_]/.test(file)),
      `a page named from the repository's symbols, in: ${pageFiles.join(', ')}`,
    ).toBe(true);
    for (const canned of CANNED_PAGE_FILES) {
      expect(pageFiles, 'no page of the stub canned structure').not.toContain(canned);
    }

    const readmeKey = `${WIKI_ID}/wiki_pages/README.md`;
    expect(pages).toContain(readmeKey);
    const readme = await readObject(page, readmeKey);
    expect(readme.status).toBe(200);
    expect(readme.text).toContain(`# ${wikiTitle}`);
    openKey = readmeKey;
  }

  // The browser reads what landed: the generated wiki is listed and the
  // page renders as markdown.
  await page.reload({ waitUntil: 'domcontentloaded' });
  // The list shows each page under its file name; the manifest entry (the
  // engine's absolute key) is the secondary line.
  const openLabel = (openKey.split('/').pop() ?? '').replace(/\.md$/i, '');
  const entry = page.getByRole('button', { name: new RegExp(`^${openLabel}\\b`) }).first();
  await expect(entry).toBeVisible({ timeout: 30_000 });
  await entry.click();
  await expect(page.getByTestId('wiki-page-error')).toHaveCount(0);
  await expect(page.getByTestId('wiki-page-content')).toBeVisible({
    timeout: 30_000,
  });
  await expect(page.getByTestId('wiki-page-content').locator('h1, h2, p').first()).toBeVisible();
  if (NATIVE) {
    await expect(page.getByTestId('wiki-page-content')).toContainText(wikiTitle);
  }
});

test('DWIKI-014b: the wiki chat answers over the generated index', async ({ page }) => {
  test.setTimeout(5 * 60 * 1000);
  await openDeepWiki(page, TOOLKIT_PATH);
  await page.getByRole('button', { name: 'Ask about this repository' }).click();
  const drawer = page.getByTestId('wiki-chat-drawer');
  await expect(drawer).toBeVisible();
  await drawer.getByPlaceholder('Ask about this repository').fill('What does this repository do?');
  await drawer.getByRole('button', { name: 'Send' }).click();
  // The stub's answer is canned; what is proven is that the engine retrieved
  // over the index it built and the answer streamed back through the host.
  // On the native engine the ask resolves the wiki the first test published
  // for the analysed repository (resolve_wiki over PostgreSQL); a missing
  // index is an error in the drawer, not an answer.
  await expect(drawer.getByTestId('wiki-chat-answer').last()).toContainText(/\S{10,}/, { timeout: 4 * 60 * 1000 });
  await expect(drawer.getByTestId('wiki-chat-error')).toHaveCount(0);
});
