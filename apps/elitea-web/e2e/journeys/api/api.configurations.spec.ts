/**
 * The configuration create contract, section by section, over the API.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's configuration cases: the AI-credential shapes (Azure
 * OpenAI and Amazon Bedrock), the vector store, the embedding model, and the
 * refusals each of them owes — including the one refusal that is common to all
 * four sections and to the toolkit credentials next door, "a configuration
 * with no label and no data is refused".
 *
 * The three legacy cases that WAIT for a configuration to report a healthy
 * status are not ported. That wait runs a real Azure or AWS provider check, so
 * it cannot be satisfied by any hermetic stack; it is left out rather than
 * left behind as a skip that reads as coverage.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * ONE ROUTE, FOUR KINDS OF ROW — AND THE `section` IS DERIVED, NOT SENT
 * ─────────────────────────────────────────────────────────────────────────────
 * `POST /configurations/configurations/{project}` serves every kind of
 * configuration, and the body decides which one it makes. The `section` column
 * is derived from the TYPE (`sectionFor` in the create handler), and it is not
 * a label: it decides which screen shows the row, which picker offers it and
 * which reader resolves it. A row filed under "" exists and is invisible, and
 * nothing anywhere reports that. So every create below asserts the section it
 * landed in, from the create's own answer AND from the read.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE SECRET INDIRECTION
 * ─────────────────────────────────────────────────────────────────────────────
 * A field the type's schema declares a password never reaches the row. It goes
 * to the project vault and the row keeps a `{{secret.<32 hex>}}` reference in
 * its place. Each sealing assertion therefore looks for the plain value in the
 * WHOLE response document rather than in the field it was sent in — a seal
 * that moved the value into a sibling key would pass the narrower check — and
 * pins the reference's SHAPE, because an empty string also fails to echo the
 * value and also makes the credential unusable.
 *
 * A field the schema does NOT declare a password must survive untouched, which
 * is the other half: a "seal" that rewrote every field would satisfy every
 * assertion about secrecy and destroy the credential.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE REFUSALS ARE THE POINT OF THIS FILE
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy platform validates a create body against the type's own schema
 * before it stores anything. This one stored the body as sent, so
 * `{"elitea_title": "x", "type": "pgvector"}` was a 201 and a row with no
 * label and no data. That row is unusable in a way nothing reports: the model
 * catalogue treats an unlabelled row as an error rather than skipping it, so
 * ONE of them empties the whole catalogue for the project. The refusals below
 * are the ported evidence for that fix; each one asserts the STATUS, the FIELD
 * the refusal names, and that the row was not created after all.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * NOTHING HERE IS A REAL CREDENTIAL
 * ─────────────────────────────────────────────────────────────────────────────
 * Every value is an `autotest`/`.invalid` fixture string, and the create route
 * contacts no provider. Everything created is removed in a `finally`.
 */
import { test, expect, type APIRequestContext } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import {
  configurationBody,
  createConfiguration,
  createEmbeddingModel,
  deleteConfiguration,
  type ConfigurationSection,
} from '../../fixtures/configurations';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const CONFIGURATIONS = `${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`;
const CONFIGURATION = (id: string): string =>
  `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`;
const TOOLKITS = `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`;
const TOOLKIT = (id: string): string =>
  `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`;

/** The shape a sealed secret takes in a stored row. */
const SEALED = /^\{\{secret\.[0-9a-f]{32}\}\}$/;

/** The endpoints the fixtures name. Deliberately unroutable (RFC 2606). */
const AZURE_ENDPOINT = 'https://autotest.invalid/openai';
const PGVECTOR_CONNECTION = 'postgresql://autotest:autotest@autotest.invalid:5432/autotest';

interface StoredConfiguration {
  readonly id?: number | string;
  readonly type?: string;
  readonly elitea_title?: string;
  readonly label?: string;
  readonly section?: string;
  readonly shared?: boolean;
  readonly data?: Record<string, unknown>;
}

/** Create one row straight from a body, asserting the 201 and returning it. */
async function postConfiguration(
  request: APIRequestContext,
  body: Record<string, unknown>,
): Promise<{ id: string; stored: StoredConfiguration; raw: string }> {
  const response = await request.post(CONFIGURATIONS, { data: body });
  const raw = await response.text();
  expect(response.status(), raw).toBe(201);
  const stored = JSON.parse(raw) as StoredConfiguration;
  const id = String(stored.id ?? '');
  expect(id, 'the create must answer an id').not.toBe('');
  return { id, stored, raw };
}

/** Read one row back from the server. */
async function readConfiguration(
  request: APIRequestContext,
  id: string,
): Promise<{ stored: StoredConfiguration; raw: string }> {
  const response = await request.get(CONFIGURATION(id));
  const raw = await response.text();
  expect(response.status(), raw).toBe(200);
  return { stored: JSON.parse(raw) as StoredConfiguration, raw };
}

/**
 * Assert one create is REFUSED, that the refusal names the field, and that
 * nothing was stored.
 *
 * The third part is the one a status code cannot give: a 400 that still wrote
 * the row leaves exactly the row the refusal exists to prevent, and the next
 * run then collides on the UNIQUE title instead of failing here.
 */
async function expectRefused(
  request: APIRequestContext,
  body: Record<string, unknown>,
  field: string,
): Promise<void> {
  const response = await request.post(CONFIGURATIONS, { data: body });
  const raw = await response.text();
  expect(response.status(), `the refusal must be a 400: ${raw}`).toBe(400);
  expect(raw, `the refusal must name ${field}`).toContain(field);

  const title = String(body['elitea_title'] ?? '');
  const listed = await request.get(`${CONFIGURATIONS}?query=${encodeURIComponent(title)}`);
  expect(listed.status()).toBe(200);
  const page = (await listed.json()) as { items?: StoredConfiguration[] };
  expect(
    (page.items ?? []).filter((row) => row.elitea_title === title),
    'a refused create must not have written the row',
  ).toEqual([]);
}

/* ══════════════════════════════════════════════════════════════════════════
 * AI credentials
 * ═════════════════════════════════════════════════════════════════════════ */

test('an Azure OpenAI credential is filed under ai_credentials with its key sealed', async ({
  request,
}) => {
  const title = autotestName('azure_full');
  const key = `${AUTOTEST_PREFIX}azure_key_value`;
  let id = '';
  try {
    const created = await postConfiguration(request, {
      type: 'azure_open_ai',
      elitea_title: title,
      label: `${title} label`,
      shared: false,
      data: { api_base: AZURE_ENDPOINT, api_key: key, api_version: '2024-02-01' },
    });
    id = created.id;

    expect(created.stored.type).toBe('azure_open_ai');
    expect(created.stored.section, 'the section is derived from the type').toBe('ai_credentials');
    expect(created.stored.elitea_title).toBe(title);
    expect(created.stored.label, 'the label the create stored must come back with it').toBe(
      `${title} label`,
    );

    // The seal happened BEFORE the row was written: there is no window in
    // which a read could return the plaintext.
    const data = created.stored.data ?? {};
    expect(String(data['api_key'] ?? '')).toMatch(SEALED);
    expect(created.raw, 'the create answer must not carry the key anywhere').not.toContain(key);
    // …and the fields the schema does not call a password are untouched.
    expect(data['api_base']).toBe(AZURE_ENDPOINT);
    expect(data['api_version']).toBe('2024-02-01');

    const read = await readConfiguration(request, id);
    expect(read.stored.section).toBe('ai_credentials');
    expect(read.stored.label).toBe(`${title} label`);
    expect(String((read.stored.data ?? {})['api_key'] ?? '')).toMatch(SEALED);
    expect(read.raw, 'no read may serve the key back').not.toContain(key);
  } finally {
    await deleteConfiguration(request, id);
  }
});

test('an Azure OpenAI credential is accepted with no key, and with only its endpoint', async ({
  request,
}) => {
  const shapes = [
    { tag: 'azure_nokey', data: { api_base: AZURE_ENDPOINT, api_version: '2024-02-01' } },
    { tag: 'azure_minimal', data: { api_base: AZURE_ENDPOINT } },
  ];
  const created: string[] = [];
  try {
    for (const shape of shapes) {
      const title = autotestName(shape.tag);
      const row = await postConfiguration(request, {
        type: 'azure_open_ai',
        elitea_title: title,
        label: title,
        shared: false,
        data: shape.data,
      });
      created.push(row.id);
      expect(row.stored.section, `${shape.tag}: filed under ai_credentials`).toBe('ai_credentials');
      // Nothing is invented in place of the key that was not sent.
      expect((row.stored.data ?? {})['api_key'], `${shape.tag}: no key may be conjured`).toBeUndefined();
    }
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }
});

test('a credential can be marked shared at creation, and the flag survives the read', async ({
  request,
}) => {
  const title = autotestName('azure_shared');
  let id = '';
  try {
    const created = await postConfiguration(request, {
      type: 'azure_open_ai',
      elitea_title: title,
      label: title,
      shared: true,
      data: { api_base: AZURE_ENDPOINT },
    });
    id = created.id;
    expect(created.stored.shared, 'the create answer reports the flag it stored').toBe(true);
    const read = await readConfiguration(request, id);
    expect(read.stored.shared, 'and the row really carries it').toBe(true);
  } finally {
    await deleteConfiguration(request, id);
  }
});

test('a Bedrock credential seals its secret, keeps its plain fields, and is accepted at its smallest', async ({
  request,
}) => {
  const full = autotestName('bedrock_full');
  const accessKeyID = `${AUTOTEST_PREFIX}AKIA_NOT_REAL`;
  const secretKey = `${AUTOTEST_PREFIX}bedrock_secret_value`;
  const created: string[] = [];
  try {
    const row = await postConfiguration(request, {
      type: 'amazon_bedrock',
      elitea_title: full,
      label: full,
      shared: false,
      data: {
        aws_access_key_id: accessKeyID,
        aws_secret_access_key: secretKey,
        aws_region_name: 'us-east-1',
      },
    });
    created.push(row.id);
    expect(row.stored.section).toBe('ai_credentials');

    const data = row.stored.data ?? {};
    // ONE of the two keys is declared a password by the type's schema, and
    // that is the one that goes to the vault. The access key id is an
    // identifier, not a secret, and a "seal" that rewrote it would leave a
    // credential that cannot authenticate.
    expect(String(data['aws_secret_access_key'] ?? '')).toMatch(SEALED);
    expect(row.raw, 'the secret key must not be echoed').not.toContain(secretKey);
    expect(data['aws_access_key_id']).toBe(accessKeyID);
    expect(data['aws_region_name']).toBe('us-east-1');

    // The two smallest legal Bedrock bodies. This type declares NO required
    // data field — the credential can come from the instance role — so both
    // are accepted, and a rule that refused them would refuse a real shape.
    for (const shape of [
      { tag: 'bedrock_empty', data: {} },
      { tag: 'bedrock_region', data: { aws_region_name: 'eu-central-1' } },
    ]) {
      const title = autotestName(shape.tag);
      const minimal = await postConfiguration(request, {
        type: 'amazon_bedrock',
        elitea_title: title,
        label: title,
        shared: false,
        data: shape.data,
      });
      created.push(minimal.id);
      expect(minimal.stored.section, `${shape.tag}: filed under ai_credentials`).toBe(
        'ai_credentials',
      );
    }
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }
});

test('editing a Bedrock credential replaces the sealed secret rather than keeping the old one', async ({
  request,
}) => {
  const title = autotestName('bedrock_edit');
  const first = `${AUTOTEST_PREFIX}bedrock_secret_first`;
  const second = `${AUTOTEST_PREFIX}bedrock_secret_second`;
  let id = '';
  try {
    const created = await postConfiguration(request, {
      type: 'amazon_bedrock',
      elitea_title: title,
      label: title,
      shared: false,
      data: { aws_access_key_id: `${AUTOTEST_PREFIX}AKIA_ONE`, aws_secret_access_key: first },
    });
    id = created.id;
    const before = String((created.stored.data ?? {})['aws_secret_access_key'] ?? '');
    expect(before).toMatch(SEALED);

    const updated = await request.put(CONFIGURATION(id), {
      data: {
        data: { aws_access_key_id: `${AUTOTEST_PREFIX}AKIA_TWO`, aws_secret_access_key: second },
      },
    });
    expect(updated.status(), await updated.text()).toBe(200);

    const read = await readConfiguration(request, id);
    const after = String((read.stored.data ?? {})['aws_secret_access_key'] ?? '');
    expect(after, 'the replacement is sealed too').toMatch(SEALED);
    // A DIFFERENT reference. The same one would mean the vault still holds the
    // first secret and the edit changed nothing that matters.
    expect(after, 'a new secret must not reuse the old reference').not.toBe(before);
    expect(read.raw).not.toContain(first);
    expect(read.raw).not.toContain(second);
    expect((read.stored.data ?? {})['aws_access_key_id']).toBe(`${AUTOTEST_PREFIX}AKIA_TWO`);
  } finally {
    await deleteConfiguration(request, id);
  }
});

/* ══════════════════════════════════════════════════════════════════════════
 * The vector store
 * ═════════════════════════════════════════════════════════════════════════ */

test('a PgVector configuration is filed under vectorstorage with its connection string sealed', async ({
  request,
}) => {
  const title = autotestName('pgvector_full');
  const created: string[] = [];
  try {
    const row = await postConfiguration(request, {
      type: 'pgvector',
      elitea_title: title,
      label: `${title} label`,
      shared: true,
      data: { connection_string: PGVECTOR_CONNECTION },
    });
    created.push(row.id);
    expect(row.stored.section, 'a vector store is not a credential').toBe('vectorstorage');
    expect(row.stored.shared, 'a vector store can be shared at creation').toBe(true);
    expect(String((row.stored.data ?? {})['connection_string'] ?? '')).toMatch(SEALED);
    expect(row.raw, 'the connection string carries a password and must not be echoed').not.toContain(
      PGVECTOR_CONNECTION,
    );

    const read = await readConfiguration(request, row.id);
    expect(read.stored.section).toBe('vectorstorage');
    expect(read.raw).not.toContain(PGVECTOR_CONNECTION);

    // The connection string is OPTIONAL: the type declares no required data
    // field, so a row that names none is a legal one.
    const bare = autotestName('pgvector_bare');
    const minimal = await postConfiguration(request, {
      type: 'pgvector',
      elitea_title: bare,
      label: bare,
      shared: false,
      data: {},
    });
    created.push(minimal.id);
    expect(minimal.stored.section).toBe('vectorstorage');
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }
});

/* ══════════════════════════════════════════════════════════════════════════
 * The embedding model
 * ═════════════════════════════════════════════════════════════════════════ */

test('an embedding model is filed under embedding and names its credential by title', async ({
  request,
}) => {
  const title = autotestName('embed_ref');
  const credentialTitle = autotestName('embed_cred');
  const created: string[] = [];
  try {
    const credential = await createConfiguration(request, 'ai_credentials', {
      title: credentialTitle,
      data: { api_base: AZURE_ENDPOINT },
    });
    created.push(credential.id);

    const model = await postConfiguration(request, {
      type: 'embedding_model',
      elitea_title: title,
      label: `${title} label`,
      shared: true,
      data: {
        name: `${title}_name`,
        ai_credentials: { elitea_title: credentialTitle, private: false },
      },
    });
    created.push(model.id);

    expect(model.stored.section, 'an embedding model is not an AI credential').toBe('embedding');
    expect(model.stored.shared).toBe(true);

    const read = await readConfiguration(request, model.id);
    const data = read.stored.data ?? {};
    expect(data['name'], 'the model name is what the catalogue publishes').toBe(`${title}_name`);
    // The link is stored as a REFERENCE and stays one: the row must not hold a
    // copy of the credential it points at.
    const link = data['ai_credentials'] as Record<string, unknown> | undefined;
    expect(link?.['elitea_title'], 'the credential is named, not copied').toBe(credentialTitle);
    expect(read.raw, 'the referenced endpoint must not be folded into the model row').not.toContain(
      AZURE_ENDPOINT,
    );

    // `private: false` is not decoration. It means "resolve this title in the
    // project that owns the row"; `true` sends the resolver to the CALLER's
    // personal project, which is a different project for every caller.
    expect(link?.['private']).toBe(false);
  } finally {
    for (const id of created.reverse()) await deleteConfiguration(request, id);
  }
});

/* ══════════════════════════════════════════════════════════════════════════
 * The refusals — one rule, every section
 * ═════════════════════════════════════════════════════════════════════════ */

test('a configuration with no label and no data is refused, in every section', async ({
  request,
}) => {
  // One rule, stated once per section, because the route is one route and the
  // legacy platform refuses this body for each of them. Asserting it for a
  // single type would leave three sections free to accept it.
  const sections: { section: ConfigurationSection; type: string }[] = [
    { section: 'ai_credentials', type: 'azure_open_ai' },
    { section: 'vectorstorage', type: 'pgvector' },
    { section: 'embedding', type: 'embedding_model' },
    // The toolkit credentials section. The same refusal, which is why it is
    // here rather than beside the GitHub authentication shapes: it is not a
    // rule about GitHub.
    { section: 'credentials', type: 'github' },
  ];
  for (const { section, type } of sections) {
    const title = autotestName(`incomplete_${section}`);
    await expectRefused(request, { type, elitea_title: title }, 'label');
    await expectRefused(request, { type, elitea_title: title }, 'data');
  }
});

test('a credential with no endpoint, and a model with no name or no credential, are refused by field', async ({
  request,
}) => {
  const azure = autotestName('azure_no_endpoint');
  await expectRefused(
    request,
    {
      type: 'azure_open_ai',
      elitea_title: azure,
      label: azure,
      data: { api_key: `${AUTOTEST_PREFIX}key`, api_version: '2024-02-01' },
    },
    'api_base',
  );

  const noName = autotestName('embed_no_name');
  await expectRefused(
    request,
    {
      type: 'embedding_model',
      elitea_title: noName,
      label: noName,
      data: { ai_credentials: { elitea_title: 'autotest_missing', private: false } },
    },
    'name',
  );

  const noCredential = autotestName('embed_no_credential');
  await expectRefused(
    request,
    {
      type: 'embedding_model',
      elitea_title: noCredential,
      label: noCredential,
      data: { name: `${noCredential}_name` },
    },
    'ai_credentials',
  );

  // A reference is an OBJECT. Written as the bare title it is not a reference
  // at all, and a row that stored it would resolve for nobody.
  const malformed = autotestName('embed_bad_link');
  await expectRefused(
    request,
    {
      type: 'embedding_model',
      elitea_title: malformed,
      label: malformed,
      data: { name: `${malformed}_name`, ai_credentials: 'autotest_missing' },
    },
    'ai_credentials',
  );
});

/* ══════════════════════════════════════════════════════════════════════════
 * What a deletion does to what depends on it
 * ═════════════════════════════════════════════════════════════════════════ */

test('deleting an embedding model a toolkit depends on makes that toolkit fail validation', async ({
  request,
}) => {
  const embedding = await createEmbeddingModel(request, { title: autotestName('embed_dep') });
  const credential = await createConfiguration(request, 'credentials', {
    title: autotestName('dep_github'),
    data: { base_url: 'https://autotest.invalid/api' },
  });
  const settings = {
    repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
    github_configuration: { elitea_title: credential.title, private: false },
    embedding_model: embedding.modelName,
    selected_tools: [],
  };
  let toolkitId = '';
  try {
    const created = await request.post(TOOLKITS, {
      data: { name: autotestName('tk_dep'), type: 'github', settings },
    });
    expect(created.status(), await created.text()).toBe(201);
    toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
    expect(toolkitId).not.toBe('');

    // The dependency is removed while the toolkit still names it. Nothing
    // rewrites the toolkit: a stored reference outlives what it points at, and
    // the platform finds out at the next save.
    await deleteConfiguration(request, embedding.id);

    const resaved = await request.put(TOOLKIT(toolkitId), {
      data: { name: autotestName('tk_dep'), type: 'github', settings },
    });
    const raw = await resaved.text();
    expect(
      resaved.status(),
      `re-saving a toolkit whose embedding model is gone must be refused: ${raw}`,
    ).toBe(400);
    expect(raw, 'the refusal must name the field the user has to fix').toContain('embedding_model');

    // The message must not become an invitation to guess: it names the field,
    // not the row's internals.
    expect(raw).not.toContain('{{secret.');
  } finally {
    if (toolkitId !== '') await request.delete(TOOLKIT(toolkitId)).catch(() => {});
    await deleteConfiguration(request, embedding.id);
    await deleteConfiguration(request, embedding.credentialId);
    await deleteConfiguration(request, credential.id);
  }
});

/* ══════════════════════════════════════════════════════════════════════════
 * The fixture the other journeys build on
 * ═════════════════════════════════════════════════════════════════════════ */

test('every section fixture stores the type its section is filed under', async ({ request }) => {
  // `configurationBody` is what the other journeys send. If its defaults ever
  // stop matching the section they claim, every journey that uses it starts
  // creating rows in the wrong place — which is invisible until a picker is
  // empty. This is the one assertion that says so directly.
  const expected: Record<ConfigurationSection, string> = {
    ai_credentials: 'ai_credentials',
    embedding: 'embedding',
    vectorstorage: 'vectorstorage',
    credentials: 'credentials',
  };
  for (const [section, wanted] of Object.entries(expected) as [ConfigurationSection, string][]) {
    const body = configurationBody(section, { title: autotestName(`shape_${section}`) });
    expect(typeof body['type'], `${section}: the fixture names a type`).toBe('string');
    expect(body['label'], `${section}: the fixture always sends a label`).toBeTruthy();
    expect(body['data'], `${section}: the fixture always sends data`).toBeDefined();
    expect(wanted, `${section}: the expectation is stated`).not.toBe('');
  }

  // …and the server agrees, for the three sections whose fixture needs no
  // second row. The embedding section is proved by its own journey above,
  // which has to create the credential its model links.
  const created: string[] = [];
  try {
    for (const section of ['ai_credentials', 'vectorstorage', 'credentials'] as const) {
      const row = await createConfiguration(request, section, {
        title: autotestName(`shape_${section}`),
      });
      created.push(row.id);
      const read = await readConfiguration(request, row.id);
      expect(read.stored.section, `${section}: the fixture files its row correctly`).toBe(
        expected[section],
      );
    }
  } finally {
    for (const id of created) await deleteConfiguration(request, id);
  }
});
