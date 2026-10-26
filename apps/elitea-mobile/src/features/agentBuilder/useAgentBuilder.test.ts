import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react-native';
import { createElement, type ReactNode } from 'react';

import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

import { useAgentBuilder } from './useAgentBuilder';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

let mockServer: FakeServer;
jest.mock('@/api/client', () => ({ ...jest.requireActual('@/api/client'), sessionApiClient: () => mockServer.client }));

const P = 7;
const draft = { name: 'Triager', description: 'Triages', instructions: 'Sort by severity.' };

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return createElement(QueryClientProvider, { client }, children);
}

const created = (id: string) => ({ status: 201, body: { id, name: 'Triager' } });
const detail = { status: 200, body: { version_details: { id: 90 } } };

describe('useAgentBuilder save retry', () => {
  beforeEach(() => {
    mockServer = fakeServer();
    __resetSessionForTests();
    setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
    setCredential({ kind: 'pat', token: 't' });
  });

  it('retries the chat for the saved agent when the draft is unchanged, creating nothing twice', async () => {
    mockServer.on('POST', `/elitea_core/applications/prompt_lib/${P}`, created('12'));
    mockServer.on('GET', `/elitea_core/application/prompt_lib/${P}/12`, { status: 500 }, detail);
    mockServer.on('POST', `/elitea_core/conversations/prompt_lib/${P}`, { status: 201, body: { id: 5 } });
    const { result } = await renderHook(() => useAgentBuilder(P), { wrapper });

    await act(async () => expect(await result.current.save(draft)).toBeNull());
    expect(result.current.saveError).toMatch(/is saved/);
    await act(async () => expect(await result.current.save(draft)).toBe(5));

    expect(mockServer.requests.filter((r) => r.method === 'POST' && r.url.pathname.endsWith(`/applications/prompt_lib/${P}`))).toHaveLength(1);
    expect(mockServer.requests.some((r) => r.method === 'DELETE')).toBe(false);
  });

  it('saves an edited draft on retry and deletes the first copy once that succeeds', async () => {
    mockServer.on('POST', `/elitea_core/applications/prompt_lib/${P}`, created('12'), created('13'));
    mockServer.on('GET', `/elitea_core/application/prompt_lib/${P}/12`, { status: 500 });
    mockServer.on('GET', `/elitea_core/application/prompt_lib/${P}/13`, detail);
    mockServer.on('DELETE', `/elitea_core/application/prompt_lib/${P}/12`, { status: 204 });
    mockServer.on('POST', `/elitea_core/conversations/prompt_lib/${P}`, { status: 201, body: { id: 6 } });
    const { result } = await renderHook(() => useAgentBuilder(P), { wrapper });

    await act(async () => expect(await result.current.save(draft)).toBeNull());
    await act(async () => expect(await result.current.save({ ...draft, instructions: 'Edited.' })).toBe(6));

    const creates = mockServer.requests.filter((r) => r.method === 'POST' && r.url.pathname.endsWith(`/applications/prompt_lib/${P}`));
    expect(JSON.parse(creates[1]?.body ?? '{}').versions[0].instructions).toBe('Edited.');
    const order = mockServer.requests.map((r) => `${r.method} ${r.url.pathname.replace('/api/v2', '')}`);
    const secondCreate = order.indexOf(`POST /elitea_core/applications/prompt_lib/${P}`, order.indexOf(`POST /elitea_core/applications/prompt_lib/${P}`) + 1);
    expect(order.indexOf(`DELETE /elitea_core/application/prompt_lib/${P}/12`)).toBeGreaterThan(secondCreate);
    expect(mockServer.requests.filter((r) => r.method === 'DELETE')).toHaveLength(1);
  });

  it('keeps the first copy when the re-save fails', async () => {
    mockServer.on('POST', `/elitea_core/applications/prompt_lib/${P}`, created('12'), { status: 500 });
    mockServer.on('GET', `/elitea_core/application/prompt_lib/${P}/12`, { status: 500 });
    const { result } = await renderHook(() => useAgentBuilder(P), { wrapper });

    await act(async () => expect(await result.current.save(draft)).toBeNull());
    await act(async () => expect(await result.current.save({ ...draft, instructions: 'Edited.' })).toBeNull());
    expect(mockServer.requests.some((r) => r.method === 'DELETE')).toBe(false);
  });
});
