import { act, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { VersionSummary } from '@/entities/version';
import {
  getCheckVersionInUseMockHandler,
  getDeleteApplicationVersionMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';

import { resolveFallbackVersionId, useDeleteVersion } from './useDeleteVersion';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function baseInput() {
  return { projectId: 'p1', applicationId: 3, versionId: 7 };
}

describe('resolveFallbackVersionId', () => {
  const versions: VersionSummary[] = [
    { id: 'v1', name: 'base', status: 'draft', agentType: 'openai', createdAt: '2026-01-01T00:00:00Z' },
    { id: 'v2', name: 'feature', status: 'draft', agentType: 'openai', createdAt: '2026-01-02T00:00:00Z' },
  ];

  it('excludes the version being deleted, then falls back to LATEST_VERSION_NAME ("base")', () => {
    expect(resolveFallbackVersionId(versions, 'v2', undefined)).toBe('v1');
  });

  it('prefers the default version id when it is not the one being deleted', () => {
    expect(resolveFallbackVersionId(versions, 'v1', 'v2')).toBe('v2');
  });

  it('falls back to any remaining version when neither default nor "base" apply', () => {
    const noBase: VersionSummary[] = [
      { id: 'v1', name: 'other', status: 'draft', agentType: 'openai', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'v2', name: 'feature', status: 'draft', agentType: 'openai', createdAt: '2026-01-02T00:00:00Z' },
    ];
    expect(resolveFallbackVersionId(noBase, 'v1', undefined)).toBe('v2');
  });

  it('returns undefined when the version being deleted was the only one', () => {
    expect(resolveFallbackVersionId([versions[0] as VersionSummary], 'v1', undefined)).toBeUndefined();
  });
});

describe('useDeleteVersion', () => {
  it('doCheckVersionInUse reports isInUse=false when nothing references the version', async () => {
    server.use(getCheckVersionInUseMockHandler({ items: [], in_use: false }));
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let checked;
    await act(async () => {
      checked = await result.current.doCheckVersionInUse();
    });

    expect(checked).toEqual({ items: [], isInUse: false, referencingParents: [], replacementVersions: [] });
    await waitFor(() => expect(result.current.isCheckingInUse).toBe(false));
  });

  /**
   * #894 — `isInUse` comes off the server's OWN `in_use`, never off
   * `items.length`. The two answer opposite questions: `items` lists what
   * this version uses, `in_use` whether anything uses IT. The body below is
   * the shape that used to read as "in use" and must not: a tool of its own,
   * nothing referencing it.
   */
  it('doCheckVersionInUse does not call a version in use because it uses a tool', async () => {
    server.use(getCheckVersionInUseMockHandler({ items: [{ type: 'tool', id: 't1' }], in_use: false }));
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let checked;
    await act(async () => {
      checked = await result.current.doCheckVersionInUse();
    });

    expect(checked).toEqual({
      items: [{ type: 'tool', id: 't1' }],
      isInUse: false,
      referencingParents: [],
      replacementVersions: [],
    });
  });

  it('doCheckVersionInUse carries the referencing parents and the replacement versions', async () => {
    server.use(
      getCheckVersionInUseMockHandler({
        items: [],
        in_use: true,
        referencing_parents: [
          { application_id: 7, application_name: 'Dependent', version_id: 70, version_name: 'base', tool_id: 5 },
        ],
        replacement_versions: [{ id: 11, name: 'base', created_at: '2026-01-02T03:04:05Z' }],
      }),
    );
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let checked;
    await act(async () => {
      checked = await result.current.doCheckVersionInUse();
    });

    expect(checked).toEqual({
      items: [],
      isInUse: true,
      referencingParents: [
        { application_id: 7, application_name: 'Dependent', version_id: 70, version_name: 'base', tool_id: 5 },
      ],
      replacementVersions: [{ id: 11, name: 'base', created_at: '2026-01-02T03:04:05Z' }],
    });
  });

  it('doDeleteVersion without a replacement calls the plain DELETE endpoint and resolves true on 204', async () => {
    server.use(getDeleteApplicationVersionMockHandler());
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let deleted;
    await act(async () => {
      deleted = await result.current.doDeleteVersion();
    });

    expect(deleted).toEqual({ ok: true, errorMessage: undefined });
    await waitFor(() => expect(result.current.isDeletingVersion).toBe(false));
  });

  /**
   * #894 — the replacement goes to the DELETE route as
   * `replacement_version_id`, not to `batch_replace_version`. That other
   * endpoint repoints the rows whose `entity_tool_mapping.entity_version_id`
   * IS the old version — the tools the deleted version used — and leaves the
   * parents that reference it pointing at a row that is about to disappear.
   */
  it('doDeleteVersion with a replacement sends replacement_version_id on the DELETE', async () => {
    let seenUrl: string | undefined;
    server.use(
      http.delete('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', ({ request }) => {
        seenUrl = request.url;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let deleted;
    await act(async () => {
      deleted = await result.current.doDeleteVersion(11);
    });

    expect(deleted).toEqual({ ok: true, errorMessage: undefined });
    expect(seenUrl).toContain('replacement_version_id=11');
  });

  /**
   * #147 — the OUTCOME carries the message, not only the hook's state.
   *
   * The server's refusal must be readable in the same tick the call resolves.
   * A caller reads its own `errorMessage` closure from the render BEFORE the
   * failure, which is `undefined`, and that is why "Unpublish first." never
   * reached a user. Both channels are asserted below; the outcome is the one
   * a confirm dialog can act on.
   */
  it('doDeleteVersion reports the server\'s own refusal on the outcome and in state', async () => {
    server.use(
      http.delete('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'Unpublish first. Cannot delete a published version.' }, { status: 400 }),
      ),
    );
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let deleted;
    await act(async () => {
      deleted = await result.current.doDeleteVersion();
    });

    expect(deleted).toEqual({
      ok: false,
      errorMessage: 'Unpublish first. Cannot delete a published version.',
    });
    await waitFor(() => expect(result.current.error).toBeDefined());
    // NOT the `eliteaFetch: 400 from <url>` diagnostic `EliteaApiError.message`
    // carries — that string used to be the whole of what a user was shown.
    expect(result.current.errorMessage).toBe('Unpublish first. Cannot delete a published version.');
  });
});
