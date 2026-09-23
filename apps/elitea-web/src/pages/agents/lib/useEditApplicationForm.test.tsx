import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getUpdateApplicationVersionMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { useEditApplicationForm } from './useEditApplicationForm';
import { useEditApplicationVersionFields } from './useEditApplicationVersionFields';

const DETAIL: ApplicationDetail = {
  id: '42',
  name: 'My Agent',
  description: 'A helpful agent',
  icon: '',
  owner_id: 'user-1',
  created_at: '2026-01-01T00:00:00Z',
  versions: [],
};

const VERSION: ApplicationVersionDetail = {
  id: '1',
  application_id: '42',
  name: 'base',
  status: 'draft',
  instructions: 'Be helpful.',
  conversation_starters: ['Hi there'],
  // A second `meta` key, so the step-limit test can prove the save MERGES
  // into the stored blob instead of replacing it.
  meta: { category: 'support' },
};

/**
 * Drives the two hooks the page composes, in the page's own order — the
 * version-level fields hook first, then the form hook that reads its state.
 * Calling `useEditApplicationForm` with a hand-built `versionFields` stub
 * would let a save payload regression pass: #307 was precisely a bridge that
 * looked correct in isolation and routed nothing in the real composition.
 */
function useEditApplicationFormHarness(detail: ApplicationDetail | undefined, version: ApplicationVersionDetail | undefined) {
  const versionFields = useEditApplicationVersionFields(version);
  const form = useEditApplicationForm(detail, version, '9', 42, versionFields);
  return { ...form, applyFieldChange: versionFields.applyFieldChange, versionFields };
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useEditApplicationForm', () => {
  it('seeds the form from detail/activeVersion', () => {
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });
    expect(result.current.form.getValues()).toEqual({
      name: 'My Agent',
      description: 'A helpful agent',
      version_details: { conversation_starters: ['Hi there'] },
    });
  });

  it('seeds empty defaults while detail has not loaded yet', () => {
    const { result } = renderHook(() => useEditApplicationFormHarness(undefined, undefined), { wrapper });
    expect(result.current.form.getValues()).toEqual({
      name: '',
      description: '',
      version_details: { conversation_starters: [] },
    });
  });

  it('handleSave is a no-op (does not call the save endpoint) when activeVersion is undefined', () => {
    const saveSpy = vi.fn(() => ({
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
    }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, undefined), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    expect(saveSpy).not.toHaveBeenCalled();
  });

  it('handleSave calls the save endpoint with the current form values once valid', async () => {
    const saveSpy = vi.fn(() => ({
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
    }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(saveSpy).toHaveBeenCalledTimes(1));
  });

  it('isSaving reflects the in-flight save state', () => {
    const saveSpy = vi.fn(() => ({
      id: '1',
      application_id: '42',
      name: 'base',
      status: 'draft',
    }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });

    expect(result.current.isSaving).toBe(false);
  });

  it('saveError is undefined before any save attempt', () => {
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });
    expect(result.current.saveError).toBeUndefined();
  });

  it('saveError surfaces once a save attempt fails — old app: useSaveVersion.js toasts on every failed save; this app has no toast infra, so the caller renders this instead', async () => {
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });

    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(result.current.saveError).toBeDefined());
    expect(result.current.isSaving).toBe(false);
  });

  it('saveError clears once a subsequent save attempt succeeds', async () => {
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const { result, rerender } = renderHook(
      ({ detail, version }: { detail: ApplicationDetail; version: ApplicationVersionDetail }) =>
        useEditApplicationFormHarness(detail, version),
      { wrapper, initialProps: { detail: DETAIL, version: VERSION } },
    );

    act(() => {
      result.current.handleSave();
    });
    await waitFor(() => expect(result.current.saveError).toBeDefined());

    server.use(
      getUpdateApplicationVersionMockHandler({
        id: '1',
        application_id: '42',
        name: 'base',
        status: 'draft',
      }),
    );
    rerender({ detail: DETAIL, version: VERSION });

    act(() => {
      result.current.handleSave();
    });
    await waitFor(() => expect(result.current.saveError).toBeUndefined());
  });

  /*
   * #307 — the discriminating tests. Every assertion above this point passes
   * against the broken hook: they check that the endpoint was CALLED, never
   * what it was called WITH. The page's whole defect was that the call went
   * out carrying `conversation_starters` and nothing else, so Save reported
   * success while discarding the user's edit.
   *
   * Each test below therefore reads the real request BODY off the wire.
   */
  async function captureSave(mutate: (harness: ReturnType<typeof useEditApplicationFormHarness>) => void) {
    const versionBodies: unknown[] = [];
    const applicationBodies: unknown[] = [];
    server.use(
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        versionBodies.push(await request.json());
        return HttpResponse.json({ id: '1', application_id: '42', name: 'base', status: 'draft' }, { status: 201 });
      }),
      http.put('*/elitea_core/application/prompt_lib/:projectId/:id', async ({ request }) => {
        applicationBodies.push(await request.json());
        return HttpResponse.json({ id: '42' }, { status: 201 });
      }),
    );
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });

    act(() => {
      mutate(result.current);
    });
    act(() => {
      result.current.handleSave();
    });

    await waitFor(() => expect(versionBodies).toHaveLength(1));
    return { versionBody: versionBodies[0] as Record<string, unknown>, applicationBodies, result };
  }

  it('sends an edited welcome message in the version PUT body — the whole point of issue 307, and the assertion the old suite lacked', async () => {
    const { versionBody } = await captureSave((harness) => {
      harness.applyFieldChange('version_details.welcome_message', 'Welcome aboard');
    });
    expect(versionBody['welcome_message']).toBe('Welcome aboard');
  });

  it('sends edited instructions in the version PUT body rather than the version the server returned', async () => {
    const { versionBody } = await captureSave((harness) => {
      harness.applyFieldChange('version_details.instructions', 'Be extremely helpful.');
    });
    expect(versionBody['instructions']).toBe('Be extremely helpful.');
    expect(versionBody['instructions']).not.toBe(VERSION.instructions);
  });

  it('merges an edited step limit into `meta` WITHOUT dropping the keys the stored version already carried', async () => {
    const { versionBody } = await captureSave((harness) => {
      harness.applyFieldChange('version_details.meta.step_limit', 40);
    });
    // The Go handler assigns the whole `meta` map it receives, so a payload
    // of `{step_limit}` alone would silently blank every other stored key.
    // `internal_tools` joined this payload with #307's Tools-panel mount —
    // the switches are saved through this same `meta` blob, and an empty
    // array is a real value (all switches off), not an omission.
    // `notes` rides in the same blob (#898 — the field has no column, by
    // design), and an empty string is a real value there: it is what an
    // emptied Editor Notes box sends, and the patch merge cannot delete a key.
    expect(versionBody['meta']).toEqual({ category: 'support', step_limit: 40, internal_tools: [], notes: '' });
  });

  it('sends the internal-tool switches through `meta.internal_tools` (issue 307 — the Tools panel had no save path at all)', async () => {
    const { versionBody } = await captureSave((harness) => {
      harness.applyFieldChange('version_details.meta.internal_tools', ['data_analysis', 'internal_mcp']);
    });
    expect(versionBody['meta']).toMatchObject({ internal_tools: ['data_analysis', 'internal_mcp'] });
  });

  it('sends the application-level name/description to the application PUT — a different endpoint, never called at all before issue 307', async () => {
    const { applicationBodies, result } = await captureSave((harness) => {
      harness.form.setValue('name', 'Renamed Agent', { shouldValidate: true, shouldDirty: true });
    });
    await waitFor(() => expect(applicationBodies).toHaveLength(1));
    expect(applicationBodies[0]).toEqual({ name: 'Renamed Agent', description: DETAIL.description });
    expect(result.current.saveError).toBeUndefined();
  });

  it('leaves an unedited field at the value the server holds, so a save cannot blank a field the user never touched', async () => {
    const { versionBody } = await captureSave(() => {
      /* no edit at all */
    });
    expect(versionBody['instructions']).toBe(VERSION.instructions);
    expect(versionBody['conversation_starters']).toEqual(VERSION.conversation_starters);
  });

  /*
   * elitea_issues #6054 — `applicationCreationSchema`'s
   * `conversationStarterEntrySchema` already refines each entry as "absent,
   * or a non-blank string" (`entities/application-form/model/validation.ts`),
   * so the form's own `isValid` goes false the moment a starter is blank —
   * `handleSubmit`'s `onValid` never runs and no PUT is sent at all. This is
   * the existing protection against a blank starter being persisted; it just
   * has no VISIBLE row-level error affordance (`ConversationStartersEditor`
   * passes no `error`/`helperText` to the row, unlike the AI-draft review
   * form's equivalent field — see gaps.md).
   */
  it('#6054 — a blank conversation starter makes the form invalid, so Save cannot fire the request at all', async () => {
    const saveSpy = vi.fn(() => ({ id: '1', application_id: '42', name: 'base', status: 'draft' }));
    server.use(getUpdateApplicationVersionMockHandler(saveSpy));
    const { result } = renderHook(() => useEditApplicationFormHarness(DETAIL, VERSION), { wrapper });

    act(() => {
      result.current.form.setValue('version_details.conversation_starters', ['Real one', '   '], {
        shouldValidate: true,
        shouldDirty: true,
      });
    });
    await waitFor(() => expect(result.current.form.formState.isValid).toBe(false));

    act(() => {
      result.current.handleSave();
    });

    // No PUT should ever land — give any (wrongly) in-flight request a tick.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(saveSpy).not.toHaveBeenCalled();
  });
});
