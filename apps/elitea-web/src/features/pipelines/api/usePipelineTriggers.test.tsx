import { waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { usePipelineTriggers } from './usePipelineTriggers';

const BASE = '/api/v2';
const PROJECT_ID = '1';
const VERSION_ID = 7;
const SCHEDULE_URL = `${BASE}/pipeline_schedules/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;
const TRIGGER_URL = `${BASE}/pipeline_triggers/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;
const REVEAL_URL = `${BASE}/pipeline_triggers/secret/prompt_lib/${PROJECT_ID}/${VERSION_ID}`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('usePipelineTriggers', () => {
  it('reads both facilities independently', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: true, active: true, cron: '0 9 * * 1' })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok' })),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));

    await waitFor(() => expect(result.current.schedule?.cron).toBe('0 9 * * 1'));
    expect(result.current.webhook?.token_id).toBe('tok');
  });

  /** "No trigger yet" is the normal state of almost every pipeline, and the backend answers it with a 200 rather than a 404. */
  it('reads an unconfigured pipeline without erroring', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));

    await waitFor(() => expect(result.current.schedule?.configured).toBe(false));
    expect(result.current.webhook?.configured).toBe(false);
  });

  it('issues nothing while the project or version is unknown', async () => {
    let read = 0;
    server.use(
      http.get(`${BASE}/pipeline_schedules/prompt_lib/*`, () => {
        read += 1;
        return HttpResponse.json({ configured: false, active: false });
      }),
      http.get(`${BASE}/pipeline_triggers/prompt_lib/*`, () => {
        read += 1;
        return HttpResponse.json({ configured: false });
      }),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(undefined, VERSION_ID));

    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(read).toBe(0);
    expect(result.current.schedule).toBeUndefined();
  });

  it('refuses to write without a scope rather than issuing a request against project 0', async () => {
    const { result } = renderHookWithProviders(() => usePipelineTriggers(undefined, undefined));
    await expect(result.current.saveSchedule('0 9 * * 1')).rejects.toThrow(/projectId\/versionId/);
  });

  it('saves the cron as an ACTIVE schedule and re-reads it', async () => {
    let saved: unknown;
    let reads = 0;
    server.use(
      http.get(SCHEDULE_URL, () => {
        reads += 1;
        return HttpResponse.json({ configured: reads > 1, active: true, cron: '0 9 * * 1' });
      }),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
      http.put(SCHEDULE_URL, async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json({ configured: true, active: true, cron: '0 9 * * 1' });
      }),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));
    await waitFor(() => expect(result.current.schedule).toBeDefined());
    await result.current.saveSchedule('0 9 * * 1');

    expect(saved).toEqual({ cron: '0 9 * * 1', active: true });
    await waitFor(() => expect(result.current.schedule?.configured).toBe(true));
  });

  it('returns the credential the create/rotate answers, which the plain read never carries', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
      http.post(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', secret: 'minted' })),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));
    await waitFor(() => expect(result.current.webhook).toBeDefined());

    const rotated = await result.current.rotateWebhook();
    expect(rotated.secret).toBe('minted');
  });

  it('reveals through the separate write-permission operation', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok' })),
      http.get(REVEAL_URL, () => HttpResponse.json({ configured: true, token_id: 'tok', secret: 'revealed' })),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));
    await waitFor(() => expect(result.current.webhook?.configured).toBe(true));
    // The read that fed `webhook` carried no secret, which is the whole point
    // of reveal being its own operation.
    expect(result.current.webhook?.secret).toBeUndefined();

    const revealed = await result.current.revealWebhook();
    expect(revealed.secret).toBe('revealed');
  });

  it('revokes and deletes through their own routes', async () => {
    let revoked = false;
    let deleted = false;
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: true, active: true, cron: '0 9 * * 1' })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: true, token_id: 'tok' })),
      http.delete(TRIGGER_URL, () => {
        revoked = true;
        return HttpResponse.json({ configured: true, token_id: 'tok', revoked_at: '2026-07-20T09:00:00Z' });
      }),
      http.delete(SCHEDULE_URL, () => {
        deleted = true;
        return HttpResponse.json({ configured: false, active: false });
      }),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));
    await waitFor(() => expect(result.current.webhook?.configured).toBe(true));

    await result.current.removeWebhook();
    await result.current.removeSchedule();
    expect(revoked).toBe(true);
    expect(deleted).toBe(true);
  });

  it('rejects with the backend error envelope when a write is refused', async () => {
    server.use(
      http.get(SCHEDULE_URL, () => HttpResponse.json({ configured: false, active: false })),
      http.get(TRIGGER_URL, () => HttpResponse.json({ configured: false })),
      http.put(SCHEDULE_URL, () => HttpResponse.json({ error: 'cron expression must have five fields' }, { status: 400 })),
    );

    const { result } = renderHookWithProviders(() => usePipelineTriggers(PROJECT_ID, VERSION_ID));
    await waitFor(() => expect(result.current.schedule).toBeDefined());

    await expect(result.current.saveSchedule('@daily')).rejects.toBeInstanceOf(Error);
  });
});
