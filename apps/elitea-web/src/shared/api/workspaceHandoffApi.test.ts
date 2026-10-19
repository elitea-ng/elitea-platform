/**
 * The three handoff routes the web uses, through the real `eliteaFetch` over
 * MSW: `eliteaFetch` returns the envelope, so a client that types it as the
 * body reads `undefined` from a 200 (the #132 shape). These pin that the
 * body comes back, and the method, path and body that go out.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { server } from '@/test/setup';

import { configureGeneratedClient, resetGeneratedClient } from './generated/mutator';
import { eraseWorkspaceHandoff, getWorkspaceHandoff, requestWorkspaceReclaim } from './workspaceHandoffApi';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('workspaceHandoffApi', () => {
  it('reads the banner state as the body, not the envelope', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, () =>
        HttpResponse.json({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true, device_name: 'Laptop', delete_after: null }),
      ),
    );
    await expect(getWorkspaceHandoff(42, UUID)).resolves.toMatchObject({ state: 'CLOUD_RUNNING', epoch: 2, reclaim_pending: true, device_name: 'Laptop' });
  });

  it('reads a thread with no handoff as local', async () => {
    server.use(http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, () => HttpResponse.json({ state: 'LOCAL', epoch: 0 })));
    await expect(getWorkspaceHandoff('42', UUID)).resolves.toEqual({ state: 'LOCAL', epoch: 0 });
  });

  it('erases with DELETE and returns the new state', async () => {
    let method = '';
    server.use(
      http.delete(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, ({ request }) => {
        method = request.method;
        return HttpResponse.json({ state: 'LOCAL', epoch: 5 });
      }),
    );
    await expect(eraseWorkspaceHandoff(42, UUID)).resolves.toEqual({ state: 'LOCAL', epoch: 5 });
    expect(method).toBe('DELETE');
  });

  it('asks for a take back with POST to the reclaim-request route', async () => {
    let hit = '';
    server.use(
      http.post(`${BASE}/elitea_core/workspace_handoff_reclaim_request/prompt_lib/42/${UUID}`, ({ request }) => {
        hit = `${request.method} ${new URL(request.url).pathname}`;
        return HttpResponse.json({}, { status: 202 });
      }),
    );
    await requestWorkspaceReclaim(42, UUID);
    expect(hit).toBe(`POST ${BASE}/elitea_core/workspace_handoff_reclaim_request/prompt_lib/42/${UUID}`);
  });

  it('rejects when the server refuses (a non-owner)', async () => {
    server.use(http.post(`${BASE}/elitea_core/workspace_handoff_reclaim_request/prompt_lib/42/${UUID}`, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
    await expect(requestWorkspaceReclaim(42, UUID)).rejects.toBeDefined();
  });
});
