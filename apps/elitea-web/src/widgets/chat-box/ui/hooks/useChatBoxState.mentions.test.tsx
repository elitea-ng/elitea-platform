import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { toParticipants } from '../ChatBox.helpers';

import { useChatBoxState } from './useChatBoxState';

// Main reuses numeric identities when a user is attached with a string ID.
const MAIN_PARTICIPANTS = [
  { id: 11, entity_name: 'user', entity_meta: { id: 2 }, meta: { user_name: 'E2E User' } },
  { id: 12, entity_name: 'dummy', entity_meta: {}, entity_settings: {} },
  { id: 13, entity_name: 'user', entity_meta: { id: 3 }, meta: { user_name: 'E2E Admin' } },
] as const;

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>;
}

function useMainMentions(participants: readonly unknown[]) {
  return useChatBoxState({
    activeParticipant: undefined, participants: toParticipants(participants), userId: '2',
    conversationStarters: [], isAgentsPage: false, chatInput: { current: null },
    projectId: '1', activeParticipantVersions: undefined,
  });
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ mcp_enabled: true })));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('user mentions from Main numeric participant identities', () => {
  it('excludes self, retains the other user identity, and adds Everyone once after refresh', () => {
    const { result, rerender } = renderHook(({ participants }) => useMainMentions(participants), {
      wrapper, initialProps: { participants: MAIN_PARTICIPANTS },
    });

    expect(result.current.hasOtherUsers).toBe(true);
    expect(result.current.users.map(({ id, name, userId }) => ({ id, name, userId }))).toEqual([
      { id: '13', name: 'E2E Admin', userId: '3' },
      { id: '@everyone', name: 'Everyone', userId: undefined },
    ]);

    rerender({ participants: structuredClone(MAIN_PARTICIPANTS) });
    expect(result.current.users.map((user) => user.id)).toEqual(['13', '@everyone']);
  });

  it('keeps the mention dropdown disabled for a numeric self identity alone', () => {
    const { result } = renderHook(() => useMainMentions(MAIN_PARTICIPANTS.slice(0, 2)), { wrapper });

    expect(result.current.hasOtherUsers).toBe(false);
    expect(result.current.users).toEqual([{ id: '@everyone', name: 'Everyone', participant: 'All users' }]);
  });

  it('does not create user candidates from object or unsafe numeric identities', () => {
    const participants = [
      MAIN_PARTICIPANTS[0],
      { id: 14, entity_name: 'user', entity_meta: { id: { toString: () => '3' } }, meta: { user_name: 'Object user' } },
      { id: 15, entity_name: 'user', entity_meta: { id: Number.MAX_SAFE_INTEGER + 1 }, meta: { user_name: 'Unsafe user' } },
    ];
    const { result } = renderHook(() => useMainMentions(participants), { wrapper });

    expect(result.current.hasOtherUsers).toBe(false);
    expect(result.current.users.map((user) => user.id)).toEqual(['@everyone']);
  });
});
