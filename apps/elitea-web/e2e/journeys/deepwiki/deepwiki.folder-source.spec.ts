/**
 * DWIKI-018 — a wiki generated from an ARTIFACT FOLDER instead of a git
 * repository.
 *
 * WHAT IS NEW. Until now the only source a wiki could have was a clone: the
 * facade resolved a `code_toolkit` into a credentialed remote, the host built
 * a `repo_config` naming one of four git providers, and the engine ran
 * `git clone`. A fifth provider type, `artifact`, names a folder in the
 * INVOKING project's own artifact store — a bucket, and optionally a prefix
 * inside it.
 *
 * WHY THIS RUNS OVER THE API. The assertions are about what the FACADE
 * accepted and what LANDED in the bucket. The browser half — the source
 * picker and the settings validator — is covered by vitest, and driving a
 * screen here would test the screen instead of the source.
 *
 * WHAT IT PROVES, AND WHAT IT DOES NOT. The fixture engine composes the
 * generation, so what this pins is the WIRING: the facade validates a folder
 * source and derives its repository, the host's extractor produces the fifth
 * provider type, the egress allowlist has nothing to refuse, and the wiki is
 * named after the folder rather than after `artifact:----…`. It does NOT
 * prove the materialiser — a fixture engine downloads nothing — which is
 * covered by the Python unit tests over a fake platform client. Per
 * `deepwiki-real-engine-acceptance`, treat a fixture-green artifact path as
 * unproven for the engine itself.
 *
 * SHARED STATE, so it makes its own. The bucket and the wiki id both carry
 * the browser project's name: chromium and webkit can run this file at the
 * same time against one stack, and a shared bucket would make each run's
 * uploads and deletions the other's flaky failure.
 */
import { expect, test, type APIRequestContext, type TestInfo } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { SEEDED } from './helpers';

/** The folder inside the source bucket. Two files, one of them nested. */
const FOLDER = 'handbook';
const FILES: readonly (readonly [string, string])[] = [
  [`${FOLDER}/overview.md`, '# Overview\n\nThe handbook explains how the service is run.\n'],
  [`${FOLDER}/guide/setup.md`, '# Setup\n\nInstall the toolchain, then run the stack.\n'],
];

/** A bucket name this run owns. Buckets are `^[a-z][a-z0-9-]{1,62}$`. */
function sourceBucket(info: TestInfo): string {
  return `dwiki018-${info.project.name.replace(/[^a-z0-9]+/g, '-')}`;
}

interface Answer {
  readonly status: number;
  readonly body: { invocation_id?: string; status?: string; result?: string; error?: string };
}

async function invoke(
  request: APIRequestContext,
  toolkit: string,
  tool: string,
  parameters: Record<string, unknown>,
  configuration: Record<string, unknown>,
): Promise<Answer> {
  const response = await request.post(
    `${BASE_URL}/api/v2/deepwiki/tools/${SEEDED.projectId}/${toolkit}/${tool}/invoke`,
    { data: { configuration: { parameters: configuration }, parameters }, failOnStatusCode: false },
  );
  let body: Answer['body'];
  try {
    body = (await response.json()) as Answer['body'];
  } catch {
    body = { error: await response.text() };
  }
  return { status: response.status(), body };
}

/** Polls one invocation to Completed — not to "no longer Pending". */
async function settle(request: APIRequestContext, tool: string, invocationId: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const response = await request.get(
          `${BASE_URL}/api/v2/deepwiki/invocations/${SEEDED.projectId}/wikis/${tool}/${invocationId}`,
          { failOnStatusCode: false },
        );
        if (!response.ok()) return `HTTP ${response.status()}`;
        const body = (await response.json()) as Answer['body'];
        return body.status ?? 'unknown';
      },
      { timeout: 180_000, intervals: [1_000] },
    )
    .toBe('Completed');
}

/** The object keys under one prefix of one bucket. */
async function keysUnder(request: APIRequestContext, bucket: string, prefix: string): Promise<string[]> {
  const response = await request.get(
    `${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${bucket}` +
      `?prefix=${encodeURIComponent(prefix)}&limit=200`,
    { failOnStatusCode: false },
  );
  if (response.status() === 404) return [];
  expect(response.ok(), `listing ${bucket}/${prefix}: ${response.status()}`).toBe(true);
  const body = (await response.json()) as { objects?: { key: string }[]; items?: { key: string }[] };
  return (body.items ?? body.objects ?? []).map((o) => o.key);
}

/** Puts the two source files in place, creating the bucket if it is absent. */
async function seedFolder(request: APIRequestContext, bucket: string): Promise<void> {
  const created = await request.post(`${BASE_URL}/api/v2/artifacts/buckets/${SEEDED.projectId}`, {
    data: { name: bucket },
    failOnStatusCode: false,
  });
  expect([200, 201, 409], `creating ${bucket}: ${await created.text()}`).toContain(created.status());

  for (const [key, body] of FILES) {
    // The multipart FILENAME is the object key, so a multi-segment key
    // survives and the folder has real depth. `overwrite=true` because the
    // route answers 409 by default and this journey re-runs.
    const uploaded = await request.post(
      `${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${bucket}?overwrite=true`,
      { multipart: { file: { name: key, mimeType: 'text/markdown', buffer: Buffer.from(body) } } },
    );
    expect([200, 201], `uploading ${key}: ${await uploaded.text()}`).toContain(uploaded.status());
  }
}

/** Removes every object this journey wrote, in both buckets. */
async function clean(request: APIRequestContext, bucket: string, wikiId: string): Promise<void> {
  for (const key of await keysUnder(request, SEEDED.bucket, `${wikiId}/`)) {
    await request.delete(`${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${SEEDED.bucket}/${key}`, {
      failOnStatusCode: false,
    });
  }
  for (const key of await keysUnder(request, bucket, `${FOLDER}/`)) {
    await request.delete(`${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${bucket}/${key}`, {
      failOnStatusCode: false,
    });
  }
}

test.describe('DeepWiki folder source', () => {
  test.setTimeout(240_000);
  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-018: a wiki is generated from an artifact folder', async ({ request }, info) => {
    const bucket = sourceBucket(info);
    const wikiId = `${bucket}--${FOLDER}--main`;
    await clean(request, bucket, wikiId);
    await seedFolder(request, bucket);

    // The two files are really there. A generation over an empty folder would
    // still compose a fixture wiki, so this is what stops the journey from
    // passing without a source at all.
    expect(await keysUnder(request, bucket, `${FOLDER}/`)).toHaveLength(FILES.length);

    // NO code_toolkit. That is the point: this body names a folder, and the
    // facade used to refuse anything without a repository configuration.
    const generation = await invoke(
      request,
      'wikis',
      'generate_wiki',
      { query: 'Document the handbook folder' },
      {
        artifact_configuration: { bucket, prefix: FOLDER },
        active_branch: 'main',
        llm_model: 'gpt-4o-mini',
      },
    );
    expect(
      generation.status,
      `a folder source was refused by the facade: ${JSON.stringify(generation.body)}`,
    ).toBeLessThan(300);
    await settle(request, 'generate_wiki', generation.body.invocation_id as string);

    // What LANDED, read through the artifact API rather than from the answer.
    const keys = await keysUnder(request, SEEDED.bucket, `${wikiId}/`);
    expect(keys.length, 'the generation put no objects in the wiki bucket').toBeGreaterThan(0);

    const manifestKey = keys.find((key) => key.includes('/wiki_manifest_'));
    expect(manifestKey, `no manifest among ${keys.join(', ')}`).toBeTruthy();
    const manifest = (await (
      await request.get(`${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${SEEDED.bucket}/${manifestKey}`)
    ).json()) as Record<string, unknown>;

    // The identity is the FOLDER's, with the `artifact://` scheme dropped.
    // Left in, `//` becomes four dashes and the wiki id — which is also this
    // object-key prefix — stops being a name.
    expect(manifest['wiki_id']).toBe(wikiId);
    expect(manifest['repository']).toBe(`${bucket}/${FOLDER}`);
    expect(manifest['provider_type']).toBe('artifact');
    expect(manifest['wiki_title']).toBe(`${FOLDER} wiki`);

    await clean(request, bucket, wikiId);
  });

  test('DWIKI-018b: a request naming two sources is refused', async ({ request }, info) => {
    // A wiki has one source. Resolving this by precedence would build a wiki
    // from material the caller did not choose, and the caller would have no
    // way to tell which one won.
    //
    // The STATUS is what a browser sees, and that is all this asserts: the
    // facade answers every refusal a caller can fix with the same message, so
    // the REASON is pinned by the Go test instead
    // (TestABodyNamingTwoSourcesIsRefused). What keeps this from passing on a
    // facade that refuses everything is DWIKI-018 above, which is the same
    // route accepting a folder.
    const refused = await invoke(
      request,
      'wikis',
      'generate_wiki',
      { query: 'Document something' },
      {
        code_toolkit: 9010,
        artifact_configuration: { bucket: sourceBucket(info), prefix: FOLDER },
        llm_model: 'gpt-4o-mini',
      },
    );
    expect(refused.status, JSON.stringify(refused.body)).toBe(400);
  });

  test('DWIKI-018c: a folder nobody could name is refused before any work starts', async ({ request }) => {
    for (const block of [{ bucket: 'Not_A_Bucket' }, { bucket: 'handbook', prefix: '../../etc' }]) {
      const refused = await invoke(
        request,
        'wikis',
        'generate_wiki',
        { query: 'Document something' },
        { artifact_configuration: block, llm_model: 'gpt-4o-mini' },
      );
      expect(refused.status, `${JSON.stringify(block)}: ${JSON.stringify(refused.body)}`).toBe(400);
    }
  });
});
