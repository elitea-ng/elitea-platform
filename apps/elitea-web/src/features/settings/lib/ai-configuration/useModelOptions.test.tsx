/**
 * useModelOptions.test.tsx
 *
 * DEFECT this file pins (#80). The AI-providers page built its default-model
 * options from the CONFIGURATION rows and labelled each with `elitea_title`,
 * while the value each select carries is
 * `${default_model_name}<<>>${default_model_project_id}` — a MODEL name.
 * Measured read-only on production, project 1: none of the six option values
 * matched the select's own value, and three of the nine catalogue models had
 * no configuration row at all, so they could not be picked.
 *
 * These tests pin the value shape, the label, and the tier filters against the
 * catalogue rows the server really sends.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../../test/setup';

import { useModelOptions } from './useModelOptions';

const BASE = '/api/v2';
const PROJECT_ID = '1';

/**
 * The shape `GET /configurations/models/{projectId}?section=llm` really
 * answers, copied from the production response: `name` is the model id the
 * default is recorded under, `display_name` is what a user should read, and
 * the tier flags live on the row.
 */
const LLM = {
  total: 3,
  items: [
    { id: '1_luna', name: 'global.openai.gpt-5.6-luna', display_name: 'GPT-5.6-Luna', project_id: 1, low_tier: true, high_tier: false },
    { id: '1_terra', name: 'global.openai.gpt-5.6-terra', display_name: 'GPT-5.6-Terra', project_id: 1, low_tier: false, high_tier: true },
    { id: '1_bare', name: 'gpt-4.1', project_id: 1, low_tier: false, high_tier: false },
  ],
  default_model_name: 'global.openai.gpt-5.6-luna',
  default_model_project_id: '1',
  low_tier_default_model_name: 'global.openai.gpt-5.6-luna',
  low_tier_default_model_project_id: '1',
  high_tier_default_model_name: 'global.openai.gpt-5.6-terra',
  high_tier_default_model_project_id: '1',
};

const EMBEDDING = {
  total: 1,
  items: [{ id: '1_emb', name: 'text-embedding-3-small', display_name: 'Embedding Small', project_id: 1 }],
  default_model_name: 'text-embedding-3-small',
  default_model_project_id: '1',
};

function mockSections(): void {
  server.use(
    http.get(`${BASE}/configurations/models/${PROJECT_ID}`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section');
      if (section === 'llm') return HttpResponse.json(LLM);
      if (section === 'embedding') return HttpResponse.json(EMBEDDING);
      return HttpResponse.json({ total: 0, items: [], default_model_name: '', default_model_project_id: '' });
    }),
  );
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

afterEach(() => {
  resetGeneratedClient();
});

describe('useModelOptions', () => {
  it('builds each option value from the MODEL name, so the select can match its own default', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    await waitFor(() => expect(result.current.modelOptions).toHaveLength(3));
    expect(result.current.modelOptions.map((option) => option.value)).toEqual([
      'global.openai.gpt-5.6-luna<<>>1',
      'global.openai.gpt-5.6-terra<<>>1',
      'gpt-4.1<<>>1',
    ]);

    // The value the section's Default select carries, built the same way the
    // panel builds it. It MUST be one of the options above.
    const selectValue = `${LLM.default_model_name}<<>>${LLM.default_model_project_id}`;
    expect(result.current.modelOptions.map((option) => option.value)).toContain(selectValue);
  });

  it('labels each option with the display name, and falls back to the model name', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    await waitFor(() => expect(result.current.modelOptions).toHaveLength(3));
    expect(result.current.modelOptions.map((option) => option.label)).toEqual([
      'GPT-5.6-Luna',
      'GPT-5.6-Terra',
      // No `display_name` on this row — the name is what is left to show.
      'gpt-4.1',
    ]);
  });

  it('filters the tier lists by the catalogue tier flags', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    await waitFor(() => expect(result.current.modelOptions).toHaveLength(3));
    expect(result.current.lowTierModelOptions.map((option) => option.label)).toEqual(['GPT-5.6-Luna']);
    expect(result.current.highTierModelOptions.map((option) => option.label)).toEqual(['GPT-5.6-Terra']);
  });

  it('reads each section from its own query, not from the LLM one', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    await waitFor(() => expect(result.current.embeddingModelOptions).toHaveLength(1));
    expect(result.current.embeddingModelOptions[0]).toEqual({
      value: 'text-embedding-3-small<<>>1',
      label: 'Embedding Small',
    });
    // A section the server has nothing for stays empty rather than borrowing
    // the LLM list.
    expect(result.current.asrOptions).toEqual([]);
  });

  it('reports each section’s defaults, so the panel needs no second fetch', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    await waitFor(() => expect(result.current.sectionData.llm.default_model_name).toBe('global.openai.gpt-5.6-luna'));
    expect(result.current.sectionData.llm.high_tier_default_model_name).toBe('global.openai.gpt-5.6-terra');
    expect(result.current.sectionData.embedding.default_model_name).toBe('text-embedding-3-small');
  });

  it('answers an empty, fully shaped result before anything has settled', () => {
    configureGeneratedClient({ baseUrl: BASE });
    mockSections();

    const { result } = renderHook(() => useModelOptions({ projectId: PROJECT_ID, includeShared: true }), { wrapper });

    // The first render, before any response. A caller reads `sectionData`
    // straight into a select value, so `undefined` here would render the
    // literal `undefined<<>>undefined`.
    expect(result.current.modelOptions).toEqual([]);
    expect(result.current.sectionData.llm.default_model_name).toBe('');
    expect(result.current.sectionData.tts.default_model_project_id).toBe('');
  });
});
