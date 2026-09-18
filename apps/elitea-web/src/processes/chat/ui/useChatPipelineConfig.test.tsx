/**
 * Coverage for the edit-mode pipeline configuration hook (#940 A12,
 * ELITEA-0928). No dedicated render is needed to drive this file's own
 * branches: `renderConfigurationPanels()` builds a plain React ELEMENT tree
 * (a `CreateAgentForm` wrapping an `AgentTagEditor`), and `onFieldChange`/
 * `onTagsChange` — this hook's two internal callbacks, neither of which is
 * part of `UseChatPipelineConfigResult` — are reachable as that tree's own
 * `information.props.onFieldChange` / `information.props.tagsSlot.props.
 * onChange`. Calling them directly exercises every branch this module owns
 * without mounting `CreateAgentForm` (a heavy real form component with its
 * own extensively-tested internals elsewhere).
 */
import type { ReactElement, ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  getEditApplicationMockHandler,
  getGetApplicationMockHandler,
  getGetApplicationVersionDetailMockHandler,
  getUpdateApplicationVersionMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { useChatPipelineConfig } from './useChatPipelineConfig';

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/**
 * Reads the `onFieldChange`/`tagsSlot` props off the built element tree
 * without mounting it. `renderConfigurationPanels`'s DECLARED return type
 * (`UseChatPipelineConfigResult`) only promises `tools` — `information` is
 * real at runtime (see this module's own doc comment) but not in that
 * narrower type, hence the cast.
 */
function panelProps(panels: { readonly tools: ReactNode; readonly information?: ReactNode }) {
  const element = panels.information as ReactElement<{
    readonly onFieldChange: (path: string, value: unknown) => void;
    readonly tagsSlot: ReactElement<{ readonly value: readonly unknown[]; readonly onChange: (next: readonly { readonly name: string }[]) => void }>;
    readonly values: { readonly name: string; readonly description: string; readonly version_details: Record<string, unknown> };
  }>;
  return {
    onFieldChange: element.props.onFieldChange,
    onTagsChange: element.props.tagsSlot.props.onChange,
    tagValue: element.props.tagsSlot.props.value,
    values: element.props.values,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('useChatPipelineConfig — inactive (create mode / unresolvable ids)', () => {
  it('is inactive and offers no save when disabled', () => {
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '1' } }, enabled: false }),
      { wrapper },
    );
    expect(result.current.onSaveVersion).toBeUndefined();
    expect(result.current.isSavingVersion).toBe(false);
  });

  it('is inactive when the participant is null', () => {
    const { result } = renderHook(() => useChatPipelineConfig({ projectId: '7', participant: null, enabled: true }), { wrapper });
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('is inactive when the participant id is not a positive finite number', () => {
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: 'not-a-number' }, entity_settings: { version_id: '1' } }, enabled: true }),
      { wrapper },
    );
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('is inactive when the version id is zero (not > 0)', () => {
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: 0 } }, enabled: true }),
      { wrapper },
    );
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('is inactive when projectId is undefined', () => {
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: undefined, participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '1' } }, enabled: true }),
      { wrapper },
    );
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('renders an empty seed draft while nothing has loaded', () => {
    server.use(getGetApplicationVersionDetailMockHandler(() => new Promise(() => {})), getGetApplicationMockHandler(() => new Promise(() => {})));
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: null, enabled: true }),
      { wrapper },
    );
    const { values, tagValue } = panelProps(result.current.renderConfigurationPanels());
    expect(values).toEqual({ name: '', description: '', version_details: {} });
    expect(tagValue).toEqual([]);
  });
});

const FULL_VERSION: ApplicationVersionDetail = {
  id: '10',
  application_id: '1',
  name: 'latest',
  status: 'draft',
  instructions: 'Do the thing',
  welcome_message: 'Hi!',
  conversation_starters: ['Start here', 42],
  // `tagNames()` accepts EITHER a bare string or a `{name}` object off the
  // wire (see that function's own comment) — `ApplicationVersionDetail`
  // only types the object shape, so the mixed array is cast as a whole.
  tags: ['alpha', { name: 'beta' }, { notAName: true }] as unknown as ApplicationVersionDetail['tags'],
  variables: [{ name: 'v1', value: 'x' }, { notName: 1 } as unknown as { name: string; value: string }],
  meta: { step_limit: 12 },
};

describe('useChatPipelineConfig — seeding from a full stored version', () => {
  it('seeds name/description/instructions/welcome/starters/tags/variables/step_limit', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler(FULL_VERSION),
      getGetApplicationMockHandler({ id: '1', name: 'App name', description: 'App description', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );

    await waitFor(() => {
      const { values } = panelProps(result.current.renderConfigurationPanels());
      expect(values.name).toBe('App name');
    });

    const { values, tagValue } = panelProps(result.current.renderConfigurationPanels());
    expect(values).toEqual({
      name: 'App name',
      description: 'App description',
      version_details: {
        id: 10,
        instructions: 'Do the thing',
        welcome_message: 'Hi!',
        conversation_starters: ['Start here'],
        tags: ['alpha', 'beta'],
        variables: [{ name: 'v1', value: 'x' }, { name: '', value: '' }],
        meta: { step_limit: 12 },
      },
    });
    expect(tagValue).toEqual([
      { id: -1, name: 'alpha', data: undefined },
      { id: -1, name: 'beta', data: undefined },
    ]);
  });

  it('falls back to empty name/description while the application read has not resolved, and defaults absent fields', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler(() => new Promise(() => {})),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );

    await waitFor(() => {
      const { values } = panelProps(result.current.renderConfigurationPanels());
      expect(values.version_details['id']).toBe(10);
    });
    const { values, tagValue } = panelProps(result.current.renderConfigurationPanels());
    expect(values.name).toBe('');
    expect(values.description).toBe('');
    expect(values.version_details).toEqual({
      id: 10,
      instructions: '',
      welcome_message: '',
      conversation_starters: [],
      tags: [],
      variables: [],
      meta: { step_limit: undefined },
    });
    expect(tagValue).toEqual([]);
  });

  it('treats a non-array tags/variables/conversation_starters/meta as empty', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({
        id: '10',
        application_id: '1',
        name: 'latest',
        status: 'draft',
        tags: 'not-an-array' as unknown as ApplicationVersionDetail['tags'],
        variables: 'not-an-array' as unknown as ApplicationVersionDetail['variables'],
        conversation_starters: 'not-an-array' as unknown as ApplicationVersionDetail['conversation_starters'],
        meta: 'not-an-object' as unknown as ApplicationVersionDetail['meta'],
      }),
      getGetApplicationMockHandler({ id: '1', name: '', description: '', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );

    await waitFor(() => {
      const { values } = panelProps(result.current.renderConfigurationPanels());
      expect(values.version_details['id']).toBe(10);
    });
    const { values } = panelProps(result.current.renderConfigurationPanels());
    expect(values.version_details).toEqual({
      id: 10,
      instructions: '',
      welcome_message: '',
      conversation_starters: [],
      tags: [],
      variables: [],
      meta: { step_limit: undefined },
    });
  });
});

describe('useChatPipelineConfig — onFieldChange', () => {
  async function seeded() {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));
    return result;
  }

  it('updates name', async () => {
    const result = await seeded();
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('name', 'New name'));
    expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('New name');
  });

  it('drops a non-string value for name (stores empty string)', async () => {
    const result = await seeded();
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('name', 123));
    expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('');
  });

  it('updates description', async () => {
    const result = await seeded();
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('description', 'New desc'));
    expect(panelProps(result.current.renderConfigurationPanels()).values.description).toBe('New desc');
  });

  it('drops a non-string value for description', async () => {
    const result = await seeded();
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('description', null));
    expect(panelProps(result.current.renderConfigurationPanels()).values.description).toBe('');
  });

  it('updates a version_details.* key', async () => {
    const result = await seeded();
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('version_details.welcome_message', 'Hello'));
    expect(panelProps(result.current.renderConfigurationPanels()).values.version_details['welcome_message']).toBe('Hello');
  });

  it('drops a nested version_details.*.* path', async () => {
    const result = await seeded();
    const before = panelProps(result.current.renderConfigurationPanels()).values.version_details;
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('version_details.meta.step_limit', 99));
    expect(panelProps(result.current.renderConfigurationPanels()).values.version_details).toEqual(before);
  });

  it('drops an unrecognised top-level path entirely', async () => {
    const result = await seeded();
    const before = panelProps(result.current.renderConfigurationPanels()).values;
    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('unknown_field', 'x'));
    expect(panelProps(result.current.renderConfigurationPanels()).values).toEqual(before);
  });
});

describe('useChatPipelineConfig — onTagsChange', () => {
  it('rebuilds version_details.tags from the tag editor value, dropping blank names', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));

    act(() =>
      panelProps(result.current.renderConfigurationPanels()).onTagsChange([{ name: 'one' }, { name: '  ' }, { name: 'two' }]),
    );

    const { tagValue, values } = panelProps(result.current.renderConfigurationPanels());
    expect(values.version_details['tags']).toEqual(['one', 'two']);
    expect(tagValue).toEqual([
      { id: -1, name: 'one', data: undefined },
      { id: -1, name: 'two', data: undefined },
    ]);
  });
});

describe('useChatPipelineConfig — isConfigurationDirty', () => {
  /*
   * `PipelineEditor.tsx`'s own `isDirty`/`isYamlDirty` never observe an edit
   * made through THIS hook's Configuration-tab fields (neither is threaded
   * into `PipelineEditorBody`'s `activeTab === 0` branch) — this hook's own
   * `isConfigurationDirty` is the only signal that reaches
   * `PipelineEditorProps.isConfigurationDirty` and, through it, the Save
   * button's `!isDirty` gate (`elitea_issues #2223/#2664`). Without it, a
   * Configuration-only edit in the chat-embedded editor left Save
   * permanently disabled.
   */
  it('starts false once the draft has been seeded from the server', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));
    expect(result.current.isConfigurationDirty).toBe(false);
  });

  it('turns true after onFieldChange', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));

    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('version_details.welcome_message', 'Hello'));

    expect(result.current.isConfigurationDirty).toBe(true);
  });

  it('turns true after onTagsChange', async () => {
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));

    act(() => panelProps(result.current.renderConfigurationPanels()).onTagsChange([{ name: 'one' }]));

    expect(result.current.isConfigurationDirty).toBe(true);
  });

  it('resets to false once a real save resolves (the refetch reseeds the draft)', async () => {
    // `getGetApplicationVersionDetailMockHandler` is also what the post-save
    // `refetch()` re-reads: it must echo the SAVED value, not the ORIGINAL
    // one, or TanStack Query's structural sharing treats the refetch as
    // "nothing changed" (deep-equal to the last-read data) and reuses the
    // OLD `data` reference — the seed effect never re-fires and this test
    // would pass for the wrong reason (an accidental reference change) or
    // fail for a testing artefact, not the real product behaviour.
    let saved = false;
    server.use(
      getGetApplicationVersionDetailMockHandler(() => ({
        id: '10',
        application_id: '1',
        name: 'latest',
        status: 'draft',
        welcome_message: saved ? 'Hello' : '',
      })),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
      getUpdateApplicationVersionMockHandler(() => {
        saved = true;
        return { id: '10', application_id: '1', name: 'latest', status: 'draft' };
      }),
      getEditApplicationMockHandler(() => ({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z' })),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));

    act(() => panelProps(result.current.renderConfigurationPanels()).onFieldChange('version_details.welcome_message', 'Hello'));
    expect(result.current.isConfigurationDirty).toBe(true);

    const onSuccess = vi.fn<(saved: unknown) => void>();
    await act(async () => {
      result.current.onSaveVersion?.(onSuccess);
      await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    });

    await waitFor(() => expect(result.current.isConfigurationDirty).toBe(false));
  });
});

describe('useChatPipelineConfig — onSaveVersion', () => {
  it('saves the draft, refetches, and reports success only after a real save', async () => {
    let updateCalls = 0;
    let editCalls = 0;
    server.use(
      getGetApplicationVersionDetailMockHandler(() => {
        return { id: '10', application_id: '1', name: 'latest', status: 'draft' };
      }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
      getUpdateApplicationVersionMockHandler(() => {
        updateCalls += 1;
        return { id: '10', application_id: '1', name: 'latest', status: 'draft' };
      }),
      getEditApplicationMockHandler(() => {
        editCalls += 1;
        return { id: '1', name: 'Saved name', description: 'Saved desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z' };
      }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));

    const onSuccess = vi.fn<(saved: unknown) => void>();
    await act(async () => {
      result.current.onSaveVersion?.(onSuccess);
      await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    });

    expect(updateCalls).toBe(1);
    expect(editCalls).toBe(1);
    const saved = onSuccess.mock.calls[0]?.[0] as { readonly versionDetail?: unknown } | undefined;
    expect(saved?.versionDetail).toBeDefined();
  });

  it('sends toVersionBody defaults (empty strings/arrays, step_limit 25) for a bare draft', async () => {
    const seenBodies: unknown[] = [];
    server.use(
      getGetApplicationVersionDetailMockHandler({ id: '10', application_id: '1', name: 'latest', status: 'draft' }),
      getGetApplicationMockHandler({ id: '1', name: '', description: '', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', async ({ request }) => {
        seenBodies.push(await request.json());
        return HttpResponse.json({ id: '10', application_id: '1', name: 'latest', status: 'draft' }, { status: 201 });
      }),
      getEditApplicationMockHandler({ id: '1', name: '', description: '', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z' }),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.version_details['id']).toBe(10));

    await act(async () => {
      result.current.onSaveVersion?.(() => {});
      await waitFor(() => expect(seenBodies.length).toBe(1));
    });

    expect(seenBodies[0]).toMatchObject({
      name: 'latest',
      instructions: '',
      welcome_message: '',
      conversation_starters: [],
      variables: [],
      tags: [],
      meta: { step_limit: 25 },
    });
  });

  it('does nothing when the ids cannot be resolved (guarded no-op)', () => {
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: 'nope' } }, enabled: true }),
      { wrapper },
    );
    expect(result.current.onSaveVersion).toBeUndefined();
  });

  it('does not report success (and does not refetch) when the save fails', async () => {
    let versionGetCalls = 0;
    server.use(
      getGetApplicationVersionDetailMockHandler(() => {
        versionGetCalls += 1;
        return { id: '10', application_id: '1', name: 'latest', status: 'draft' };
      }),
      getGetApplicationMockHandler({ id: '1', name: 'App', description: 'Desc', icon: '', owner_id: 'u1', created_at: '2026-01-01T00:00:00Z', versions: [] }),
      http.put('*/elitea_core/version/prompt_lib/:projectId/:applicationId/:versionId', () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
    );
    const { result } = renderHook(
      () => useChatPipelineConfig({ projectId: '7', participant: { entity_meta: { id: '1' }, entity_settings: { version_id: '10' } }, enabled: true }),
      { wrapper },
    );
    await waitFor(() => expect(panelProps(result.current.renderConfigurationPanels()).values.name).toBe('App'));
    const callsBeforeSave = versionGetCalls;

    const onSuccess = vi.fn();
    await act(async () => {
      result.current.onSaveVersion?.(onSuccess);
      // Give the failed save's promise chain a turn to settle.
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(onSuccess).not.toHaveBeenCalled();
    // No refetch means no extra GET beyond the initial seed read.
    expect(versionGetCalls).toBe(callsBeforeSave);
  });
});
