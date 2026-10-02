import { act, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getSaveApplicationNewVersionMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';

import type { SaveNewVersionInput } from './useSaveNewVersion';
import { useSaveNewVersion } from './useSaveNewVersion';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

const newVersionWire = {
  id: '9',
  application_id: '3',
  name: 'v2',
  status: 'draft',
  agent_type: 'openai',
  instructions: 'Be helpful',
};

function baseInput(overrides: Partial<SaveNewVersionInput> = {}): SaveNewVersionInput {
  return {
    projectId: 'p1',
    applicationId: 3,
    name: 'v2',
    version: { instructions: 'Be helpful' },
    ...overrides,
  };
}

describe('useSaveNewVersion', () => {
  it.each([undefined, 12])('sends the optional skill source version %s', async (sourceVersionId) => {
    let body: unknown;
    server.use(
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(newVersionWire, { status: 201 });
      }),
    );
    const { result } = renderHookWithProviders(() => useSaveNewVersion());
    await act(async () => {
      await result.current.onCreateNewVersion(baseInput(sourceVersionId === undefined ? {} : { sourceVersionId }));
    });
    expect(body).toEqual({
      instructions: 'Be helpful',
      name: 'v2',
      ...(sourceVersionId === undefined ? {} : { copy_skills_from_version_id: sourceVersionId }),
    });
  });

  it('POSTs the new version and returns the generated (snake_case) response verbatim', async () => {
    server.use(getSaveApplicationNewVersionMockHandler(newVersionWire));
    let onSuccessArg: unknown;
    const { result } = renderHookWithProviders(() =>
      useSaveNewVersion({ onSuccess: (data) => (onSuccessArg = data) }),
    );

    let created;
    await act(async () => {
      created = await result.current.onCreateNewVersion(baseInput());
    });

    expect(created).toEqual(expect.objectContaining({ id: '9', name: 'v2', application_id: '3' }));
    expect(onSuccessArg).toEqual(created);
    await waitFor(() => expect(result.current.isSavingNewVersion).toBe(false));
  });

  it('POSTs the new version unconditionally even when onSaveTools resolves false — only onSuccess is gated (baseline useSaveNewVersion.js:112-149 parity)', async () => {
    server.use(getSaveApplicationNewVersionMockHandler(newVersionWire));
    const onSaveTools = vi.fn().mockResolvedValue(false);
    let onSuccessCalled = false;
    const { result } = renderHookWithProviders(() =>
      useSaveNewVersion({
        onSaveTools,
        onSuccess: () => {
          onSuccessCalled = true;
        },
      }),
    );

    let created;
    await act(async () => {
      created = await result.current.onCreateNewVersion(baseInput());
    });

    expect(created).toEqual(expect.objectContaining({ id: '9', name: 'v2', application_id: '3' }));
    expect(onSaveTools).toHaveBeenCalledTimes(1);
    expect(onSuccessCalled).toBe(false);
    expect(result.current.error).toBeUndefined();
  });

  it('calls onSuccess when onSaveTools resolves true', async () => {
    server.use(getSaveApplicationNewVersionMockHandler(newVersionWire));
    const onSaveTools = vi.fn().mockResolvedValue(true);
    let onSuccessCalled = false;
    const { result } = renderHookWithProviders(() =>
      useSaveNewVersion({
        onSaveTools,
        onSuccess: () => {
          onSuccessCalled = true;
        },
      }),
    );

    await act(async () => {
      await result.current.onCreateNewVersion(baseInput());
    });

    expect(onSuccessCalled).toBe(true);
  });

  it('sets error/errorMessage and returns undefined on a failed POST', async () => {
    server.use(
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'version name is required' }, { status: 400 }),
      ),
    );
    const { result } = renderHookWithProviders(() => useSaveNewVersion());

    let created;
    await act(async () => {
      created = await result.current.onCreateNewVersion(baseInput());
    });

    expect(created).toBeUndefined();
    await waitFor(() => expect(result.current.error).toBeDefined());
    expect(result.current.errorMessage?.length).toBeGreaterThan(0);
  });
});
