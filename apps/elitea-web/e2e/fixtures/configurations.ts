/**
 * Configuration (credential) fixtures for the API journeys.
 *
 * ## Why these are here
 *
 * `POST /configurations/configurations/{project}` is ONE route serving four
 * different kinds of row, and which kind a body makes is decided entirely by
 * its `type`. The route then files the row under a `section`, and the section
 * decides which screen shows it, which picker offers it and which reader
 * resolves it. Every journey that needed a credential used to hand-roll that
 * body, so the section rule lived in nobody's code and each copy re-learnt it.
 * `CONFIGURATION_SECTIONS` below states it once.
 *
 * ## The `private` trap
 *
 * A stored reference to one of these rows is an OBJECT — `{elitea_title,
 * private}` — and `private: true` does NOT mean "not shared". It means
 * "resolve this title in the CALLER's PERSONAL project"
 * (`internal/application/configurations/expand.go`). So a row created here,
 * in the project under test, must be referenced with `private: false`: a
 * `true` there answers `configuration_not_found` for a row that plainly
 * exists, and it only LOOKS right in a setup where the project under test
 * happens to be the caller's own personal project. `e2e/fixtures/api.ts`'s
 * `createGithubToolkit` writes the correct shape; copy that one.
 *
 * ## Cleanup
 *
 * Everything made here is `autotest_*`-named and `deleteConfiguration` is
 * best-effort, so it can run in a `finally`.
 */
import type { APIRequestContext } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from './api';

/**
 * The four families of configuration row, named as the platform files them.
 *
 * `models` and `embedding` are both LLM-provider rows; they differ in what the
 * row is FOR, which is why the platform keeps them apart and why a journey has
 * to say which one it wants.
 */
export type ConfigurationSection = 'ai_credentials' | 'embedding' | 'vectorstorage' | 'credentials';

/** What one section needs in its `data` before the route will store the row. */
interface SectionShape {
  /** The provider type this section's rows are created with by default. */
  readonly defaultType: string;
  /** The keys the route requires for that type, with placeholder values. */
  readonly placeholderData: Readonly<Record<string, unknown>>;
}

/*
 * The placeholders are DELIBERATELY unroutable. Every value here is an
 * `autotest`/`.invalid` string: a journey that accidentally reached a real
 * endpoint would be a journey whose result depends on somebody else's
 * service, and `e2e/live/` is where a real credential belongs.
 */
const CONFIGURATION_SECTIONS: Readonly<Record<ConfigurationSection, SectionShape>> = {
  // A model provider the chat and the model picker read.
  ai_credentials: {
    defaultType: 'open_ai',
    placeholderData: {
      api_base: 'https://autotest.invalid/v1',
      api_key: `${AUTOTEST_PREFIX}not_a_real_key`,
    },
  },
  // An embedding MODEL, which is a different kind of row from the credential
  // it links: `embedding_model` under `embedding`, carrying a model name and a
  // reference to an `ai_credentials` row. It used to be declared here as an
  // `open_ai` credential, which the create route files under `ai_credentials`
  // — so the "embedding" fixture made a row in another section, the embedding
  // catalogue stayed empty, and a toolkit naming that model was refused for a
  // configuration the journey had just created. Use `createEmbeddingModel`,
  // which makes the credential too.
  embedding: {
    defaultType: 'embedding_model',
    placeholderData: {},
  },
  // A vector store an index writes into.
  vectorstorage: {
    defaultType: 'pgvector',
    placeholderData: {
      connection_string: 'postgresql://autotest:autotest@autotest.invalid:5432/autotest',
    },
  },
  // A toolkit's credential — the section `createGithubToolkit` writes into.
  credentials: {
    defaultType: 'github',
    placeholderData: { base_url: 'https://autotest.invalid/api' },
  },
};

/** What a caller wants stored, over the section's own defaults. */
export interface ConfigurationInput {
  /** `elitea_title` — the name a toolkit or a model reference resolves BY. */
  readonly title: string;
  /** The provider type. Defaults to the section's own. */
  readonly type?: string;
  /** The display label. Defaults to the title. */
  readonly label?: string;
  /** Replaces the section's placeholder data. Omit to take the placeholder. */
  readonly data?: Readonly<Record<string, unknown>>;
  /** Whether every project may resolve this row. Defaults to false. */
  readonly shared?: boolean;
}

/**
 * The request body for one configuration, with its section's requirements
 * filled in.
 *
 * Exported separately from `createConfiguration` because a refusal case needs
 * to author a body and BREAK one field of it, and re-deriving the other three
 * by hand is how a "refused" result stops proving which field was refused.
 */
export function configurationBody(
  section: ConfigurationSection,
  input: ConfigurationInput,
): Record<string, unknown> {
  const shape = CONFIGURATION_SECTIONS[section];
  return {
    type: input.type ?? shape.defaultType,
    elitea_title: input.title,
    label: input.label ?? input.title,
    shared: input.shared ?? false,
    // REPLACED, not merged: a caller that supplies its own data is
    // supplying the whole of it, and folding a placeholder `base_url` or
    // `api_key` underneath a real one would build a credential that is
    // half this fixture's invention.
    data: input.data ?? shape.placeholderData,
  };
}

/** A stored configuration, as its callers address it afterwards. */
export interface CreatedConfiguration {
  readonly id: string;
  /** The title a reference resolves by — NOT the id. */
  readonly title: string;
}

/**
 * A stored embedding model, and the credential it links.
 *
 * `modelName` is separate from `title` ON PURPOSE, and they are deliberately
 * different strings. A toolkit's `embedding_model` field is matched against the
 * model CATALOGUE, and the catalogue publishes `data.name`
 * (`services/elitea-main/internal/infra/db/repos/models.go`) — not the row's
 * `elitea_title`. A fixture that returned one value for both would let a
 * journey pass the title and never notice.
 */
export interface CreatedEmbeddingModel extends CreatedConfiguration {
  /** The name a toolkit's `embedding_model` field must carry. */
  readonly modelName: string;
  /** The `ai_credentials` row this model links. Delete it too. */
  readonly credentialId: string;
}

export async function createConfiguration(
  request: APIRequestContext,
  section: ConfigurationSection,
  input: ConfigurationInput,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<CreatedConfiguration> {
  const url = `${API_BASE}/configurations/configurations/${projectId}`;
  const response = await request.post(url, { data: configurationBody(section, input) });
  if (!response.ok()) {
    throw new Error(
      `createConfiguration(${section}): POST ${url} -> ${response.status()}: ` +
        `${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { id?: string | number };
  const id = String(body.id ?? '');
  if (id === '') {
    throw new Error(`createConfiguration(${section}): the create answered no id`);
  }
  return { id, title: input.title };
}

/**
 * Create an embedding model the project can really see.
 *
 * THREE things have to be true before a toolkit may name one, and each is
 * decided somewhere else:
 *
 *   1. the row is `type: "embedding_model"`, so the create route files it under
 *      the `embedding` section — the only section the embedding catalogue reads;
 *   2. it links an `ai_credentials` row that EXISTS, because the write route
 *      resolves the reference and stores `status_ok = false` when it dangles;
 *   3. the catalogue selects `status_ok = true`, so (2) is what makes the model
 *      visible at all.
 *
 * So this makes the credential as well, and returns both ids. The chain is
 * pinned server-side by `TestAToolkitCanNameAnEmbeddingModelTheProjectCanSee`
 * (`services/elitea-main/internal/api`), which fails first and names the link
 * that broke.
 */
export async function createEmbeddingModel(
  request: APIRequestContext,
  input: { readonly title: string; readonly modelName?: string; readonly shared?: boolean },
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<CreatedEmbeddingModel> {
  const credentialTitle = `${input.title}_cred`;
  const credential = await createConfiguration(
    request,
    'ai_credentials',
    { title: credentialTitle, data: { api_base: 'https://autotest.invalid/v1' } },
    projectId,
  );
  const modelName = input.modelName ?? `${input.title}_name`;
  const model = await createConfiguration(
    request,
    'embedding',
    {
      title: input.title,
      shared: input.shared,
      data: {
        name: modelName,
        ai_credentials: { elitea_title: credentialTitle, private: false },
      },
    },
    projectId,
  );
  return { ...model, modelName, credentialId: credential.id };
}

/** Remove a configuration. Best effort by design — it runs in a `finally`. */
export async function deleteConfiguration(
  request: APIRequestContext,
  id: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  if (id === '') return;
  await request
    .delete(`${API_BASE}/configurations/configuration/${projectId}/${id}`)
    .catch(() => {});
}
