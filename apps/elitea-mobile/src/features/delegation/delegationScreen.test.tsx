import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { Alert } from 'react-native';

import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import AgentDelegationsScreen from '@/app/(app)/agent-delegations/[id]';
import { DelegationGrantsView, GOVERNED_NOTE, REAPPROVAL_GOVERNED_NOTE } from '@/features/delegation/DelegationGrantsView';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

let mockParams: Record<string, string> = { id: '41', name: 'Mira' };
let mockTitle: unknown = null;
jest.mock('expo-router', () => ({
  useLocalSearchParams: () => mockParams,
  Stack: {
    Screen: ({ options }: { options: { title?: string } }) => {
      mockTitle = options.title;
      return null;
    },
  },
}));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const P = 7;
const LIST = `/elitea_core/delegation_grants/prompt_lib/${P}/41`;
const ONE = (id: number) => `/elitea_core/delegation_grant/prompt_lib/${P}/41/${id}`;
const AGENTS = `/elitea_core/applications/prompt_lib/${P}`;

const grant = (id: number, extra: Record<string, unknown> = {}) => ({
  id,
  caller_application_id: 41,
  callee_application_id: 50 + id,
  edge_kind: 'agent_tool',
  state: 'pending',
  governed: false,
  legacy: false,
  items: [{ kind: 'toolkit', name: 'toolkit:7' }, { kind: 'tool', name: 'git_push', toolkit: 'toolkit:7' }, { kind: 'egress_host', name: 'git.corp.example' }],
  omitted: [],
  ...extra,
});

function install(contract = '1.59'): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  server.on('GET', AGENTS, { status: 200, body: { rows: [{ id: 51, name: 'Git specialist' }], total: 1 } });
  return server;
}

let queries: QueryClient;
const withQueries = (ui: ReactElement) => <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;
const settle = () => act(() => new Promise<void>((resolve) => setImmediate(() => resolve())));

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  mockParams = { id: '41', name: 'Mira' };
  queries = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, workspaceId: 'aaaaaaaaaaaa', projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.restoreAllMocks();
});
afterEach(() => queries.clear());

it('lists what each attached agent brings, with the state and the callee name', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(1), grant(2, { state: 'approved' }), grant(3, { state: 'stale' })] } });
  await render(withQueries(<AgentDelegationsScreen />));
  expect(await screen.findByText('Git specialist')).toBeTruthy();
  expect(mockTitle).toBe("Mira's delegations");
  expect(screen.getByText('Agent tool · Awaiting approval'.replace('Agent tool', 'Agent used as a tool'))).toBeTruthy();
  expect(screen.getByText('Agent used as a tool · Approved')).toBeTruthy();
  expect(screen.getByText('Agent used as a tool · Needs re-approval')).toBeTruthy();
  expect(screen.getAllByText('Tool git_push (toolkit:7)').length).toBe(3);
  expect(screen.getAllByText('Egress host git.corp.example').length).toBe(3);
});

it('lands on the edge a chat notice named: its grant is listed first and marked', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(1), grant(2), grant(3)] } });
  // grant(3) calls agent 53.
  mockParams = { id: '41', name: 'Mira', callee: '53' };
  await render(withQueries(<AgentDelegationsScreen />));
  await screen.findByTestId('delegation-grant-3');
  const rows = screen.getAllByTestId(/^delegation-grant-\d$/).map((row) => row.props.testID);
  expect(rows).toEqual(['delegation-grant-3', 'delegation-grant-1', 'delegation-grant-2']);
  expect(screen.getByTestId('delegation-grant-3').props.accessibilityState).toEqual({ selected: true });
  expect(screen.getByTestId('delegation-grant-1').props.accessibilityState).toEqual({ selected: false });
});

it('ignores a callee param that is not an id', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(1), grant(2)] } });
  mockParams = { id: '41', name: 'Mira', callee: 'abc' };
  await render(withQueries(<AgentDelegationsScreen />));
  await screen.findByTestId('delegation-grant-1');
  expect(screen.getAllByTestId(/^delegation-grant-\d$/).map((row) => row.props.testID)).toEqual(['delegation-grant-1', 'delegation-grant-2']);
});

it('approves a pending grant only after the confirmation, then reads the list again', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(1)] } });
  server.on('POST', `${ONE(1)}/approve`, { status: 200, body: grant(1, { state: 'approved' }) });
  await render(withQueries(<AgentDelegationsScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Approve delegation to Git specialist' }));
  expect(server.requests.some((r) => r.method === 'POST')).toBe(false);
  expect(alert.mock.calls[0][0]).toBe('Approve Git specialist?');
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Approve')!.onPress!());
  await settle();
  const post = server.requests.find((r) => r.method === 'POST');
  expect(post?.url.pathname).toBe(`/api/v2${ONE(1)}/approve`);
  // No body: the server approves everything the caller holds access to.
  expect(post?.body ?? '').toBe('');
  await waitFor(() => expect(server.requests.filter((r) => r.method === 'GET' && r.url.pathname.endsWith('/41')).length).toBeGreaterThan(1));
});

it('shows what a stale grant now also holds and re-approves it after a confirmation', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install('1.62');
  server.on('GET', LIST, {
    status: 200,
    body: { grants: [grant(3, { state: 'stale', drift: { callee_version_id: 9, digest: 'd', added: [{ kind: 'tool', name: 'git_delete', toolkit: 'toolkit:7' }] } })] },
  });
  server.on('POST', `${ONE(3)}/approve`, { status: 200, body: grant(4, { state: 'approved' }) });
  await render(withQueries(<AgentDelegationsScreen />));
  expect(await screen.findByText('Agent 53 now also holds, not approved yet')).toBeTruthy();
  expect(screen.getByText('Tool git_delete (toolkit:7)')).toBeTruthy();
  await fireEvent.press(screen.getByRole('button', { name: 'Re-approve delegation to Agent 53' }));
  expect(server.requests.some((r) => r.method === 'POST')).toBe(false);
  expect(alert.mock.calls[0][0]).toBe('Re-approve Agent 53?');
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Approve')!.onPress!());
  await settle();
  expect(server.requests.find((r) => r.method === 'POST')?.url.pathname).toBe(`/api/v2${ONE(3)}/approve`);
});

it('points a governed stale grant to its change request when re-approval is asked', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install('1.62');
  server.on('GET', LIST, { status: 200, body: { grants: [grant(3, { state: 'stale', governed: true })] } });
  server.on('POST', `${ONE(3)}/approve`, { status: 409, body: { error: 'governed', code: 'DELEGATION_GOVERNED', change_request_id: 8 } });
  await render(withQueries(<AgentDelegationsScreen />));
  expect(await screen.findByText(REAPPROVAL_GOVERNED_NOTE)).toBeTruthy();
  await fireEvent.press(screen.getByRole('button', { name: 'Re-approve delegation to Agent 53' }));
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Approve')!.onPress!());
  await settle();
  expect(await screen.findByText('This delegation is decided in a change request. Open it on the web or desktop app to approve it.')).toBeTruthy();
});

it('shows a refused approval in the app words: governed, self-approval, nothing to approve', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(1)] } });
  const cases: [number, unknown, string][] = [
    [409, { error: 'governed', code: 'DELEGATION_GOVERNED', change_request_id: 4 }, 'This delegation is decided in a change request. Open it on the web or desktop app to approve it.'],
    [403, { error: 'the approver must differ from the proposer' }, 'Someone other than the person who proposed this delegation has to approve it.'],
    [400, { error: 'nothing is left to approve: you hold access to none of the items, or none were chosen' }, 'You hold access to none of the items on this delegation, so there is nothing for you to approve.'],
  ];
  await render(withQueries(<AgentDelegationsScreen />));
  for (const [status, body, text] of cases) {
    server.on('POST', `${ONE(1)}/approve`, { status, body });
    await fireEvent.press(await screen.findByRole('button', { name: 'Approve delegation to Git specialist' }));
    await act(async () => alert.mock.calls.at(-1)![2]!.find((b) => b.text === 'Approve')!.onPress!());
    await settle();
    expect(await screen.findByText(text)).toBeTruthy();
  }
});

it('revokes after a confirmation, and a failed revoke says so', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: { grants: [grant(2, { state: 'approved' })] } });
  server.on('DELETE', ONE(2), { status: 500, body: {} });
  await render(withQueries(<AgentDelegationsScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Revoke delegation to Agent 52' }));
  expect(server.requests.some((r) => r.method === 'DELETE')).toBe(false);
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Revoke')!.onPress!());
  await settle();
  expect(await screen.findByText("The delegation wasn't revoked. Try again.")).toBeTruthy();
});

it('says unavailable on a workspace before contract 1.59 and reads nothing', async () => {
  const server = install('1.58');
  await render(withQueries(<AgentDelegationsScreen />));
  expect(await screen.findByText("This workspace doesn't show delegations in the app yet.")).toBeTruthy();
  expect(server.requests.some((r) => r.url.pathname.includes('delegation'))).toBe(false);
});

it('says the agent could not be found for a bad id', async () => {
  install();
  mockParams = { id: 'abc' };
  await render(withQueries(<AgentDelegationsScreen />));
  expect(await screen.findByText('This agent could not be found.')).toBeTruthy();
});

describe('DelegationGrantsView', () => {
  const base = { agentName: 'Mira', names: {}, busy: null, banner: null, selectable: false, onApprove: () => {}, onRevoke: () => {}, onRetry: () => {} };
  const view = (id: number, extra: Record<string, unknown>) => ({
    id,
    calleeApplicationId: 60 + id,
    edgeKind: 'sub_agent',
    state: 'pending',
    governed: false,
    legacy: false,
    changeRequestId: null,
    items: [{ kind: 'toolkit', name: 'toolkit:7', toolkit: null }],
    omitted: [],
    drift: [],
    ...extra,
  });

  it('shows a governed pending grant as awaiting its change request, with no Approve', async () => {
    await render(<DelegationGrantsView {...base} status={{ kind: 'ready' }} grants={[view(1, { governed: true, changeRequestId: 3 })]} />);
    expect(screen.getByText(GOVERNED_NOTE)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Approve delegation/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Revoke delegation/ })).toBeNull();
  });

  it('lists what was left out apart from what the grant covers', async () => {
    await render(
      <DelegationGrantsView
        {...base}
        status={{ kind: 'ready' }}
        grants={[view(2, { state: 'approved', omitted: [{ kind: 'credential', name: 'ref-1', toolkit: 'toolkit:7' }] })]}
      />,
    );
    expect(screen.getByText('Left out, because nobody who approved holds access to it')).toBeTruthy();
    expect(screen.getByText('Credential reference ref-1 (toolkit:7)')).toBeTruthy();
    expect(screen.getByText('Agent 62 can use')).toBeTruthy();
  });

  it('draws loading, empty and error states', async () => {
    const { rerender } = await render(<DelegationGrantsView {...base} status={{ kind: 'loading' }} grants={[]} />);
    expect(screen.queryByText('No delegations')).toBeNull();
    await rerender(<DelegationGrantsView {...base} status={{ kind: 'ready' }} grants={[]} />);
    expect(screen.getByText('No delegations')).toBeTruthy();
    await rerender(<DelegationGrantsView {...base} status={{ kind: 'error', message: 'boom' }} grants={[]} />);
    expect(screen.getByText('boom')).toBeTruthy();
  });
});
