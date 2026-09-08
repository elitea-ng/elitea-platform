/**
 * Credentials for the TOOLKIT types — the five shapes the legacy suite drives.
 *
 * Ported from the legacy public suite, by use case:
 *
 *   C-1 ← `tests/ui/toolkits/test_github_toolkit.py::TestCreateGitHubCredential::
 *          test_create_github_credential`
 *       + `tests/ui/toolkits/test_github_toolkit.py::TestCreateGitHubToolkit::
 *          test_create_github_toolkit`
 *   C-2 ← `tests/ui/toolkits/test_toolkit_parameterized.py::TestCreateCredential::
 *          test_create_credential[github|jira|gitlab|bitbucket|confluence]`
 *
 * ## What the existing coverage already had, and what it did not
 *
 * `credentials.lifecycle.spec.ts`'s J19b creates ONE credential — `open_ai`,
 * an `ai_credentials` row — through the same form, and proves the round trip
 * (catalogue → schema-driven form → POST → list → edit → delete). What it
 * cannot say is anything about the OTHER section: every type in this file is
 * `section: "credentials"`, each carries its own `data` sub-schema, and three
 * of the five put a required field inside the schema's `auth` SUBSECTION,
 * which is a different renderer (`CredentialFormSection`'s radio group) from
 * the flat fields J19b fills. A form that rendered no subsection at all would
 * pass every J19b assertion.
 *
 * ## The fields are read from the served catalogue, never listed here
 *
 * `GET /configurations/available/` carries each type's `config_schema`, and
 * the form labels every field with that schema's `title`. Naming the labels
 * here would be a second copy of the SDK's snapshot, and it would fail as
 * "credentials are broken" the first time a title changes. So the required
 * list and the labels are derived, and the derivation itself is asserted:
 * each type must still carry at least one required data field, or the loop
 * would fill nothing and save an empty form.
 *
 * ## What stays out of scope, and why
 *
 * "Test settings" / "Test connection" against the real provider — the legacy
 * `TestToolkitTestSettings` and `TestGitHubToolkitTestSettings` — needs a
 * live GitHub/Jira/GitLab/Bitbucket/Confluence credential. Nothing here calls
 * a provider: the Go API does not contact one at CREATE time, which is why
 * these five can be authored with placeholder values at all. Those cases are
 * not ported and are not left behind as skips.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** The five the legacy suite parametrises over (`toolkit_configs.py`'s `TOOLKIT_CONFIGS`). */
const TOOLKIT_CREDENTIAL_TYPES = ['github', 'jira', 'gitlab', 'bitbucket', 'confluence'] as const;

const RUN_ID = String(Date.now()).slice(-6);

/** Placeholder values. Never a real credential — see the file header. */
const PLACEHOLDER_URL = 'https://autotest.invalid/api';
const PLACEHOLDER_TEXT = 'autotest-placeholder';

/**
 * THE ROUTE THAT OFFERS THESE FIVE TYPES, and it is not the one J19b uses.
 *
 * `CredentialForm` renders one picker for two routes, and the routes ask the
 * catalogue for DIFFERENT sections. `useTypeSections`
 * (`src/pages/credentials/useTypeSections.ts:27-36,98`) sends
 * `/settings/create-configuration` — `mode.configurationMode` — the seven
 * MODEL sections (`llm`, `embedding`, `vectorstorage`, `ai_credentials`,
 * `image_generation`, `asr`, `tts`), and `/credentials/create-credential` the
 * single `credentials` section. Every type in this file is
 * `section: "credentials"`
 * (`services/elitea-main/internal/application/configurations/
 * current_available_snapshot.json`), so the configuration route serves no tile
 * for any of them — the picker rendered, the search matched nothing, and the
 * tile assertion read 0.
 *
 * J19b's `open_ai` is an `ai_credentials` row, which is why that journey is
 * correct on the OTHER route. The two are not interchangeable and neither is
 * wrong.
 */
const CREDENTIAL_CREATE_ROUTE = 'credentials/create-credential';

interface SchemaNode {
  readonly title?: string;
  readonly type?: string;
  readonly required?: readonly string[];
  readonly properties?: Readonly<Record<string, SchemaNode>>;
}

interface ConfigurationTypeDescriptor {
  readonly type?: string;
  readonly config_schema?: {
    readonly title?: string;
    /** The screen prefers `metadata.label` over `title` — see `tileLabelOf`. */
    readonly metadata?: { readonly label?: string };
    readonly properties?: { readonly data?: SchemaNode };
  };
}

/**
 * The tile's text, DERIVED THE WAY THE SCREEN DERIVES IT.
 *
 * `CredentialTypeSelector`'s `displayLabel`
 * (`src/pages/credentials/CredentialTypeSelector.tsx:57-59`) is
 * `metadata.label ?? title ?? type`, in that order. Today the five types here
 * carry no root `metadata`, so `title` decides — but reading only `title`
 * would have made this file fail as "the type is not offered" on the day one
 * of them gained a label, which is a different defect entirely.
 */
function tileLabelOf(descriptor: ConfigurationTypeDescriptor | undefined, type: string): string {
  return descriptor?.config_schema?.metadata?.label ?? descriptor?.config_schema?.title ?? type;
}

/** One field the create form must be given a value for. */
interface RequiredField {
  readonly key: string;
  /** The `title` the form labels it with (`CredentialFormFields.tsx`'s `metaFor`). */
  readonly label: string;
}

interface TypeUnderTest {
  readonly type: string;
  /** The tile's text — `CredentialTypeSelector`'s `displayLabel`. */
  readonly tileLabel: string;
  readonly requiredFields: readonly RequiredField[];
}

/** Every configuration type this deployment offers, keyed by type. */
async function readAvailableTypes(
  request: APIRequestContext,
): Promise<Record<string, ConfigurationTypeDescriptor>> {
  const response = await request.get(`${API_BASE}/configurations/available/`);
  expect(response.status(), await response.text()).toBe(200);
  const rows = (await response.json()) as readonly ConfigurationTypeDescriptor[];
  const byType: Record<string, ConfigurationTypeDescriptor> = {};
  for (const row of rows) {
    if (row.type !== undefined) byType[row.type] = row;
  }
  return byType;
}

/** The served shape of one type, as this file drives it. */
function describeType(
  catalogue: Record<string, ConfigurationTypeDescriptor>,
  type: string,
): TypeUnderTest {
  const descriptor = catalogue[type];
  expect(descriptor, `this deployment does not offer the ${type} credential type`).toBeDefined();
  const data = descriptor?.config_schema?.properties?.data;
  const requiredKeys = data?.required ?? [];
  expect(
    requiredKeys.length,
    `the ${type} schema declares no required field, so this loop would save an empty form`,
  ).toBeGreaterThan(0);
  return {
    type,
    tileLabel: tileLabelOf(descriptor, type),
    requiredFields: requiredKeys.map((key) => ({
      key,
      label: data?.properties?.[key]?.title ?? key,
    })),
  };
}

/** A value that suits the field, so a URL field is not handed a word. */
function valueFor(field: RequiredField): string {
  return /url$/i.test(field.key) ? PLACEHOLDER_URL : `${PLACEHOLDER_TEXT}-${field.key}`;
}

/**
 * Author one credential through the create form, from the type CHOOSER.
 *
 * Through the chooser and not a deep link to
 * `/credentials/create-credential/{type}`: "the type is offered at all" is
 * half of what each legacy case asserts, and a deep link would render the form
 * for a type whose tile the catalogue had stopped serving.
 *
 * Returns the id the server assigned.
 */
async function createCredentialThroughForm(
  page: Page,
  subject: TypeUnderTest,
  name: string,
): Promise<string> {
  await page.goto(`${BASE_URL}/app/${CREDENTIAL_CREATE_ROUTE}`, {
    waitUntil: 'domcontentloaded',
  });

  const search = page.getByPlaceholder('Search credentials');
  await expect(search, 'the credential type chooser must render').toBeVisible({ timeout: 30_000 });
  await search.fill(subject.tileLabel);

  // Anchored and case-insensitive, the same rule `toolkits.catalogue.spec.ts`
  // states: the screen's label and the server's differ in case for several
  // types, and an unanchored name would let `Jira` also match a future
  // `Jira Service Desk`.
  const escaped = subject.tileLabel.replaceAll(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`);
  const tile = page.getByRole('button', { name: new RegExp(`^${escaped}$`, 'i') });
  await expect(tile, `no tile for the ${subject.type} credential type`).toHaveCount(1);
  await tile.click();

  await expect(page).toHaveURL(new RegExp(`/${CREDENTIAL_CREATE_ROUTE}/${subject.type}`), {
    timeout: 20_000,
  });

  /*
   * `exact`, because the accessible name is a SUBSTRING match by default and
   * the schema-driven fields below are labelled by the served schema. The
   * github/gitlab/bitbucket schemas carry a "Username" field, whose
   * accessible name contains "Name", so the unanchored locator resolved to
   * two elements and the strict-mode check failed before the form was ever
   * filled.
   */
  const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameInput).toBeVisible({ timeout: 20_000 });
  await nameInput.fill(name);

  /*
   * EVERY required field, by the label the SCHEMA gave it.
   *
   * `getByLabel` and not `getByRole('textbox')`: the ones whose schema says
   * `format: password` render through `SecretManagementInput`, i.e. an
   * `input[type=password]`, which carries no textbox role. Both kinds are
   * labelled, and both are addressed the same way here — which is also what
   * makes a field that stopped rendering fail on its own line.
   */
  for (const field of subject.requiredFields) {
    const control = page.getByLabel(field.label).first();
    await expect(
      control,
      `the ${subject.type} form did not render its required "${field.label}" field`,
    ).toBeVisible({ timeout: 20_000 });
    await control.fill(valueFor(field));
  }

  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeEnabled({ timeout: 15_000 });

  const [response] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().includes('/configurations/configurations/'),
      { timeout: 30_000 },
    ),
    save.click(),
  ]);
  expect(
    response.ok(),
    `saving a ${subject.type} credential answered ${response.status()}: ${(await response.text()).slice(0, 300)}`,
  ).toBe(true);
  const created = (await response.json()) as { id?: string | number };
  const id = String(created.id ?? '');
  expect(id, 'the created credential must carry an id').not.toBe('');
  return id;
}

/** Ids to remove, whatever happened. */
const createdCredentialIds: string[] = [];
const createdToolkitIds: string[] = [];

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext();
  try {
    for (const id of createdToolkitIds) {
      await context.request
        .delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`)
        .catch(() => {});
    }
    for (const id of createdCredentialIds) {
      await context.request
        .delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`)
        .catch(() => {});
    }
  } finally {
    await context.close();
  }
});

test('C-1: a GitHub credential is authored from the form and a GitHub toolkit references it (legacy test_create_github_credential + test_create_github_toolkit)', async ({
  page,
}) => {
  test.setTimeout(180_000);

  const catalogue = await readAvailableTypes(page.request);
  const subject = describeType(catalogue, 'github');

  // The github shape, not a generic one: `base_url` is required and arrives
  // PREFILLED from the schema's `prefill_value`, and the three auth methods
  // live in the schema's own `auth` subsections.
  expect(subject.requiredFields.map((field) => field.key)).toContain('base_url');

  const name = `${AUTOTEST_PREFIX}cred_github_${RUN_ID}`;
  const credentialId = await createCredentialThroughForm(page, subject, name);
  createdCredentialIds.push(credentialId);

  // ── the server stored it, under the name the form typed ─────────────────
  const stored = await page.request.get(
    `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${credentialId}`,
  );
  expect(stored.status(), await stored.text()).toBe(200);
  const row = (await stored.json()) as {
    type?: string;
    label?: string;
    elitea_title?: string;
  };
  expect(row.type, 'the stored row must carry the type the tile chose').toBe('github');
  expect(row.label ?? row.elitea_title).toBe(name);
  const eliteaTitle = row.elitea_title ?? '';
  expect(eliteaTitle, 'the stored credential must carry an elitea_title to be referenced by').not.toBe('');

  /*
   * ── the second legacy case: a TOOLKIT that references the credential ───
   *
   * `github`'s toolkit schema declares `github_configuration` as a
   * CONFIGURATION REFERENCE (`configuration_types`, not a value), so the
   * toolkit does not hold the secret — it holds the credential's
   * `elitea_title`. That reference is the whole of what
   * `test_create_github_toolkit` establishes without a live provider, and
   * getting its SHAPE wrong is a real defect class here (the same
   * "`ai_credentials` stored as a bare string" bug `CredentialFormFields.tsx`
   * documents).
   *
   * Created over the API rather than through the toolkit form: the form's
   * credential picker is the surface `toolkits.catalogue.spec.ts` owns, and
   * duplicating it here would test the picker twice and the reference once.
   */
  const toolkitName = `${AUTOTEST_PREFIX}tk_github_${RUN_ID}`;
  const toolkit = await page.request.post(
    `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        name: toolkitName,
        type: 'github',
        settings: {
          github_configuration: { elitea_title: eliteaTitle, private: false },
          // REQUIRED, alongside the reference. `validateToolkitCreate`
          // (`services/elitea-main/internal/api/v2/toolkits/handler.go:987-1004`)
          // answers 400 without it, and the SDK settings schema declares
          // `required: ["github_configuration", "repository"]` for this type —
          // so a body carrying only the reference could never have been saved
          // through the form either.
          repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
          selected_tools: [],
        },
      },
    },
  );
  expect(toolkit.status(), await toolkit.text()).toBe(201);
  const toolkitBody = (await toolkit.json()) as {
    id?: string;
    settings?: { github_configuration?: { elitea_title?: string } };
  };
  const toolkitId = String(toolkitBody.id ?? '');
  expect(toolkitId).not.toBe('');
  createdToolkitIds.push(toolkitId);
  expect(
    toolkitBody.settings?.github_configuration?.elitea_title,
    'the stored toolkit must keep the credential reference, not flatten it to a string',
  ).toBe(eliteaTitle);

  // …and it opens as a real toolkit, under the name it was created with.
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(toolkitName, {
    timeout: 20_000,
  });
});

test('C-2: each toolkit credential type is offered, renders its own schema, and saves (legacy test_create_credential[github|jira|gitlab|bitbucket|confluence])', async ({
  page,
}) => {
  // Five chooser loads, five schema-driven forms, five saves.
  test.setTimeout(10 * 60_000);

  const catalogue = await readAvailableTypes(page.request);
  const saved: string[] = [];

  /*
   * ONE test that loops the five, not one test per type.
   *
   * Playwright fixes its test list before the run starts, so a test per type
   * would need the served catalogue at module load — a network read at import
   * time, with no browser context and no way to report a failure as a test.
   * The same reasoning `toolkits.catalogue.spec.ts` states for its own
   * data-driven loop; every assertion below names the type it is about, so a
   * failure still says which one broke.
   */
  for (const type of TOOLKIT_CREDENTIAL_TYPES) {
    const subject = describeType(catalogue, type);
    const name = `${AUTOTEST_PREFIX}cred_${type}_${RUN_ID}`;

    const id = await createCredentialThroughForm(page, subject, name);
    createdCredentialIds.push(id);

    // Read back from the SERVER, not from the list screen: these are
    // `section: "credentials"` rows and the AI-Configuration screen J19b
    // reads shows the `ai_credentials` sections, so a list assertion there
    // would fail for the wrong reason.
    const stored = await page.request.get(
      `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`,
    );
    expect(stored.status(), `${type}: ${await stored.text()}`).toBe(200);
    const row = (await stored.json()) as { type?: string; label?: string; elitea_title?: string };
    expect(row.type, `the stored row must be a ${type} credential`).toBe(type);
    expect(row.label ?? row.elitea_title, `${type} did not keep the name the form typed`).toBe(name);
    saved.push(type);
  }

  /*
   * A loop is a shape that can pass by running zero times, and this one runs
   * over a list read at run time. Without this the whole test would go green
   * against an empty catalogue, in about a second, and read as a pass.
   */
  expect(saved).toEqual([...TOOLKIT_CREDENTIAL_TYPES]);
});
