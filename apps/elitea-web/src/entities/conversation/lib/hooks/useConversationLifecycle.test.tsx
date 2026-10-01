import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useConversationLifecycle } from './useConversationLifecycle';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useConversationLifecycle', () => {
  it('createConversation resolves undefined and makes no request when projectId is undefined', async () => {
    const { result } = renderHook(() => useConversationLifecycle(undefined));
    let created;
    await act(async () => {
      created = await result.current.createConversation({ name: 'x', isPrivate: true });
    });
    expect(created).toBeUndefined();
  });

  it('createConversation POSTs and returns the server response', async () => {
    server.use(http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, () => HttpResponse.json({ id: 1, name: 'New' })));
    const { result } = renderHook(() => useConversationLifecycle(7));
    let created;
    await act(async () => {
      created = await result.current.createConversation({ name: 'New', isPrivate: true });
    });
    expect(created).toEqual({ id: 1, name: 'New' });
    expect(result.current.isCreating).toBe(false);
    expect(result.current.createError).toBeUndefined();
  });

  it('seeds model settings before the server adds its blank responder', async () => {
    let capturedBody: unknown;
    server.use(http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, async ({ request }) => {
      capturedBody = await request.json();
      return HttpResponse.json({ id: 777, name: 'New' });
    }));
    const participants = [{
      entity_name: 'dummy', entity_meta: {},
      entity_settings: { llm_settings: { model_name: 'eu.anthropic.claude-haiku', model_project_id: 1 } },
    }];
    const { result } = renderHook(() => useConversationLifecycle(7));
    await act(async () => {
      await result.current.createConversation({ name: 'New', isPrivate: true, participants });
    });
    expect(capturedBody).toEqual({ name: 'New', is_private: true, participants, meta: {} });
  });

  it('createConversation records an error and returns undefined on failure', async () => {
    server.use(http.post(`${BASE}/elitea_core/conversations/prompt_lib/7`, () => HttpResponse.json({ error: 'nope' }, { status: 500 })));
    const { result } = renderHook(() => useConversationLifecycle(7));
    let created;
    await act(async () => {
      created = await result.current.createConversation({ name: 'New', isPrivate: true });
    });
    expect(created).toBeUndefined();
    await waitFor(() => expect(result.current.createError).toBeDefined());
  });

  it('editConversation PUTs only the fields given', async () => {
    let capturedBody: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ id: 1, name: 'Renamed' });
      }),
    );
    const { result } = renderHook(() => useConversationLifecycle(7));
    await act(async () => {
      await result.current.editConversation({ id: 1, name: 'Renamed' });
    });
    expect(capturedBody).toEqual({ name: 'Renamed' });
  });

  it('deleteConversation resolves true without a network call for a playback row', async () => {
    const { result } = renderHook(() => useConversationLifecycle(7));
    let ok;
    await act(async () => {
      ok = await result.current.deleteConversation({ id: 1, isPlayback: true });
    });
    expect(ok).toBe(true);
  });

  it('deleteConversation DELETEs and resolves true on success', async () => {
    server.use(http.delete(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, () => HttpResponse.json({})));
    const { result } = renderHook(() => useConversationLifecycle(7));
    let ok;
    await act(async () => {
      ok = await result.current.deleteConversation({ id: 1 });
    });
    expect(ok).toBe(true);
  });

  it('deleteConversation resolves false on request failure', async () => {
    server.use(http.delete(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, () => HttpResponse.json({ error: 'nope' }, { status: 500 })));
    const { result } = renderHook(() => useConversationLifecycle(7));
    let ok;
    await act(async () => {
      ok = await result.current.deleteConversation({ id: 1 });
    });
    expect(ok).toBe(false);
  });

  it('selectConversation resolves undefined without a network call for a playback row', async () => {
    const { result } = renderHook(() => useConversationLifecycle(7));
    let details;
    await act(async () => {
      details = await result.current.selectConversation({ id: 1, isPlayback: true });
    });
    expect(details).toBeUndefined();
  });

  it('selectConversation fetches details and marks-selected together', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/7/1`, () => HttpResponse.json({ id: 1, name: 'Conv' })),
      http.post(`${BASE}/elitea_core/select_conversation/prompt_lib/7/1`, () => HttpResponse.json({})),
    );
    const { result } = renderHook(() => useConversationLifecycle(7));
    let details;
    await act(async () => {
      details = await result.current.selectConversation({ id: 1 });
    });
    expect(details).toEqual({ id: 1, name: 'Conv' });
  });

  it('unselectConversation DELETEs the project-scoped selection', async () => {
    let called = false;
    server.use(
      http.delete(`${BASE}/elitea_core/select_conversation/prompt_lib/7`, () => {
        called = true;
        return HttpResponse.json({});
      }),
    );
    const { result } = renderHook(() => useConversationLifecycle(7));
    await act(async () => result.current.unselectConversation());
    expect(called).toBe(true);
  });

  /**
   * Regression: `useAsyncAction`'s `run` used to depend on `[action]`, and
   * every one of the 5 actions below is a fresh inline closure each render
   * of `useConversationLifecycle` — so every returned function identity
   * changed on every re-render, defeating any consumer's own memoization
   * (e.g. a `useEffect`/`useCallback` deps array containing
   * `deleteConversation`). Found by adversarial verify.
   */
  it('keeps every action function\'s identity stable across re-renders (no referential-instability regression)', () => {
    const { result, rerender } = renderHook(({ projectId }) => useConversationLifecycle(projectId), {
      initialProps: { projectId: 7 as string | number },
    });
    const first = result.current;
    rerender({ projectId: 7 });
    const second = result.current;
    expect(second.createConversation).toBe(first.createConversation);
    expect(second.editConversation).toBe(first.editConversation);
    expect(second.deleteConversation).toBe(first.deleteConversation);
    expect(second.selectConversation).toBe(first.selectConversation);
    expect(second.unselectConversation).toBe(first.unselectConversation);
  });
});
