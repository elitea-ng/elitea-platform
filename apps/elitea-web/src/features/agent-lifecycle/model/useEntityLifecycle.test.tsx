/**
 * The lifecycle plane's failure vocabulary, and the export read.
 *
 * A refusal that reads "the action could not be completed" is the one that
 * costs an author the most: the three refusals this plane actually produces
 * mean three different things, and only one of them is something the author
 * can fix by editing their agent.
 */
import { waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { lifecycleErrorMessage, useEntityLifecycle } from './useEntityLifecycle';
import { renderHookWithProviders } from '../__tests__/testUtils';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('lifecycleErrorMessage', () => {
  it('prefers the server message over any of its own copy', () => {
    const error = new EliteaApiError({
      kind: 'http',
      status: 403,
      url: '/x',
      body: { error: 'publishing is blocked on this deployment' },
    });
    expect(lifecycleErrorMessage(error)).toBe('publishing is blocked on this deployment');
  });

  it('names the permission when a 403 carries no message', () => {
    const error = new EliteaApiError({ kind: 'http', status: 403, url: '/x', body: null });
    expect(lifecycleErrorMessage(error)).toMatch(/not permitted/);
  });

  it('names the conflict a 409 actually is', () => {
    const error = new EliteaApiError({ kind: 'http', status: 409, url: '/x', body: null });
    expect(lifecycleErrorMessage(error)).toMatch(/already published/);
  });

  it('names validation for a 422', () => {
    const error = new EliteaApiError({ kind: 'http', status: 422, url: '/x', body: null });
    expect(lifecycleErrorMessage(error)).toMatch(/validation/);
  });

  // A session that has expired is not a publishing problem, and telling the
  // author to fix their agent would send them the wrong way.
  it('tells the reader to sign in again on an auth failure', () => {
    const error = new EliteaApiError({ kind: 'auth', status: 401, url: '/x' });
    expect(lifecycleErrorMessage(error)).toMatch(/sign in/i);
  });

  it('names the network when the server was never reached', () => {
    const error = new EliteaApiError({ kind: 'network', url: '/x', message: 'boom', cause: null });
    expect(lifecycleErrorMessage(error)).toMatch(/could not be reached/);
  });

  it('names a cancellation as one', () => {
    const error = new EliteaApiError({ kind: 'aborted', url: '/x' });
    expect(lifecycleErrorMessage(error)).toMatch(/cancelled/);
  });

  it('falls back for anything that is not an API error at all', () => {
    expect(lifecycleErrorMessage(new Error('boom'))).toMatch(/could not be completed/);
    expect(lifecycleErrorMessage('boom')).toMatch(/could not be completed/);
  });

  // An empty `error` string is not a message; using it would render a blank
  // alert that looks like a rendering bug.
  it('ignores an empty server message', () => {
    const error = new EliteaApiError({ kind: 'http', status: 500, url: '/x', body: { error: '' } });
    expect(lifecycleErrorMessage(error)).toMatch(/could not be completed/);
  });
});

describe('useEntityLifecycle.exportDocument', () => {
  // The plain export — no `?fork=true` — is the document Import round-trips.
  // A fork document holds only the latest version, so exporting one as a
  // backup would quietly lose every other version.
  it('reads the plain export, without the fork flag', async () => {
    const seen: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, ({ request }) => {
        seen.push(request.url);
        return HttpResponse.json({ ok: true, applications: [{ name: 'Audit Agent' }] });
      }),
    );
    const { result } = renderHookWithProviders(() => useEntityLifecycle('2', '7'));
    const document = await result.current.exportDocument(7);

    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0]).toContain('/export_import/prompt_lib/2/7');
    expect(seen[0]).not.toContain('fork=true');
    expect(document.applications).toHaveLength(1);
  });
});
