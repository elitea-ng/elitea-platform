/**
 * API-H: is the API reachable, does it answer a signed-in caller, and does it
 * say so in a shape a client can read?
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * `EliteaAI/elitea-testing-public`, `automation/tests/api/test_api_health.py`
 * — three tests:
 *
 *  - `TestAPIHealth::test_api_root_reachable`  -> API-H1
 *  - `TestAPIHealth::test_api_returns_json`    -> API-H3
 *  - `TestAuthenticatedAPI::test_list_agents`  -> API-H2
 *
 * The legacy trio probes `GET /api/v2/` and asserts only `status < 500`. That
 * route does not exist on the Go platform — it answers 404 — so a transcription
 * would have asserted nothing at all while looking like a liveness check: 404
 * is under 500, and so is every other refusal. The three claims are therefore
 * rewritten against routes this platform DOES serve, keeping the use case of
 * each:
 *
 *  - API-H1 "the server is up": `/healthz`, the liveness route
 *    (`internal/api/health/handler.go`), which is what a load balancer reads.
 *    `vary: Origin` is asserted with it, because that header is the one thing
 *    that tells elitea-main's own answer apart from the SPA's catch-all: the
 *    static handler serves `index.html` with a 200 for any path it does not
 *    know, so a probe that checked only the status code would report a
 *    completely dead API as healthy.
 *  - API-H2 "an authenticated read works": the agents list, the same endpoint
 *    and the same six query parameters the legacy test sent.
 *  - API-H3 "the answer is machine-readable": an unknown route under
 *    `/api/v2/` answers 404 with a JSON `{error}` body. The legacy test asked
 *    the same question — "is the content type parseable" — of a route that
 *    answered a redirect, so it never reached its own assertion. Here it is
 *    asked of the one answer a client is most likely to have to parse and
 *    most likely to get wrong: chi's default fallback writes
 *    `404 page not found` as text/plain, which is why the router declares its
 *    own (`internal/api/router.go`, the `r.NotFound` beside the group).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Three reads, no writes.
 */
import { test, expect } from '@playwright/test';

import { API_BASE, DEFAULT_PROJECT_ID, describeRefusal } from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

// The operator persona, for the same reason the legacy suite used a token with
// project access: API-H2 asks whether a signed-in caller can read the shared
// project, and a persona without a role in it would make a permission refusal
// look like an outage.
test.use({ storageState: STORAGE_STATE.admin });

test('API-H1: the liveness route answers, and answers as the API and not as the SPA', async ({
  request,
}) => {
  const resp = await request.get('/healthz');
  expect(resp.status(), await resp.text()).toBe(200);

  // `vary: Origin` is written by the CORS middleware every API response passes
  // through, and by nothing that serves the built SPA. It is the discriminator
  // between "elitea-main answered" and "the static handler answered
  // index.html", which a status code alone cannot make.
  expect(
    resp.headers()['vary'] ?? '',
    'a /healthz without `vary: Origin` was answered by the static handler, not by elitea-main',
  ).toContain('Origin');

  expect((await resp.json()) as { status?: string }).toMatchObject({ status: 'ok' });
});

test('API-H2: a signed-in caller can list the project’s agents', async ({ request }) => {
  // The legacy test's own six parameters, kept: they are what the agents list
  // screen sends, and a route that only works without them is a route the
  // product cannot use.
  const query = new URLSearchParams({
    agents_type: 'classic',
    sort_by: 'created_at',
    sort_order: 'desc',
    query: '',
    limit: '50',
    offset: '0',
  });
  const url = `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}?${query.toString()}`;
  const resp = await request.get(url);
  expect(resp.status(), `${await resp.text()}${await describeRefusal(resp)}`).toBe(200);
  expect(resp.headers()['content-type'] ?? '').toContain('application/json');

  // An object with a `rows` array, not "a list or a dict" — the legacy
  // assertion admitted both because it ran against two platform generations.
  // This one is the shape elitea-web's own list page reads.
  const body = (await resp.json()) as { rows?: unknown; total?: unknown };
  expect(Array.isArray(body.rows), `the listing answered ${JSON.stringify(body).slice(0, 200)}`).toBe(
    true,
  );
});

test('API-H3: an unknown API route answers a typed JSON 404', async ({ request }) => {
  // A path under the versioned API that no handler claims. Deliberately not a
  // "retired" one: this asserts the FALLBACK, and a route that exists would
  // measure the route instead.
  const resp = await request.get(`${API_BASE}/elitea_core/no_such_resource/prompt_lib/${DEFAULT_PROJECT_ID}`);

  // 404 and not 401: the caller holds a credential. The router's own comment
  // records why the order matters — a 401 for a path that merely moved is read
  // by the SPA as an expired session and opens a fresh OIDC round trip on every
  // visit to the page that still calls it.
  expect(resp.status(), await resp.text()).toBe(404);
  expect(resp.headers()['content-type'] ?? '').toContain('application/json');
  expect(await resp.json()).toMatchObject({ error: expect.any(String) as unknown as string });
});
