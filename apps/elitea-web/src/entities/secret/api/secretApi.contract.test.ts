/**
 * URL contract for the secrets client (#151).
 *
 * These assertions exist because nothing else in the repo could see the
 * defect they pin. The client had invented `/secrets/{mode}/…` with a mode
 * (`prompt_lib`) the legacy plugin does not define; #137 then read the 404s
 * as a Go double-mount bug and moved the server to match, which broke every
 * consumer that had always used the real legacy shape — elitea-sdk
 * (`runtime/clients/{client,sandbox_client}.py`), admin_ui
 * (`frontend/src/api/secretsApi.js`) and qa/elitea-api-testing
 * (`utils/utils.py:322`). The unit suite had no URL assertion, the domain
 * was absent from `openapi/v2.yaml`, and the E2E journey asserted against
 * whatever the client happened to build — so all three layers agreed with
 * each other and with nothing else.
 *
 * Each `it()` below therefore registers exactly ONE msw route, the literal
 * expected URL. msw's `onUnhandledRequest: 'error'` (src/test/setup.ts)
 * turns any other URL into a failure, so these fail on drift rather than
 * silently following the client.
 *
 * The paths are the pinned baseline's, verbatim:
 * `apps/elitea-ui/src/api/secrets.js` — `apiSlicePath = '/secrets'` plus
 * `/secrets/default/<id>`, `/secret/default/<id>/<name>`,
 * `/hide/default/<id>/<name>`.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createSecret, deleteSecret, hideSecret, listSecrets, showSecret, updateSecret } from './secretApi';

const BASE = '/api/v2';
const PROJECT = 7;

/**
 * The six URLs, spelled out as literals rather than composed from the
 * client's own helpers — a test that rebuilt the path the way the source
 * does would have passed against `prompt_lib` too.
 */
const LIST = `${BASE}/secrets/secrets/default/${PROJECT}`;
const ITEM = `${BASE}/secrets/secret/default/${PROJECT}/api-key`;
const HIDE = `${BASE}/secrets/hide/default/${PROJECT}/api-key`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('secrets URL contract', () => {
  it('lists at GET /secrets/secrets/default/{projectID}', async () => {
    server.use(http.get(LIST, () => HttpResponse.json([{ name: 'api-key', secret_name: '{{secret.api-key}}', is_default: false }])));
    await expect(listSecrets(PROJECT)).resolves.toEqual([
      { name: 'api-key', secretName: '{{secret.api-key}}', isDefault: false },
    ]);
  });

  it('creates at POST /secrets/secrets/default/{projectID}', async () => {
    let body: unknown;
    server.use(
      http.post(LIST, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ name: 'api-key' }, { status: 201 });
      }),
    );
    await createSecret(PROJECT, { name: 'api-key', value: 's3cr3t' });
    expect(body).toEqual({ name: 'api-key', value: 's3cr3t' });
  });

  it('shows at GET /secrets/secret/default/{projectID}/{name}', async () => {
    server.use(http.get(ITEM, () => HttpResponse.json({ name: 'api-key', value: 's3cr3t' })));
    await expect(showSecret(PROJECT, 'api-key')).resolves.toEqual({ name: 'api-key', value: 's3cr3t' });
  });

  it('updates at PUT /secrets/secret/default/{projectID}/{name}', async () => {
    let body: unknown;
    server.use(
      http.put(ITEM, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ name: 'renamed' });
      }),
    );
    await updateSecret(PROJECT, 'api-key', { name: 'renamed', value: 's3cr3t' });
    expect(body).toEqual({ name: 'renamed', value: 's3cr3t' });
  });

  it('deletes at DELETE /secrets/secret/default/{projectID}/{name}', async () => {
    let called = false;
    server.use(
      http.delete(ITEM, () => {
        called = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await deleteSecret(PROJECT, 'api-key');
    expect(called).toBe(true);
  });

  it('hides at POST /secrets/hide/default/{projectID}/{name}', async () => {
    let called = false;
    server.use(
      http.post(HIDE, () => {
        called = true;
        return HttpResponse.json({ message: 'ok' });
      }),
    );
    await hideSecret(PROJECT, 'api-key');
    expect(called).toBe(true);
  });

  it('percent-encodes a secret name with URL-significant characters', async () => {
    let seen = '';
    server.use(
      http.get(`${BASE}/secrets/secret/default/${PROJECT}/a%2Fb%20c`, ({ request }) => {
        seen = new URL(request.url).pathname;
        return HttpResponse.json({ name: 'a/b c', value: 'v' });
      }),
    );
    await showSecret(PROJECT, 'a/b c');
    expect(seen).toBe(`${BASE}/secrets/secret/default/${PROJECT}/a%2Fb%20c`);
  });
});
