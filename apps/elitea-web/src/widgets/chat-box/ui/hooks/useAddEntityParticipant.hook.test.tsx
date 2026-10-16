/**
 * The hook's own wiring, over MSW.
 *
 * Separate from `useAddEntityParticipant.test.ts` because it needs a
 * `QueryClientProvider` wrapper (so a `.tsx` file), and because that sibling
 * deliberately runs without a React tree at all. R-M1 forbids `vi.mock`, so
 * the version lookup and the participant POST are served as real requests —
 * which is the point here: the two round trips, and their order, ARE the
 * defect this file covers.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '@/test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { useAddEntityParticipant } from './useAddEntityParticipant';

const BASE = '/api/v2';

/** The row shape the "+" menu is populated from: a LIST row, carrying no version. */
const AGENT_LIST_ROW = {
  // Ids are strings on this wire: "Numeric id serialized as string"
  // (`shared/api/generated/model/applicationVersionDetail.zod.ts:54`).
  id: '12',
  project_id: '2',
  name: 'RustProbe',
  agent_type: 'openai',
  participantType: 'application',
  meta: {},
};

const NO_PARTICIPANTS: readonly [] = [];

let addedBodies: unknown[] = [];

function createWrapper(): ({ children }: { children: ReactNode }) => ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  addedBodies = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    // The DETAIL endpoint — the only place a version comes from.
    http.get(`${BASE}/elitea_core/application/prompt_lib/2/12`, () => HttpResponse.json({
      id: '12',
      project_id: '2',
      name: 'RustProbe',
      agent_type: 'openai',
      version_details: { id: '34', name: 'latest' },
      versions: [{ id: '33', name: 'v1' }, { id: '34', name: 'latest' }],
    })),
    http.post(`${BASE}/elitea_core/participants/prompt_lib/2/77`, async ({ request }) => {
      addedBodies.push(await request.json());
      return HttpResponse.json([
        { id: 101, entity_name: 'application', entity_meta: { id: '12', project_id: '2', name: 'RustProbe' }, entity_settings: { version_id: '34' } },
      ]);
    }),
    http.get(`${BASE}/elitea_core/conversation/prompt_lib/2/77`, () => HttpResponse.json({ id: 77, name: 'New Chat', participants: [] })),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useAddEntityParticipant on a chat with no conversation', () => {
  it('creates the conversation, resolves the version, and attaches the agent — the click used to do nothing at all', async () => {
    const createConversation = vi.fn(() => Promise.resolve({ id: 77, uuid: 'u-77' }));
    const onConversationCreated = vi.fn();
    const onChangeParticipant = vi.fn();
    const { result } = renderHook(() => useAddEntityParticipant({
      projectId: 2,
      conversationId: undefined,
      participants: NO_PARTICIPANTS,
      onChangeParticipant,
      createConversation,
      onConversationCreated,
    }), { wrapper: createWrapper() });

    act(() => { result.current.onSelectParticipant(AGENT_LIST_ROW); });

    await waitFor(() => { expect(addedBodies).toHaveLength(1); });
    expect(createConversation).toHaveBeenCalledTimes(1);
    // The whole point of the version lookup: without `version_id` the
    // resolver's join finds no row and every turn answers 422.
    const [posted] = addedBodies as { entity_name: string; entity_meta: Record<string, unknown>; entity_settings: Record<string, unknown> }[][];
    expect(posted?.[0]?.entity_name).toBe('application');
    expect(posted?.[0]?.entity_meta['id']).toBe('12');
    expect(posted?.[0]?.entity_settings['version_id']).toBe('34');
    await waitFor(() => { expect(onConversationCreated).toHaveBeenCalledWith({ id: 77, uuid: 'u-77', activeParticipantId: '101' }); });
    expect(onChangeParticipant.mock.calls[0]?.[0]).toMatchObject({ id: '101', entityName: 'application' });
  });

  it('lets the row be picked again after a selection that settled without adding anything', async () => {
    // `applyParticipantSelection` RESOLVES — it does not reject — on several
    // no-op paths; this one is `conversationForSelection` returning undefined
    // because the create call produced no id. Pending was cleared only by
    // `.catch` or by the `participants` prop changing, and neither happens
    // here, so the "+"-menu row stayed disabled for the rest of the session
    // and the user could never retry.
    const createConversation = vi.fn(() => Promise.resolve(undefined));
    const { result } = renderHook(() => useAddEntityParticipant({
      projectId: 2,
      conversationId: undefined,
      participants: NO_PARTICIPANTS,
      createConversation,
    }), { wrapper: createWrapper() });

    act(() => { result.current.onSelectParticipant(AGENT_LIST_ROW); });
    // In flight the key IS held — that is the double-click guard, and it stays.
    expect(result.current.getParticipantMenuState(AGENT_LIST_ROW).pending).toBe(true);

    await waitFor(() => { expect(createConversation).toHaveBeenCalledTimes(1); });
    await waitFor(() => { expect(result.current.getParticipantMenuState(AGENT_LIST_ROW).pending).toBeUndefined(); });
    expect(addedBodies).toHaveLength(0);
  });

  it('still refuses without a project, which is the other half of the route', async () => {
    const createConversation = vi.fn(() => Promise.resolve({ id: 77 }));
    const { result } = renderHook(() => useAddEntityParticipant({
      projectId: undefined,
      conversationId: undefined,
      participants: NO_PARTICIPANTS,
      createConversation,
    }), { wrapper: createWrapper() });

    act(() => { result.current.onSelectParticipant(AGENT_LIST_ROW); });

    await Promise.resolve();
    expect(createConversation).not.toHaveBeenCalled();
    expect(addedBodies).toHaveLength(0);
  });
});
