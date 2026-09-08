/**
 * DWIKI-015 — the `wiki_query` toolkit family, through the facade.
 *
 * THE REGRESSION. The provider DECLARED this family in its descriptor and its
 * admission table (three toolkits, `wiki_query` among them) and served none of
 * it: the host's tool table listed three tools while the admission table
 * declared seven, so `list_wikis` was admitted at the door and refused at the
 * last gate. The facade half was worse — one rewrite served all three toolkits
 * and REQUIRED `code_toolkit`, so a `wiki_query` invoke never reached the
 * provider at all: it was answered 400, "The requested code toolkit is not a
 * repository configuration in this project", for a body whose toolkit declares
 * no code toolkit by design.
 *
 * NO UI, so this drives the API. There is no wiki_query screen — the family
 * exists for AGENTS, which reach it exactly this way: POST the invoke, poll
 * the invocation, read the composed object list. Driving a browser to a page
 * that does not exist would test nothing about the thing that broke.
 *
 * `deepwiki-stack` only, and in PROVIDER_BACKED_JOURNEYS: the facade can only
 * be composed on the full standalone stack (it needs production Form
 * authentication, which the E2E stack's OIDC-only mode does not have), and
 * every assertion below is about what the provider answered.
 *
 * SHARED STATE, so it makes its own. `delete_wiki` deletes for real, so this
 * generates a wiki under a repository no other journey names, rather than
 * deleting toolkit 9001's — which DWIKI-001..003 and DWIKI-012 read — or
 * toolkit 9002's, which DWIKI-005 owns. The project runs with one worker.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { SEEDED } from './helpers';

/** The repository this journey generates under; named by nothing else. */
const OWN_REPOSITORY = 'acme/e2e-wiki-query';
const OWN_WIKI_ID = 'acme--e2e-wiki-query--main';

/** The github toolkit `code_toolkit` names (scripts/e2e-stack.sh). */
const CODE_TOOLKIT = 9010;

interface Answer {
  readonly status: number;
  readonly body: { invocation_id?: string; status?: string; result?: string; error?: string };
}

interface ResultObject {
  readonly object_type?: string;
  readonly result_target?: string;
  readonly result_encoding?: string;
  readonly data?: string;
}

/** The object keys under one wiki id, read through the artifact API. */
async function keysUnder(request: APIRequestContext, wikiId: string): Promise<string[]> {
  const response = await request.get(
    `${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${SEEDED.bucket}` +
      `?prefix=${encodeURIComponent(`${wikiId}/`)}&limit=200`,
    { failOnStatusCode: false },
  );
  if (response.status() === 404) return [];
  expect(response.ok(), `listing ${wikiId}: ${response.status()}`).toBe(true);
  const body = (await response.json()) as { objects?: { key: string }[]; items?: { key: string }[] };
  return (body.items ?? body.objects ?? []).map((o) => o.key);
}

test.describe('DeepWiki wiki_query', () => {
  // A provider round trip per tool, plus one real generation through the
  // fixture runner's paced steps.
  test.setTimeout(240_000);
  test.use({ storageState: STORAGE_STATE.member });

  /** POST one invoke and return the raw answer, without asserting on it. */
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

  /** Poll one invocation to Completed and return its result objects. */
  async function settle(
    request: APIRequestContext,
    toolkit: string,
    tool: string,
    invocationId: string,
  ): Promise<ResultObject[]> {
    let last: Answer['body'] = {};
    await expect
      .poll(
        async () => {
          const response = await request.get(
            `${BASE_URL}/api/v2/deepwiki/invocations/${SEEDED.projectId}/${toolkit}/${tool}/${invocationId}`,
            { failOnStatusCode: false },
          );
          if (!response.ok()) return `HTTP ${response.status()}`;
          last = (await response.json()) as Answer['body'];
          return last.status ?? 'unknown';
        },
        { timeout: 180_000, intervals: [1_000] },
      )
      // Completed, not "no longer Pending": an Error is terminal too, and
      // polling until "terminal" would let a failed invocation pass as a
      // settled one.
      .toBe('Completed');
    return JSON.parse(last.result ?? '[]') as ResultObject[];
  }

  /** Invoke and settle, asserting the facade accepted the body. */
  async function run(
    request: APIRequestContext,
    tool: string,
    parameters: Record<string, unknown> = {},
    configuration: Record<string, unknown> = { llm_model: 'gpt-4o-mini' },
  ): Promise<ResultObject[]> {
    const started = await invoke(request, 'wiki_query', tool, parameters, configuration);
    expect(started.status, `${tool} was refused by the facade: ${JSON.stringify(started.body)}`).toBeLessThan(300);
    const id = started.body.invocation_id;
    expect(id, `${tool} returned no invocation id: ${JSON.stringify(started.body)}`).toBeTruthy();
    return settle(request, 'wiki_query', tool, id as string);
  }

  /** The one object a wiki_query tool composes. */
  function only(objects: ResultObject[], objectType: string): string {
    expect(objects, `expected exactly one ${objectType} object`).toHaveLength(1);
    expect(objects[0].object_type).toBe(objectType);
    expect(objects[0].result_target).toBe('response');
    expect(objects[0].result_encoding).toBe('plain');
    return objects[0].data ?? '';
  }

  test('DWIKI-015: list_wikis lists the seeded wiki', async ({ request }) => {
    const listing = only(await run(request, 'list_wikis'), 'wiki_list');
    expect(listing, 'the compact listing is the legacy text').toContain('Available wikis:');
    expect(listing).toContain(SEEDED.readOnly.wikiId);

    // The metadata format is the OTHER rendering, and it carries the fields
    // the compact one drops — which is the whole reason the flag exists.
    const detailed = only(await run(request, 'list_wikis', { include_metadata: true }), 'wiki_list');
    expect(detailed).toContain('# Available Wikis');
    expect(detailed).toContain(`## ${SEEDED.readOnly.wikiId}`);
    expect(detailed).toContain(`- **Repository**: ${SEEDED.readOnly.repository}`);
  });

  test('DWIKI-015b: resolve_and_ask answers about the wiki it resolved', async ({ request }) => {
    // With a hint: no resolution at all, so this pins the RETRIEVAL half —
    // the registry read, the repo_config built from the wiki's own entry, and
    // the composed "*Querying wiki: …*" prefix.
    const question = 'Where do the wiki pages live?';
    const hinted = only(
      await run(request, 'resolve_and_ask', { question, wiki_id_hint: SEEDED.readOnly.wikiId }),
      'answer',
    );
    expect(hinted).toContain(`*Querying wiki: ${SEEDED.readOnly.wikiId}*`);
    // The fixture runner's own `ask`, so this is the answer that actually
    // travelled — not a screen rendering something else.
    expect(hinted).toContain(`Fixture answer to: ${question}`);

    // Without a hint the resolver picks. The question names the seeded
    // repository and nothing else in the bucket, so the choice is decidable
    // without a model — which is what the fixture resolver scores.
    const resolved = only(
      await run(request, 'resolve_and_ask', { question: 'How does the e2e-service route requests?' }),
      'answer',
    );
    expect(resolved).toContain(`*Querying wiki: ${SEEDED.readOnly.wikiId}*`);
  });

  test('DWIKI-015c: resolve_and_deep_research reports on the wiki it resolved', async ({ request }) => {
    const report = only(
      await run(request, 'resolve_and_deep_research', {
        question: 'How is the e2e-service put together?',
        wiki_id_hint: SEEDED.readOnly.wikiId,
        research_type: 'architecture',
      }),
      'report',
    );
    expect(report).toContain(`*Deep research on wiki: ${SEEDED.readOnly.wikiId}*`);
    // research_type reached the engine: the fixture prints it in the heading.
    expect(report).toContain('# Research report (architecture)');
  });

  test('DWIKI-015d: delete_wiki removes every key of a wiki it generated', async ({ request }) => {
    // Generated through the `Wikis` toolkit, which is the only way a wiki gets
    // into the bucket. Its repository is named by nothing else, so what this
    // deletes cannot be a wiki another journey is reading.
    const generation = await invoke(
      request,
      'wikis',
      'generate_wiki',
      { query: 'Document the wiki_query fixture repository' },
      { code_toolkit: CODE_TOOLKIT, repository: OWN_REPOSITORY, active_branch: 'main', llm_model: 'gpt-4o-mini' },
    );
    expect(generation.status, `generate_wiki was refused: ${JSON.stringify(generation.body)}`).toBeLessThan(300);
    await settle(request, 'wikis', 'generate_wiki', generation.body.invocation_id as string);

    // What LANDED, read through the artifact API rather than from the answer.
    const before = await keysUnder(request, OWN_WIKI_ID);
    expect(before.length, 'the generation put no objects in the bucket').toBeGreaterThan(0);

    const message = only(await run(request, 'delete_wiki', { wiki_id: OWN_WIKI_ID }), 'message');
    expect(message).toContain(`Wiki '${OWN_WIKI_ID}' successfully deleted.`);
    expect(message).toContain(`Objects removed: ${before.length}`);

    // The bucket is what settles it. A message claiming a deletion is exactly
    // the legacy behaviour this port replaced: every legacy delete reported
    // "completed with errors" and removed nothing, because the artifact client
    // it called had no list_artifacts at all.
    await expect
      .poll(async () => (await keysUnder(request, OWN_WIKI_ID)).length, { timeout: 30_000, intervals: [1_000] })
      .toBe(0);

    // And the neighbouring wiki is untouched: the delete is bounded by the
    // `{wiki_id}/` prefix, not by "everything in the bucket".
    expect((await keysUnder(request, SEEDED.readOnly.wikiId)).length).toBeGreaterThan(0);
  });

  test('DWIKI-015e: a tool this family does not serve is refused as invalid input', async ({ request }) => {
    // The family refuses by LABEL — "not available in wiki_query toolkit" —
    // which is a DIFFERENT refusal shape from the main family's bare "Unknown
    // tool", and the frozen error fixture keeps the two apart. A regression
    // here would show up as a generation started through a query toolkit.
    const started = await invoke(request, 'wiki_query', 'generate_wiki', {}, { llm_model: 'gpt-4o-mini' });
    if (started.status < 300 && started.body.invocation_id !== undefined) {
      const response = await request.get(
        `${BASE_URL}/api/v2/deepwiki/invocations/${SEEDED.projectId}/wiki_query/generate_wiki/` +
          `${started.body.invocation_id}`,
        { failOnStatusCode: false },
      );
      expect(JSON.stringify(await response.json())).toContain('wiki_query toolkit');
      return;
    }
    expect(started.status, JSON.stringify(started.body)).toBeGreaterThanOrEqual(400);
  });
});
