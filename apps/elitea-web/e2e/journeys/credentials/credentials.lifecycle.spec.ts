/**
 * Journey 19: create a credential → verify it persists → remove it (JRNY-019)
 *
 * Spec §8.5 acceptance (from parity/manifest/credentials.json JRNY-019).
 * Acceptance: the credential is persisted and selectable; removing it
 * invalidates dependent configuration gracefully.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE DELIBERATELY NO LONGER DOES
 *
 * The previous version had 13 escape hatches (a `test.skip`, three early
 * `return`s, five `.catch(() => false)` swallows and four `.or()` fallbacks)
 * and asserted nothing that a heading-only stub could not satisfy. The
 * decisive one was `getByTestId('config-form').or(getByRole('dialog'))
 * .or(getByRole('main'))` — `role=main` is rendered by the app shell on
 * EVERY route, so that locator matched unconditionally. Every hatch is gone;
 * nothing below is optional.
 *
 * PRODUCT GAP (do not re-add): the "…and use it in an agent" half of
 * JRNY-019 is unassertable because no credential picker is reachable in the
 * running app. `src/features/credentials/ui/CredentialsSelect.tsx` is the
 * only component that renders one, and it has no production caller — its
 * two slot consumers (`AgentEditor`'s optional `renderLlmModelSelector` and
 * `PipelineEditorParts`) are never handed a renderer by any route or page.
 * The testids the old test used for that half —`llm-selector`, `config-form`,
 * `create-application-form-panel` — do not exist in `src` at all; the first
 * two are vitest mocks in `AgentEditor.test.tsx`, the third is a doc-comment
 * string in `CreateApplication.tsx`. That half is therefore deleted rather
 * than left behind a `test.skip()` that reads as coverage.
 * ─────────────────────────────────────────────────────────────────────────
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import {
  createConfiguration,
  createEmbeddingModel,
  deleteConfiguration,
} from '../../fixtures/configurations';

/**
 * The entry point into the credential-creation flow.
 *
 * This is a genuine round-trip landmark, not a paint check: `AddModelButton`
 * renders only inside `ConfigurationsPanel`, which `AIConfiguration.tsx:117`
 * mounts only once `configurationsBySection` is non-null — and
 * `useConfigurationsBySection.ts:76` returns null while ANY of its seven
 * per-section queries is still fetching. A stubbed or heading-only
 * `/settings/model-configuration` cannot produce it.
 */
test('J19a: the AI-Configuration screen offers credential creation and routes to the create flow', async ({ page }) => {
  await page.goto(BASE_URL + '/app/settings/model-configuration');

  const createButton = page.getByRole('button', { name: 'Create configuration', exact: true });
  await expect(createButton).toBeEnabled({ timeout: 30_000 });

  await checkA11y(page);

  await createButton.click();

  // AddModelButton.tsx:19-21 navigates with `search: { from: 'model-configuration' }`.
  // Asserting the search param (not just the path) pins the actual call, so a
  // plain <Link to="/settings/create-configuration"> would not satisfy it.
  await expect(page).toHaveURL(/\/settings\/create-configuration\?.*\bfrom=model-configuration\b/);
});

/**
 * The full lifecycle: pick a type from the server catalog → fill the
 * schema-driven form → POST → see the row → re-open it with server-seeded
 * values → delete it and see it gone.
 *
 * Green as of the #131 fix. It was written red on purpose and every
 * assertion below is the original one — nothing was softened to land it.
 *
 * What it now pins, and what each step would catch if it regressed:
 *
 *  1. The tiles come from GET /configurations/available/, which must serve
 *     the pinned 49-entry registry snapshot WITH `config_schema`. The route
 *     used to serve eight hardcoded {type, display_name, section} rows and no
 *     schema at all — "OpenAI"/"Azure OpenAI"/"Vertex AI" below are
 *     `config_schema.title`s that only the snapshot carries.
 *  2. `CredentialTypeSelector` must survive a schema-less entry rather than
 *     throwing past a route with no error boundary. That crash is what made
 *     /settings/create-configuration render "Something went wrong.", so the
 *     tile assertions double as the regression guard for it.
 *  3. POST must write `elitea_title` from the body key the client actually
 *     sends. Reading `name` wrote "" into a UNIQUE column, so the SECOND
 *     configuration in a project 500'd permanently — which is why this spec
 *     is safe to run repeatedly, and why it is worth running repeatedly.
 *  4. The saved row must appear in the list without a page reload. Three
 *     independent things have to hold: the server derives `section` from the
 *     type's registry entry (open_ai -> ai_credentials) and honours the
 *     ?section= filter, the list client unwraps the transport envelope, and
 *     the create mutation invalidates the cache namespace the
 *     AI-Configuration screen actually reads.
 */
test('J19b: create a credential, verify it persisted, then delete it', async ({ page }) => {
  // `elitea_title` carries a UNIQUE index (scripts/e2e-stack.sh:252) and the
  // create path submits the typed name as both `label` and `elitea_title`
  // (useCredentialFormController.ts buildTitle), so the name must be unique
  // per run AND per concurrently-running spec file.
  const unique = `${AUTOTEST_PREFIX}cred_lifecycle_${Date.now()}`;

  await page.goto(BASE_URL + '/app/settings/model-configuration');

  const createButton = page.getByRole('button', { name: 'Create configuration', exact: true });
  await expect(createButton).toBeEnabled({ timeout: 30_000 });

  // Pre-state: the row must be absent now, so its later appearance proves a
  // round trip rather than a pre-seeded fixture.
  await expect(page.getByText(unique)).toHaveCount(0);

  await createButton.click();
  await expect(page).toHaveURL(/\/settings\/create-configuration\?.*\bfrom=model-configuration\b/);

  // ── The type picker is built from GET /configurations/available/ ─────────
  // These labels come from the catalog's `config_schema.title`
  // (CredentialTypeSelector.tsx displayLabel). A static route body cannot
  // produce them, and neither can a catalogue entry without a schema.
  const openAiTile = page.getByRole('button', { name: 'OpenAI', exact: true });
  await expect(openAiTile).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'Azure OpenAI', exact: true })).toBeVisible();

  // Filtering the fetched list proves the tiles are data, not markup.
  const search = page.getByPlaceholder('Search credentials');
  await search.fill('Vertex');
  await expect(page.getByRole('button', { name: 'Vertex AI', exact: true })).toBeVisible();
  await expect(openAiTile).toHaveCount(0);
  await search.fill('');
  await expect(openAiTile).toBeVisible();

  await checkA11y(page);

  await openAiTile.click();
  await expect(page).toHaveURL(/\/settings\/create-configuration\/open_ai/);

  // ── The form is schema-driven ────────────────────────────────────────────
  // "Api Base"/"Api Key" are `title`s taken from the open_ai JSON schema
  // (CredentialFormFields.tsx metaFor), and "Test connection" renders only
  // when the FETCHED descriptor carries has_test_connection
  // (CredentialForm.tsx:131) — none of the three is reachable without the
  // catalog response.
  // `exact`: an accessible name matches as a substring, and a schema that
  // carries a "Username" field would otherwise resolve two elements.
  const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameInput).toBeVisible({ timeout: 15_000 });
  const apiBase = page.getByRole('textbox', { name: 'Api Base' });
  await expect(apiBase).toBeVisible();
  await expect(page.getByLabel('Api Key')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Test connection' })).toBeVisible();

  await checkA11y(page);

  // ── The save gate is real ────────────────────────────────────────────────
  // canSubmit() needs the seeded `configurations.configuration.update`
  // permission AND a non-blank name (useCredentialFormController.ts:148).
  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeDisabled();
  await nameInput.fill(unique);
  await apiBase.fill('http://localhost/mock');
  await expect(save).toBeEnabled();

  // ── Server round trip, with the assigned id ──────────────────────────────
  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().includes('/configurations/configurations/'),
      { timeout: 20_000 },
    ),
    save.click(),
  ]);
  expect(createResponse.ok()).toBe(true);
  const created = (await createResponse.json()) as { id?: string | number; uuid?: string };
  // The detail/delete routes accept either the numeric id or the uuid; this
  // uses the id, so `uuid` is exercised by the Go unit tests rather than here.
  const configId = String(created.id ?? '');
  expect(configId).not.toBe('');

  // performSave only calls onSaved() — and the route only navigates back —
  // on the success branch, so this URL is itself a mutation assertion.
  await expect(page).toHaveURL(/\/settings\/model-configuration(\?|$)/, { timeout: 15_000 });

  // A section renders only when non-empty (ConfigurationSection.tsx:188), so
  // this row exists only because the POST landed and the list refetched.
  await expect(page.getByText(unique, { exact: true }).first()).toBeVisible({ timeout: 20_000 });

  // ── Values seeded from the server ────────────────────────────────────────
  // useFormSeeding fills these from GET /configurations/configuration/{project}/{id}.
  // The edit screen is reached by deep link because clicking the card goes to
  // /settings/create-configuration and buries the id in a breadcrumb string
  // (useConfigurationNavigation.ts:21-42) — a separate product defect, not
  // something to assert around.
  //
  // The DETAIL READ is waited for by response, not by the field it fills
  // (#545). This journey's recorded webkit flake is exactly here: `Name`
  // resolved 38 times to `<input value="">` and the run ended on
  // `toHaveValue`. That message cannot say whether the read was refused, was
  // slow, or landed and did not seed the form — three different defects with
  // one signature. Asserting the response first makes each of them fail on its
  // own line: a refused or absent read fails on the status, and a form that
  // ignores a 200 it received fails on the value below.
  //
  // The wait is armed BEFORE the navigation that causes the request, because
  // its budget is wall-clock from creation.
  //
  // THE PROJECT SEGMENT IS PART OF THE PREDICATE, and it is not decoration.
  // Matching on the path prefix and the id alone also matched a request for
  // NO project — `/configurations/configuration//{id}`, which the edit screen
  // used to fire on its first render, while the selected-project store was
  // still hydrating. Chromium abandons that request when the store resolves
  // and the query key changes, and then has no body to give for the response
  // this line captured: `Protocol error (Network.getResponseBody): No data
  // found for resource with given identifier`. The app no longer makes that
  // request (`features/credentials/api/useConfigurations.ts`'s
  // `isResolvedId`, unit-tested there); naming the project here means a
  // future stray fails on its own assertion instead of on this one's body.
  const detailRead = page.waitForResponse(
    (res) => res.request().method() === 'GET'
      && new RegExp(`/configurations/configuration/${DEFAULT_PROJECT_ID}/${configId}(\\?|$)`).test(res.url()),
    { timeout: 20_000 },
  );
  await page.goto(`${BASE_URL}/app/settings/edit-configuration/${configId}`);
  const detail = await detailRead;
  expect(detail.status(), await detail.text()).toBe(200);
  const stored = (await detail.json()) as { label?: string; elitea_title?: string };
  expect(
    stored.label ?? stored.elitea_title,
    'the stored row must carry the name the create step sent',
  ).toBe(unique);

  await expect(page.getByRole('textbox', { name: 'Name', exact: true })).toHaveValue(unique, { timeout: 20_000 });
  await expect(page.getByRole('textbox', { name: 'Api Base' })).toHaveValue('http://localhost/mock');

  await checkA11y(page);

  // ── Removal (the second half of JRNY-019's acceptance) ───────────────────
  // Delete exists in edit mode only (CredentialForm.tsx:140), which is why it
  // runs from the edit route rather than the create one.
  await page.getByRole('button', { name: 'Credential actions' }).click();
  await page.getByRole('menuitem', { name: 'Delete' }).click();
  // DeleteEntityModal's type-to-confirm: Confirm stays disabled until the
  // typed value matches the entity name exactly (isConfirmDisabled).
  const confirm = page.getByRole('button', { name: 'Delete', exact: true });
  await expect(confirm).toBeDisabled();
  await page.locator('#delete-entity-modal-input-name').fill(unique);
  await expect(confirm).toBeEnabled();

  const [deleteResponse] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'DELETE' && r.url().includes('/configurations/configuration/'),
      { timeout: 20_000 },
    ),
    confirm.click(),
  ]);
  expect(deleteResponse.ok()).toBe(true);

  // Back on the list, refetched from the server — the row is gone for real,
  // not merely dropped from local state.
  await expect(page).toHaveURL(/\/settings\/model-configuration(\?|$)/, { timeout: 15_000 });
  await expect(createButton).toBeEnabled({ timeout: 30_000 });
  await expect(page.getByText(unique)).toHaveCount(0);
});

/**
 * J19d: the four kinds of configuration appear on the credential list under
 * their OWN section, carrying the fields they were stored with — and the
 * vector store one is usable by a toolkit.
 *
 * Ported by use case from the legacy API suite: its "…appear in the project's
 * credential list" cases, one per section, plus "the shared PgVector fixture
 * is usable by a toolkit".
 *
 * ─────────────────────────────────────────────────────────────────────────
 * WHY THIS IS AN API TEST IN A UI JOURNEY FILE
 * ─────────────────────────────────────────────────────────────────────────
 * J19b above proves the SCREEN renders what the list answers. This proves the
 * list answers the right rows, and it has to look at four sections at once —
 * which through the UI would be four navigations and four pickers, none of
 * them the subject. The two together are the claim: the server files each row
 * under its own section, and the screen shows what it files.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE READ IS FILTERED, AND BOTH FILTERS EARN THEIR PLACE
 * ─────────────────────────────────────────────────────────────────────────
 * `section` is the subject. `query` is what makes the assertion safe under
 * parallel workers: the list is paged, chromium and webkit run this file at
 * the same time against one database, and other journeys create rows in these
 * same sections — so "the first page of the section" is not a page this run
 * can reason about. Filtering by the row's own unique title is.
 *
 * The CROSS-SECTION assertion is the discriminating one. A route that ignored
 * `?section=` would satisfy every "my row is here" line and would put every
 * credential under every heading — which is exactly what it used to do.
 */
test('J19d: each section lists its own rows, and a vector store can be linked by a toolkit', async ({
  request,
}) => {
  const tag = Math.random().toString(36).slice(2, 8);
  const azureTitle = `${AUTOTEST_PREFIX}list_azure_${tag}`;
  const vectorTitle = `${AUTOTEST_PREFIX}list_pgvector_${tag}`;
  const githubTitle = `${AUTOTEST_PREFIX}list_github_${tag}`;
  const embeddingTitle = `${AUTOTEST_PREFIX}list_embed_${tag}`;
  const created: string[] = [];
  let toolkitId = '';
  try {
    const azure = await createConfiguration(request, 'ai_credentials', {
      title: azureTitle,
      type: 'azure_open_ai',
      data: { api_base: 'https://autotest.invalid/openai' },
    });
    created.push(azure.id);
    const vector = await createConfiguration(request, 'vectorstorage', { title: vectorTitle, shared: true });
    created.push(vector.id);
    const github = await createConfiguration(request, 'credentials', { title: githubTitle });
    created.push(github.id);
    const embedding = await createEmbeddingModel(request, { title: embeddingTitle });
    created.push(embedding.id, embedding.credentialId);

    const expectations = [
      { section: 'ai_credentials', title: azureTitle, type: 'azure_open_ai' },
      { section: 'vectorstorage', title: vectorTitle, type: 'pgvector' },
      { section: 'credentials', title: githubTitle, type: 'github' },
      { section: 'embedding', title: embeddingTitle, type: 'embedding_model' },
    ];

    for (const wanted of expectations) {
      const response = await request.get(
        `${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}` +
          `?section=${wanted.section}&query=${encodeURIComponent(wanted.title)}`,
      );
      expect(response.status(), await response.text()).toBe(200);
      const page = (await response.json()) as {
        items?: { elitea_title?: string; label?: string; type?: string; section?: string }[];
      };
      const row = (page.items ?? []).find((entry) => entry.elitea_title === wanted.title);
      expect(row, `${wanted.section}: the row this run created must be listed here`).toBeDefined();
      expect(row?.type, `${wanted.section}: the stored type comes back with it`).toBe(wanted.type);
      expect(row?.label, `${wanted.section}: and the label it was stored with`).toBe(wanted.title);
      expect(row?.section, `${wanted.section}: the row is filed where it is listed`).toBe(
        wanted.section,
      );

      // …and it is listed HERE ONLY. Every other section must answer without
      // it, which is what tells a working filter from a route that serves the
      // whole table under each heading.
      for (const other of expectations.filter((entry) => entry.section !== wanted.section)) {
        const foreign = await request.get(
          `${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}` +
            `?section=${other.section}&query=${encodeURIComponent(wanted.title)}`,
        );
        expect(foreign.status()).toBe(200);
        const foreignPage = (await foreign.json()) as { items?: { elitea_title?: string }[] };
        expect(
          (foreignPage.items ?? []).filter((entry) => entry.elitea_title === wanted.title),
          `${wanted.title} must not appear under ${other.section}`,
        ).toEqual([]);
      }
    }

    // The vector store is not only listed, it is USABLE: a toolkit may link it
    // by title, and the save-time gate resolves that reference before it
    // stores anything. A row the picker offers and the save rejects is the
    // failure this half exists to catch.
    const toolkit = await request.post(
      `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
      {
        data: {
          name: `${AUTOTEST_PREFIX}list_tk_${tag}`,
          type: 'github',
          settings: {
            repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
            github_configuration: { elitea_title: githubTitle, private: false },
            pgvector_configuration: { elitea_title: vectorTitle, private: false },
            selected_tools: [],
          },
        },
      },
    );
    expect(toolkit.status(), await toolkit.text()).toBe(201);
    toolkitId = String(((await toolkit.json()) as { id?: string | number }).id ?? '');
    expect(toolkitId).not.toBe('');
  } finally {
    if (toolkitId !== '') {
      await request
        .delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkitId}`)
        .catch(() => {});
    }
    for (const id of created.reverse()) await deleteConfiguration(request, id);
  }
});
