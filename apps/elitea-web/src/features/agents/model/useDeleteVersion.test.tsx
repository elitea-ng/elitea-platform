import { act, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { VersionSummary } from '@/entities/version';
import {
  getBatchReplaceVersionReferencesMockHandler,
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
  it('doCheckVersionInUse reports isInUse=false and empty items when nothing references the version', async () => {
    server.use(getCheckVersionInUseMockHandler({ items: [] }));
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let checked;
    await act(async () => {
      checked = await result.current.doCheckVersionInUse();
    });

    expect(checked).toEqual({ items: [], isInUse: false });
    await waitFor(() => expect(result.current.isCheckingInUse).toBe(false));
  });

  it('doCheckVersionInUse reports isInUse=true with the referencing items', async () => {
    server.use(getCheckVersionInUseMockHandler({ items: [{ type: 'tool', id: 't1' }] }));
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let checked;
    await act(async () => {
      checked = await result.current.doCheckVersionInUse();
    });

    expect(checked).toEqual({ items: [{ type: 'tool', id: 't1' }], isInUse: true });
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

  it('doDeleteVersion with a replacement calls batchReplaceVersionReferences(delete_old=true) instead', async () => {
    server.use(getBatchReplaceVersionReferencesMockHandler({ ok: true }));
    const { result } = renderHookWithProviders(() => useDeleteVersion(baseInput()));

    let deleted;
    await act(async () => {
      deleted = await result.current.doDeleteVersion(11);
    });

    expect(deleted).toEqual({ ok: true, errorMessage: undefined });
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
