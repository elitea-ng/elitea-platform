/**
 * Toolkit credentials and the toolkit creation contract, over the API.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's GitHub credential cases (the three authentication
 * shapes and their sealing, the two incomplete shapes that are still allowed,
 * the shared flag and the credential listing) and its toolkit cases (the full
 * reference create, the minimal create, the two refusals, the branches, the
 * selected tools, the artifact toolkit against a bucket, and the three
 * assertions on the toolkit DETAILS document).
 *
 * It is an API journey and not an extension of
 * `e2e/journeys/credentials/credentials.toolkit-types.spec.ts` because that
 * file is a UI journey: it drives the create FORM, which is the right surface
 * for "is this type offered and does its form render" and the wrong one for
 * twenty payload shapes. Folding these in would also have taken that file past
 * a thousand lines for cases none of which need a browser. The two do not
 * overlap: nothing here opens a page, and nothing there sends a hand-built
 * body.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE DEFECT THIS PACKAGE FOUND
 * ─────────────────────────────────────────────────────────────────────────────
 * The toolkit details route reported a `toolkit_name` no runtime path uses. It
 * kept alphanumerics only, while index admission, the tool-run path, the
 * built-in name deriver, the Python worker's SDK adapter and all three web
 * helpers keep `_`, `.` and `-` and fold the `.` into `_`. A toolkit called
 * `autotest github-toolkit.v1` was reported as `autotestgithubtoolkitv1` and
 * addressed by the agent as `autotestgithub-toolkit_v1`, so an instruction
 * written from the details page named a tool that does not exist. The rule now
 * lives once, in `internal/toolkitnaming`, and the details route reads it from
 * there; the journey at the foot of this file is the API-side statement of it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * NOTHING HERE IS A REAL CREDENTIAL
 * ─────────────────────────────────────────────────────────────────────────────
 * Every value stored below is an `autotest`/`.invalid` fixture string. The
 * create route does not contact a provider, which is why placeholder values
 * can be stored at all; the "test connection" cases that DO need a live GitHub
 * account are out of scope and are not left behind as skips.
 *
 * The token-shaped fields go into the project VAULT: the create seals every
 * schema-declared password field and stores `{{secret.<32 hex>}}` in the row,
 * so the plain value exists nowhere the API can serve it back. That is what
 * the sealing assertions check, and they check it by looking for the plain
 * value in the WHOLE response document rather than in the field it was sent
 * in — a seal that moved the value to a sibling key would pass the narrower
 * check.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * ONE CASE IS DELIBERATELY NOT HERE
 * ─────────────────────────────────────────────────────────────────────────────
 * "A GitHub credential with no label and no data is refused" is not a
 * credentials-section rule: the configuration create route validates neither
 * field for ANY type, so the same body is accepted for a model, a vector store
 * and an embedding model too. It belongs with the configuration create
 * contract, which owns the refusal for all four sections, and asserting
 * today's acceptance here would have to be reverted by the change that fixes
 * it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every credential, toolkit and bucket is `autotest`-named, created
 * through the API and removed in a `finally`.
 */
import { test, expect, type APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createGithubToolkit,
  deleteGithubToolkit,
  type GithubToolkitFixture,
} from '../../fixtures/api';
import {
  createConfiguration,
  createEmbeddingModel,
  deleteConfiguration,
} from '../../fixtures/configurations';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * A bucket name, which may NOT carry the `autotest_` prefix.
 *
 * The artifact toolkit's own settings schema declares
 * `pattern: "^[a-z][a-z0-9-]*$"` for `bucket`, and the object store applies
 * the same S3 naming rules. `autotest-` keeps the sweepable prefix inside
 * them.
 */
function autotestBucketName(): string {
  return `autotest-tk-${Math.random().toString(36).slice(2, 8)}`;
}

/** The placeholder GitHub endpoint. Deliberately unroutable. */
const PLACEHOLDER_BASE_URL = 'https://autotest.invalid/api';

const CREDENTIALS = `${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`;
const CREDENTIAL = (id: string): string =>
  `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`;
const TOOLKITS = `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`;
const TOOLKIT = (id: string): string =>
  `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`;
const TOOLKIT_TYPE_SCHEMAS = `${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`;
const BUCKETS = `${API_BASE}/artifacts/buckets/${DEFAULT_PROJECT_ID}`;

/**
 * The shape a sealed secret takes in a stored row: `{{secret.<32 hex>}}`.
 *
 * Asserted as a SHAPE and not merely as "not the plaintext". A seal that wrote
 * an empty string, or the literal `{{secret.}}`, would also fail to echo the
 * value — and would also make the credential unusable, which is the failure
 * this pattern separates from a working seal.
 */
const SEALED = /^\{\{secret\.[0-9a-f]{32}\}\}$/;

interface StoredConfiguration {
  readonly id?: number;
  readonly type?: string;
  readonly name?: string;
  readonly elitea_title?: string;
  readonly label?: string;
  readonly section?: string;
  readonly shared?: boolean;
  readonly data?: Record<string, unknown>;
}

/** Create one credential row straight from a body, and return what was stored. */
async function postCredential(
  request: APIRequestContext,
  body: Record<string, unknown>,
): Promise<{ id: string; stored: StoredConfiguration; raw: string }> {
  const response = await request.post(CREDENTIALS, { data: body });
  const raw = await response.text();
  expect(response.status(), raw).toBe(201);
  const stored = JSON.parse(raw) as StoredConfiguration;
  const id = String(stored.id ?? '');
  expect(id, 'the create must answer an id').not.toBe('');
  return { id, stored, raw };
}

/** Read one credential row back from the server. */
async function readCredential(
  request: APIRequestContext,
  id: string,
): Promise<{ stored: StoredConfiguration; raw: string }> {
  const response = await request.get(CREDENTIAL(id));
  const raw = await response.text();
  expect(response.status(), raw).toBe(200);
  return { stored: JSON.parse(raw) as StoredConfiguration, raw };
}

interface ToolkitDetails {
  readonly id?: string;
  readonly type?: string;
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly description?: string;
  readonly settings?: Record<string, unknown>;
  readonly meta?: Record<string, unknown>;
  readonly author_id?: unknown;
  readonly author?: Record<string, unknown>;
  readonly agent_type?: unknown;
  readonly online?: unknown;
}

/** Create one toolkit straight from a body. Returns the id and the echo. */
async function postToolkit(
  request: APIRequestContext,
  body: Record<string, unknown>,
): Promise<{ id: string; created: ToolkitDetails }> {
  const response = await request.post(TOOLKITS, { data: body });
  const raw = await response.text();
  expect(response.status(), raw).toBe(201);
  const created = JSON.parse(raw) as ToolkitDetails;
  const id = String(created.id ?? '');
  expect(id, 'the create must answer an id').not.toBe('');
  return { id, created };
}

/** Read the toolkit DETAILS document — the one this file's last cases are about. */
async function readToolkit(request: APIRequestContext, id: string): Promise<ToolkitDetails> {
  const response = await request.get(TOOLKIT(id));
  const raw = await response.text();
  expect(response.status(), raw).toBe(200);
  return JSON.parse(raw) as ToolkitDetails;
}

/** Remove a toolkit. Best effort by design — it runs in a `finally`. */
async function removeToolkit(request: APIRequestContext, id: string): Promise<void> {
  if (id === '') return;
  await request.delete(TOOLKIT(id)).catch(() => {});
}

/** The refusal body the toolkit create writes: `{"error": "<reason>"}`. */
async function refusalReason(response: { text(): Promise<string> }): Promise<string> {
  const raw = await response.text();
  try {
    return String((JSON.parse(raw) as { error?: unknown }).error ?? raw);
  } catch {
    return raw;
  }
}

/* ── the GitHub authentication shapes ─────────────────────────────────────── */

/**
 * The three shapes the type's schema declares, as its own `auth` subsections
 * name them: a token, a username/password pair, and an App id with a private
 * key. The secret half of each is a `format: password` field, which is what
 * the create seals.
 */
const AUTHENTICATION_SHAPES = [
  {
    name: 'access token',
    data: { access_token: `${AUTOTEST_PREFIX}gh_token_value` },
    sealed: ['access_token'],
    plain: [] as const,
  },
  {
    name: 'username and password',
    data: { username: `${AUTOTEST_PREFIX}gh_user`, password: `${AUTOTEST_PREFIX}gh_password` },
    sealed: ['password'],
    plain: ['username'],
  },
  {
    name: 'App id and private key',
    data: { app_id: '424242', app_private_key: `${AUTOTEST_PREFIX}gh_private_key` },
    sealed: ['app_private_key'],
    plain: ['app_id'],
  },
] as const;

test('each GitHub authentication shape is stored with its secret sealed and its plain fields intact', async ({
  request,
}) => {
  const created: string[] = [];
  const proved: string[] = [];
  try {
    for (const shape of AUTHENTICATION_SHAPES) {
      const title = autotestName(`cred_${shape.sealed[0] ?? 'auth'}`);
      const { id, stored, raw } = await postCredential(request, {
        type: 'github',
        elitea_title: title,
        label: title,
        shared: false,
        data: { base_url: PLACEHOLDER_BASE_URL, ...shape.data },
      });
      created.push(id);

      expect(stored.type, `${shape.name}: the stored row must be a github credential`).toBe(
        'github',
      );
      // The section decides which screen shows the row and which picker offers
      // it, and the create route derives it from the TYPE. A row filed under
      // "" exists and is invisible.
      expect(stored.section, `${shape.name}: the row must be filed under credentials`).toBe(
        'credentials',
      );
      expect(stored.elitea_title, `${shape.name}: the title a toolkit resolves by`).toBe(title);

      const data = stored.data ?? {};
      for (const key of shape.sealed) {
        expect(
          String(data[key] ?? ''),
          `${shape.name}: ${key} must come back as a vault reference`,
        ).toMatch(SEALED);
      }
      for (const key of shape.plain) {
        // The NON-secret half of the pair must survive as itself. A seal that
        // swept the whole `data` object would pass every assertion above and
        // leave a credential that cannot authenticate.
        expect(data[key], `${shape.name}: ${key} is not a secret and must be stored as sent`).toBe(
          (shape.data as Record<string, string>)[key],
        );
      }
      expect(data['base_url'], `${shape.name}: the base URL is not a secret`).toBe(
        PLACEHOLDER_BASE_URL,
      );

      // The plaintext must appear NOWHERE in the create echo or in the read —
      // not in the field it was sent in, and not in a sibling.
      const { raw: readRaw } = await readCredential(request, id);
      for (const key of shape.sealed) {
        const secretValue = (shape.data as Record<string, string>)[key] ?? '';
        expect(secretValue).not.toBe('');
        expect(raw, `${shape.name}: the create echoed the plaintext ${key}`).not.toContain(
          secretValue,
        );
        expect(readRaw, `${shape.name}: the read served the plaintext ${key}`).not.toContain(
          secretValue,
        );
      }
      proved.push(shape.name);
    }
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }

  // A loop is a shape that can pass by running zero times.
  expect(proved).toEqual(AUTHENTICATION_SHAPES.map((shape) => shape.name));
});

test('a GitHub credential with no authentication, and one with half a pair, are both stored', async ({
  request,
}) => {
  /*
   * Both are DELIBERATE. `auth` is `required: false` in the type's schema and
   * each of its fields is nullable, because a GitHub Enterprise endpoint may
   * be reachable anonymously and because the create form saves what has been
   * filled in so far. A route that refused either would make the form
   * unsavable half way through.
   */
  const shapes = [
    { name: 'no authentication at all', data: {} as Record<string, string> },
    { name: 'a username with no password', data: { username: `${AUTOTEST_PREFIX}gh_user` } },
  ];
  const created: string[] = [];
  const proved: string[] = [];
  try {
    for (const shape of shapes) {
      const title = autotestName('cred_partial');
      const { id, stored } = await postCredential(request, {
        type: 'github',
        elitea_title: title,
        label: title,
        shared: false,
        data: { base_url: PLACEHOLDER_BASE_URL, ...shape.data },
      });
      created.push(id);
      expect(stored.section, `${shape.name}: filed under credentials`).toBe('credentials');
      const data = stored.data ?? {};
      expect(data['base_url'], `${shape.name}: the base URL is stored`).toBe(PLACEHOLDER_BASE_URL);
      // Nothing is invented in place of the missing half.
      expect(data['password'], `${shape.name}: no password may be conjured`).toBeUndefined();
      proved.push(shape.name);
    }
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }
  expect(proved).toEqual(shapes.map((shape) => shape.name));
});

test('a GitHub credential can be marked shared, and the flag survives the read', async ({
  request,
}) => {
  const title = autotestName('cred_shared');
  const { id, stored } = await postCredential(request, {
    type: 'github',
    elitea_title: title,
    label: title,
    shared: true,
    data: { base_url: PLACEHOLDER_BASE_URL, access_token: `${AUTOTEST_PREFIX}gh_shared_token` },
  });
  try {
    expect(stored.shared, 'the create must store the shared flag it was sent').toBe(true);
    // Read back, because `shared` is the one field a partial update used to
    // erase the row's data over: the create echo is not the column.
    const { stored: read } = await readCredential(request, id);
    expect(read.shared, 'the stored row must still be shared').toBe(true);
    expect(String(read.data?.['access_token'] ?? ''), 'sharing must not unseal the token').toMatch(
      SEALED,
    );
  } finally {
    await deleteConfiguration(request, id);
  }
});

test('a GitHub credential is listed under the credentials section, by its own name', async ({
  request,
}) => {
  const title = autotestName('cred_listed');
  const { id } = await postCredential(request, {
    type: 'github',
    elitea_title: title,
    label: title,
    shared: false,
    data: { base_url: PLACEHOLDER_BASE_URL },
  });
  try {
    /*
     * Filtered by section, type AND name — never a hopeful first page. The
     * list route searches `label` with an ILIKE, and every worker in both
     * engines is writing rows into this same project while this runs, so a
     * paged read would be a statement about them.
     */
    const listed = await request.get(
      `${CREDENTIALS}?section=credentials&type=github&query=${encodeURIComponent(title)}`,
    );
    const raw = await listed.text();
    expect(listed.status(), raw).toBe(200);
    const envelope = JSON.parse(raw) as {
      items?: readonly StoredConfiguration[];
      total?: number;
    };
    const rows = envelope.items ?? [];
    const mine = rows.filter((row) => row.elitea_title === title);
    expect(mine, `the credential is not in its own section's list: ${raw.slice(0, 300)}`).toHaveLength(
      1,
    );
    expect(mine[0]?.section).toBe('credentials');
    expect(mine[0]?.type).toBe('github');
  } finally {
    await deleteConfiguration(request, id);
  }
});

/* ── the GitHub toolkit creation contract ─────────────────────────────────── */

test('a GitHub toolkit carries its credential, its vector store and its embedding model by reference', async ({
  request,
}) => {
  const credentialTitle = autotestName('tkcred');
  const vectorTitle = autotestName('tkvector');
  const embeddingTitle = autotestName('tkembed');

  const credential = await createConfiguration(request, 'credentials', {
    title: credentialTitle,
    data: { base_url: PLACEHOLDER_BASE_URL, access_token: `${AUTOTEST_PREFIX}gh_ref_token` },
  });
  const vector = await createConfiguration(request, 'vectorstorage', { title: vectorTitle });
  // Not `createConfiguration('embedding', …)`: an embedding model is only
  // referenceable once the project can SEE it, which takes a credential of its
  // own and an admitted row. The fixture makes both.
  const embedding = await createEmbeddingModel(request, { title: embeddingTitle });

  const name = autotestName('tk_full');
  let toolkitId = '';
  try {
    const { id, created } = await postToolkit(request, {
      name,
      type: 'github',
      settings: {
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        /*
         * OBJECTS, not bare strings. A configuration reference is
         * `{elitea_title, private}`, and `private: true` does not mean "not
         * shared" — it means "resolve this title in the CALLER's PERSONAL
         * project". A row created here, in the project under test, must be
         * referenced with `private: false`.
         */
        github_configuration: { elitea_title: credential.title, private: false },
        pgvector_configuration: { elitea_title: vector.title, private: false },
        // An embedding model is named by a plain string — the schema declares
        // it `configuration_model: "embedding"`, not a configuration-reference
        // object, and the two shapes are not interchangeable. The string is the
        // model's NAME, which the catalogue publishes from `data.name`; the
        // row's own title names nothing there.
        embedding_model: embedding.modelName,
        selected_tools: ['get_issues', 'get_issue'],
      },
    });
    toolkitId = id;
    expect(created.type).toBe('github');

    const details = await readToolkit(request, id);
    const settings = (details.settings ?? {}) as Record<string, Record<string, unknown> | unknown>;
    expect(
      (settings['github_configuration'] as Record<string, unknown> | undefined)?.['elitea_title'],
      'the credential reference must survive as an object',
    ).toBe(credential.title);
    expect(
      (settings['pgvector_configuration'] as Record<string, unknown> | undefined)?.['elitea_title'],
      'the vector-store reference must survive as an object',
    ).toBe(vector.title);
    expect(settings['embedding_model'], 'the embedding model is stored by name').toBe(
      embedding.modelName,
    );
  } finally {
    await removeToolkit(request, toolkitId);
    await deleteConfiguration(request, embedding.id);
    await deleteConfiguration(request, embedding.credentialId);
    await deleteConfiguration(request, vector.id);
    await deleteConfiguration(request, credential.id);
  }
});

test('a GitHub toolkit can be created with only the two settings its type requires', async ({
  request,
}) => {
  const credentialTitle = autotestName('tkcred_min');
  const credential = await createConfiguration(request, 'credentials', {
    title: credentialTitle,
    data: { base_url: PLACEHOLDER_BASE_URL },
  });
  const name = autotestName('tk_min');
  let toolkitId = '';
  try {
    const { id } = await postToolkit(request, {
      name,
      type: 'github',
      // The two the type declares required, and nothing else. Everything the
      // schema gives a default must be optional at the route as well, or the
      // create form's own defaults would be the only way to save.
      settings: {
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        github_configuration: { elitea_title: credential.title, private: false },
      },
    });
    toolkitId = id;
    const details = await readToolkit(request, id);
    expect(details.name).toBe(name);
    expect(details.type).toBe('github');
  } finally {
    await removeToolkit(request, toolkitId);
    await deleteConfiguration(request, credential.id);
  }
});

test('a GitHub toolkit body missing a required setting is refused, and says which one', async ({
  request,
}) => {
  const credentialTitle = autotestName('tkcred_refuse');
  const credential = await createConfiguration(request, 'credentials', {
    title: credentialTitle,
    data: { base_url: PLACEHOLDER_BASE_URL },
  });
  /*
   * The three refusals `validateToolkitCreate` writes, with the reason each
   * carries. The reasons are asserted, not just the status: a 400 that named
   * the wrong field would leave the create form pointing at a field the user
   * had filled in.
   */
  const refusals = [
    {
      name: 'no repository',
      settings: { github_configuration: { elitea_title: credential.title, private: false } },
      reason: 'settings.repository is required for github toolkit',
    },
    {
      name: 'a blank repository',
      settings: {
        repository: '',
        github_configuration: { elitea_title: credential.title, private: false },
      },
      reason: 'settings.repository is required for github toolkit',
    },
    {
      name: 'no credential reference',
      settings: { repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo` },
      reason: 'settings.github_configuration is required for github toolkit',
    },
  ];
  const proved: string[] = [];
  try {
    for (const refusal of refusals) {
      const name = autotestName('tk_refused');
      const response = await request.post(TOOLKITS, {
        data: { name, type: 'github', settings: refusal.settings },
      });
      expect(response.status(), `${refusal.name}: ${await response.text()}`).toBe(400);
      expect(await refusalReason(response), `${refusal.name}: the reason`).toBe(refusal.reason);
      // The refusal carries no id, so there is nothing to clean up — the
      // route validates before it writes. "Did it nevertheless store a row"
      // is NOT asserted by scanning the project's toolkit list: that list
      // offers no name filter and every other worker in both engines is
      // creating toolkits in this same project while this runs, so the scan
      // would be a statement about them. The Go suite states it where it can
      // be stated exactly.
      expect(
        (JSON.parse(await response.text()) as { id?: unknown }).id,
        `${refusal.name}: a refused create must not answer an id`,
      ).toBeUndefined();
      proved.push(refusal.name);
    }
  } finally {
    await deleteConfiguration(request, credential.id);
  }
  expect(proved).toEqual(refusals.map((refusal) => refusal.name));
});

test('a GitHub toolkit records the branches and the tools it was given, and only those', async ({
  request,
}) => {
  const credential = await createConfiguration(request, 'credentials', {
    title: autotestName('tkcred_branch'),
    data: { base_url: PLACEHOLDER_BASE_URL },
  });
  const name = autotestName('tk_branches');
  const selected = ['get_issues', 'create_issue', 'list_branches_in_repo'];
  let toolkitId = '';
  try {
    const { id } = await postToolkit(request, {
      name,
      type: 'github',
      settings: {
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        github_configuration: { elitea_title: credential.title, private: false },
        active_branch: 'autotest-feature',
        base_branch: 'autotest-main',
        selected_tools: selected,
      },
    });
    toolkitId = id;
    const details = await readToolkit(request, id);
    const settings = details.settings ?? {};
    expect(settings['active_branch'], 'the active branch must be stored as given').toBe(
      'autotest-feature',
    );
    expect(settings['base_branch'], 'the base branch must be stored as given').toBe(
      'autotest-main',
    );
    /*
     * EXACTLY the selected tools, in the order they were sent. `toEqual` and
     * not `toContain`: the failure this guards against is the opposite one —
     * a route that stored the type's whole tool list would give the agent
     * every GitHub tool including the writes, for a toolkit the user
     * restricted to three.
     */
    expect(settings['selected_tools'], 'only the selected tools may be stored').toEqual(selected);
  } finally {
    await removeToolkit(request, toolkitId);
    await deleteConfiguration(request, credential.id);
  }
});

/* ── the artifact toolkit ─────────────────────────────────────────────────── */

test('an artifact toolkit is created against a bucket with every tool its type offers', async ({
  request,
}) => {
  /*
   * The tool list is READ FROM THE SERVED CATALOGUE, never listed here.
   *
   * `GET /elitea_core/toolkits/prompt_lib/{project}` carries each type's
   * settings schema, and the authority on "every tool this type offers" is the
   * KEY SET of `selected_tools.args_schemas` — not the SDK snapshot's own
   * `enum`, which the served schema replaces. Reading the keys is also what
   * makes this correct under a guardrail policy: a blocked tool is removed
   * from `args_schemas`, so "every tool" here means every tool the deployment
   * actually offers, and a hardcoded list would try to select one it does not.
   */
  const catalogueResponse = await request.get(TOOLKIT_TYPE_SCHEMAS);
  expect(catalogueResponse.status(), await catalogueResponse.text()).toBe(200);
  const catalogue = (await catalogueResponse.json()) as Record<
    string,
    | { properties?: { selected_tools?: { args_schemas?: Record<string, unknown> } } }
    | undefined
  >;
  const everyTool = Object.keys(
    catalogue['artifact']?.properties?.selected_tools?.args_schemas ?? {},
  ).sort();
  expect(
    everyTool.length,
    'this deployment offers no artifact tools, so the create below would select nothing',
  ).toBeGreaterThan(0);

  const bucket = autotestBucketName();
  const bucketResponse = await request.post(BUCKETS, { data: { name: bucket } });
  expect([200, 201], await bucketResponse.text()).toContain(bucketResponse.status());

  const name = autotestName('tk_artifact');
  let toolkitId = '';
  try {
    const { id } = await postToolkit(request, {
      name,
      type: 'artifact',
      // `bucket` is the only required setting the artifact type declares; the
      // pgvector and embedding references are for indexing and are left off.
      settings: { bucket, selected_tools: [...everyTool] },
    });
    toolkitId = id;
    const details = await readToolkit(request, id);
    const settings = details.settings ?? {};
    expect(settings['bucket'], 'the toolkit must point at the bucket it was given').toBe(bucket);
    expect(settings['selected_tools'], 'every offered tool must be stored').toEqual([...everyTool]);
  } finally {
    await removeToolkit(request, toolkitId);
    await request.delete(`${BUCKETS}/${bucket}`).catch(() => {});
  }
});

/* ── the toolkit details document ─────────────────────────────────────────── */

test('the toolkit details name the author, carry meta, and report agent_type and online as absent', async ({
  request,
}) => {
  let fixture: GithubToolkitFixture | undefined;
  try {
    /*
     * Through the SHARED fixture, which is also the point: three other
     * journeys depend on `createGithubToolkit` producing a usable toolkit, and
     * nothing said so. If the fixture ever stops making one, this fails here
     * rather than as an unrelated assertion in those three.
     */
    fixture = await createGithubToolkit(request, DEFAULT_PROJECT_ID, autotestName('tk_details'));
    const details = await readToolkit(request, fixture.toolkitId);

    expect(details.name, 'the details must carry the stored name').toBe(fixture.toolkitName);
    expect(details.type).toBe('github');

    /*
     * The author is ONE OBJECT with three fields, not a bare id. The editor
     * renders the author line from it, and a route that served only
     * `author_id` left that line empty — the same shape defect the agent
     * version read carries.
     */
    const author = details.author ?? {};
    for (const field of ['id', 'name', 'email']) {
      expect(author, `the author object must carry ${field}`).toHaveProperty(field);
    }
    expect(details.author_id, 'author_id stays alongside the object').toBeDefined();

    /*
     * `meta` is a JSON OBJECT, never null and never an array: a null in the
     * sibling configuration column is what made a whole credentials listing
     * answer 500 for every member of a project, permanently, from one row.
     */
    expect(details.meta, 'meta must not be null').not.toBeNull();
    expect(Array.isArray(details.meta), 'meta must not be an array').toBe(false);
    expect(typeof details.meta, 'meta must be an object').toBe('object');

    /*
     * `agent_type` and `online` describe an AGENT participant, and a plain
     * toolkit is not one. They are present and null rather than absent, and
     * the client reads them with `??`, so a route that omitted the keys and a
     * route that answered a value would both change what the toolkit list
     * renders.
     */
    expect(details.agent_type, 'a plain toolkit has no agent type').toBeNull();
    expect(details.online, 'a plain toolkit has no online state').toBeNull();
  } finally {
    await deleteGithubToolkit(request, DEFAULT_PROJECT_ID, fixture);
  }
});

test('the toolkit details expose the identifier the runtime addresses the toolkit by', async ({
  request,
}) => {
  /*
   * THE DEFECT THIS CASE FOUND.
   *
   * `toolkit_name` is not a display name — it is what an agent's instructions
   * and the SDK's tool registry call this toolkit. The runtime rule keeps
   * `_`, `.` and `-` and folds the `.` into `_`; the details route kept
   * alphanumerics only, so it answered `autotestgithubtoolkitv1` for a toolkit
   * every runtime path addresses as `autotestgithub-toolkit_v1`.
   *
   * The expected value is written out in full rather than computed, on
   * purpose: a helper shared with the code under test would agree with
   * whatever that code does.
   */
  const credential = await createConfiguration(request, 'credentials', {
    title: autotestName('tkcred_name'),
    data: { base_url: PLACEHOLDER_BASE_URL },
  });
  const suffix = Math.random().toString(36).slice(2, 8);
  const storedName = `${AUTOTEST_PREFIX}github toolkit-x.v1_${suffix}`;
  const runtimeName = `${AUTOTEST_PREFIX}githubtoolkit-x_v1_${suffix}`;
  let toolkitId = '';
  try {
    const { id } = await postToolkit(request, {
      name: storedName,
      type: 'github',
      settings: {
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        github_configuration: { elitea_title: credential.title, private: false },
      },
    });
    toolkitId = id;
    const details = await readToolkit(request, id);
    // The two fields answer different questions, and both must be right: the
    // display name is what the user typed, untouched.
    expect(details.name, 'the stored name is served as it was written').toBe(storedName);
    expect(
      details.toolkit_name,
      'the details must report the identifier the runtime uses, not an alphanumeric squash',
    ).toBe(runtimeName);
  } finally {
    await removeToolkit(request, toolkitId);
    await deleteConfiguration(request, credential.id);
  }
});
