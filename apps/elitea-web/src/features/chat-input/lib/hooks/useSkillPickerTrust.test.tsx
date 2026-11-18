import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { ConversationSkillPin } from '@/entities/conversation-skill';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { TRUST_LOOKUP_LIMIT, useUntrustedSkillIds } from './useSkillPickerTrust';

const BASE = '/api/v2';

function pin(skillId: number, state: 'trusted' | 'untrusted'): ConversationSkillPin {
  return {
    id: skillId,
    kind: 'project',
    skill_id: skillId,
    name: `skill ${String(skillId)}`,
    description: '',
    trust: { state },
    available: true,
    pinned_by: 1,
    pinned_at: '2026-11-18T00:00:00Z',
  };
}

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

describe('useUntrustedSkillIds', () => {
  const asked: string[] = [];
  beforeEach(() => {
    asked.length = 0;
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/elitea_core/skill_trust/prompt_lib/:projectId/:skillId`, ({ params }) => {
        asked.push(String(params['skillId']));
        return HttpResponse.json({ foreign: true, revision: 'a', state: params['skillId'] === '2' ? 'untrusted' : 'not_required', history: [] });
      }),
    );
  });
  afterEach(() => resetGeneratedClient());

  it('takes a pinned skill\'s state from its pin and asks the server only about the others', async () => {
    const { result } = renderHook(() => useUntrustedSkillIds('5', [pin(1, 'untrusted'), pin(3, 'trusted')], ['1', '2', '3', '4'], true), { wrapper });
    await waitFor(() => expect([...result.current].sort()).toEqual(['1', '2']));
    expect(asked.sort()).toEqual(['2', '4']);
  });

  it('asks nothing while the picker is closed', () => {
    const { result } = renderHook(() => useUntrustedSkillIds('5', [], ['2'], false), { wrapper });
    expect(result.current.size).toBe(0);
    expect(asked).toEqual([]);
  });

  it('asks about one screenful, not the whole project', async () => {
    const ids = Array.from({ length: TRUST_LOOKUP_LIMIT + 8 }, (_, index) => String(index + 10));
    renderHook(() => useUntrustedSkillIds('5', [], ids, true), { wrapper });
    await waitFor(() => expect(asked).toHaveLength(TRUST_LOOKUP_LIMIT));
  });

  it('does not mark a skill whose state could not be read', async () => {
    server.use(http.get(`${BASE}/elitea_core/skill_trust/prompt_lib/:projectId/:skillId`, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    const { result } = renderHook(() => useUntrustedSkillIds('5', [], ['2'], true), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(result.current.size).toBe(0);
  });
});
