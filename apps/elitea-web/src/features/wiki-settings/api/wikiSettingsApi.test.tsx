/**
 * The two writes. Both routes are served — unlike the artifact READ paths
 * (issue #665) — so these are real behaviours, not a staged shape.
 */
import { act } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../../wiki-browser/__tests__/testUtils';
import { useDeleteWiki, useSaveWikiSettings } from './wikiSettingsApi';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useSaveWikiSettings', () => {
  it('PUTs the WHOLE toolkit, not just the settings', async () => {
    // The route is a PUT and replaces the resource. Sending only `settings`
    // would clear every other field the toolkit carries — the reason the
    // legacy code spread the toolkit, preserved rather than tidied into a
    // PATCH the API does not serve.
    let body: Record<string, unknown> | undefined;
    server.use(
      http.put('/api/v2/elitea_core/tool/prompt_lib/1/42', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ data: {} });
      }),
    );

    const { result } = renderHookWithProviders(() => useSaveWikiSettings());
    await act(async () => {
      await result.current.mutateAsync({
        projectId: 1,
        toolkitId: 42,
        toolkit: { id: 42, name: 'deepwiki', owner: 'someone' },
        settings: { github_repository: 'acme/notes-service' },
      });
    });

    expect(body?.name).toBe('deepwiki');
    expect(body?.owner).toBe('someone');
    expect(body?.settings).toEqual({ github_repository: 'acme/notes-service' });
  });
});

describe('useDeleteWiki', () => {
  it('deletes every key in ONE request', async () => {
    // The legacy screen deleted one artifact at a time and counted successes
    // into a message, so a partial failure left a half-deleted wiki that still
    // listed.
    let requests = 0;
    let sentKeys: string[] = [];
    server.use(
      http.post('/api/v2/artifacts/objects/1/wiki-artifacts:batchDelete', async ({ request }) => {
        requests += 1;
        const body = (await request.json()) as { keys: string[] };
        sentKeys = body.keys;
        return HttpResponse.json({ deleted: body.keys, failed: [] });
      }),
    );

    const { result } = renderHookWithProviders(() => useDeleteWiki());
    let outcome;
    await act(async () => {
      outcome = await result.current.mutateAsync({
        projectId: 1,
        keys: ['w/manifest.json', 'w/pages/a.md', 'w/pages/b.md'],
      });
    });

    expect(requests).toBe(1);
    expect(sentKeys).toHaveLength(3);
    expect(outcome).toEqual({ deleted: 3, failed: [] });
  });

  it('reports which keys survived a partial delete', async () => {
    // "Deleted 3/7" as a toast is what this replaces: the caller gets the keys
    // so the screen can say what is still there.
    server.use(
      http.post('/api/v2/artifacts/objects/1/wiki-artifacts:batchDelete', () =>
        HttpResponse.json({
          // The server's real envelope (BatchDeleteObjects): the field is
          // `failed`, each entry {key, code, message}. This mock once said
          // `errors`, mirroring the hook's bug, and the test passed against
          // a shape the server never sends.
          deleted: ['w/a.md'],
          failed: [{ key: 'w/b.md', code: 'PreconditionFailed', message: 'object changed' }],
        }),
      ),
    );

    const { result } = renderHookWithProviders(() => useDeleteWiki());
    let outcome;
    await act(async () => {
      outcome = await result.current.mutateAsync({ projectId: 1, keys: ['w/a.md', 'w/b.md'] });
    });
    expect(outcome).toEqual({ deleted: 1, failed: ['w/b.md'] });
  });

  it('still reads an envelope that names the survivors `errors`', async () => {
    // Accepted alongside `failed` so a change of envelope form never again
    // turns a partial delete silent.
    server.use(
      http.post('/api/v2/artifacts/objects/1/wiki-artifacts:batchDelete', () =>
        HttpResponse.json({ deleted: [], errors: [{ key: 'w/a.md' }, { key: 'w/b.md' }] }),
      ),
    );
    const { result } = renderHookWithProviders(() => useDeleteWiki());
    let outcome;
    await act(async () => {
      outcome = await result.current.mutateAsync({ projectId: 1, keys: ['w/a.md', 'w/b.md'] });
    });
    expect(outcome).toEqual({ deleted: 0, failed: ['w/a.md', 'w/b.md'] });
  });

  it('refuses an empty batch rather than reporting a deleted wiki', async () => {
    let requests = 0;
    server.use(
      http.post('/api/v2/artifacts/objects/1/wiki-artifacts:batchDelete', () => {
        requests += 1;
        return HttpResponse.json({ deleted: [], failed: [] });
      }),
    );

    const { result } = renderHookWithProviders(() => useDeleteWiki());
    await act(async () => {
      await expect(
        result.current.mutateAsync({ projectId: 1, keys: [] }),
      ).rejects.toThrow(/no objects/i);
    });
    // Not sent at all: a request that deletes nothing and reports success
    // reads as a deleted wiki.
    expect(requests).toBe(0);
  });

  it('encodes a bucket name safely into the path', async () => {
    let url = '';
    server.use(
      http.post('*/artifacts/objects/*', ({ request }) => {
        url = new URL(request.url).pathname;
        return HttpResponse.json({ deleted: ['k'], failed: [] });
      }),
    );
    const { result } = renderHookWithProviders(() => useDeleteWiki());
    await act(async () => {
      await result.current.mutateAsync({ projectId: 1, keys: ['k'], bucket: 'odd bucket/name' });
    });
    expect(url).toContain('odd%20bucket%2Fname');
  });
});
