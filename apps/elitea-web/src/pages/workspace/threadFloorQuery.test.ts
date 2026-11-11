import { QueryClient } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import type { TurnStartRequest } from '@/shared/desktop/workspaceIpc';

import { server } from '../../test/setup';

import { rerouted, THREAD_FLOOR_STALE_MS, threadFloorQuery, withVersion } from './useSendPrompt';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;

const coder = { id: 77, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } };
const reviewer = { id: 88, entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer' }, entity_settings: { version_id: 10, executor: 'cloud' } };
const detail = { id: 77, uuid: 'c0ffee00-0000-4000-8000-000000000077', name: 'fix it', meta: { floor: { policy: 'lead', lead_participant_id: 88 } }, participants: [coder, reviewer] };

/** Answers the thread's detail (`body`) and counts the reads, per conversation id. */
function serveThread(body: (id: string) => Record<string, unknown>): Record<string, number> {
  const reads: Record<string, number> = {};
  server.use(
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/42/:id`, ({ params }) => {
      const id = String(params['id']);
      reads[id] = (reads[id] ?? 0) + 1;
      return HttpResponse.json(body(id));
    }),
  );
  return reads;
}

/** Answers `GET application` for agent 6 with `answer`, and counts the reads. */
function serveApplication(answer: () => Response): { reads: number } {
  const seen = { reads: 0 };
  server.use(
    http.get(`${BASE}/elitea_core/application/prompt_lib/42/6`, () => {
      seen.reads += 1;
      return answer();
    }),
  );
  return seen;
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('threadFloorQuery (#1928)', () => {
  it('reads a thread once per send window, and again when asked for a fresh read', async () => {
    const reads = serveThread(() => detail);
    const client = new QueryClient();
    const query = threadFloorQuery(42, '77');

    const first = await client.query(query);
    await client.query(query);
    expect(reads['77']).toBe(1);
    expect(first.agents.map((agent) => agent.participantId)).toEqual([77, 88]);
    expect(first.meta).toEqual(detail.meta);
    expect(query.staleTime).toBe(THREAD_FLOOR_STALE_MS);

    await client.query({ ...query, staleTime: 0 });
    expect(reads['77']).toBe(2);
  });

  it('keeps one copy per thread', async () => {
    const reads = serveThread(() => detail);
    const client = new QueryClient();
    await client.query(threadFloorQuery(42, '77'));
    await client.query(threadFloorQuery(42, '78'));
    expect(reads).toEqual({ '77': 1, '78': 1 });
  });
});

describe('withVersion (#1926)', () => {
  const unpinned = { addressed_to: [88], application_id: 6, version_id: 0, participant_id: 88 };

  it('runs an unpinned agent on its default version', async () => {
    serveApplication(() => HttpResponse.json({ id: 6, name: 'Reviewer', versions: [{ id: 3, name: 'base' }] }));
    await expect(withVersion(42, unpinned)).resolves.toEqual({ ...unpinned, version_id: 3 });
  });

  it('keeps the address when the default version cannot be read, so the agent named still answers', async () => {
    serveApplication(() => HttpResponse.json({ error: 'down' }, { status: 503 }));
    await expect(withVersion(42, unpinned)).resolves.toEqual({ addressed_to: [88] });
    serveApplication(() => HttpResponse.json({ id: 6, name: 'Reviewer', versions: [] }));
    await expect(withVersion(42, unpinned)).resolves.toEqual({ addressed_to: [88] });
    await expect(withVersion(42, { application_id: 6, version_id: 0, participant_id: 88 })).resolves.toEqual({});
  });

  it('leaves a pinned agent alone', async () => {
    const seen = serveApplication(() => HttpResponse.json({ id: 6, versions: [{ id: 3, name: 'base' }] }));
    await expect(withVersion(42, { ...unpinned, version_id: 10 })).resolves.toEqual({ ...unpinned, version_id: 10 });
    expect(seen.reads).toBe(0);
  });
});

describe('rerouted (#1931)', () => {
  const base: TurnStartRequest = { workspace_id: 'w', project_id: 42, conversation_id: '77', application_id: 5, version_id: 9, prompt: 'fix it', plan_mode: false, mentions: [], skills: [] };
  const refused = { ...base, participant_id: 77 };

  it('does not resend a start that falls back to the agent the refused one was on', async () => {
    // The thread now reads one agent: the retry would be the composer's agent again, with no participant named.
    serveThread(() => ({ ...detail, meta: {}, participants: [coder] }));
    await expect(rerouted(new QueryClient(), refused, 42, '77', base)).resolves.toBeUndefined();
  });

  it('resends on the agent the thread now routes to', async () => {
    serveThread(() => detail);
    await expect(rerouted(new QueryClient(), refused, 42, '77', base)).resolves.toMatchObject({ application_id: 6, version_id: 10, participant_id: 88, cloud_agent: true });
  });
});
