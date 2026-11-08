import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import ThreadScreen from '@/app/(app)/c/[id]';
import { prepareTurn, startTurn, TurnInputError, TurnStartError } from '@/api/queries/sendMessage';
import { ParticipantsSheet } from '@/features/chat/ParticipantsSheet';
import { clearTargets } from '@/features/chat/targetStore';
import { clearLiveTurns } from '@/features/chat/turnController';
import { TURN_TAKING_COPY } from '@/features/chat/turnTakingCopy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

jest.mock('expo-router', () => ({
  useIsFocused: () => true,
  router: { push: jest.fn(), replace: jest.fn(), dismissTo: jest.fn() },
  useLocalSearchParams: () => ({ id: '3' }),
  Stack: { Screen: () => null },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));

// ADR-0050 decisions 7c-7f on mobile (#1612) and the multi-agent first send (#1624).

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const Q = '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f';

const me = { id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} };
const agent = (id: number, name: string) => ({
  id,
  entity_name: 'application',
  kind: 'agent',
  entity_meta: { id: id * 10, project_id: P },
  entity_settings: { version_id: id * 100 },
  meta: { name },
});
const group = (id: number, author: number, text: string, extra: Record<string, unknown> = {}) => ({
  id,
  uuid: `00000000-0000-4000-8000-${String(id).padStart(12, '0')}`,
  author_participant_id: author,
  created_at: `2026-11-08T10:00:${String(id).padStart(2, '0')}Z`,
  message_items: [{ item_type: 'text_message', order_index: 0, item_details: { content: text } }],
  ...extra,
});

function installServer(detail: { participants: unknown[]; message_groups?: unknown[]; meta?: Record<string, unknown> }): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, {
    status: 200,
    body: {
      id: '3',
      uuid: UUID,
      project_id: String(P),
      name: 'team',
      created_at: '2026-11-08T10:00:00Z',
      updated_at: '2026-11-08T10:00:00Z',
      meta: detail.meta ?? {},
      participants: detail.participants,
      message_groups: detail.message_groups ?? [],
    },
  });
  server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
  // Refused, so no stream opens: the request body is what these tests check.
  server.on('POST', `/elitea_core/messages/prompt_lib/${P}/${UUID}`, { status: 422, body: { error: 'rejected_for_test' } });
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

const sends = (server: FakeServer) => server.requests.filter((r) => r.method === 'POST' && r.url.pathname.includes('/messages/'));

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  clearTargets();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

describe('send body: addressed_to', () => {
  it('prepareTurn adds addressed_to (deduped) only when non-empty, and refuses a bad id', () => {
    const base = { projectId: 7, conversationUuid: UUID, participantId: 3, questionId: Q, userInput: 'hi' };
    expect(JSON.parse(prepareTurn({ ...base, addressedTo: [5, 3, 5] }).bodyJson).addressed_to).toEqual([5, 3]);
    expect(prepareTurn({ ...base, addressedTo: [] }).bodyJson).not.toContain('addressed_to');
    expect(() => prepareTurn({ ...base, addressedTo: [0] })).toThrow(TurnInputError);
    expect(() => prepareTurn({ ...base, addressedTo: Array.from({ length: 17 }, (_, i) => i + 1) })).toThrow(TurnInputError);
  });

  it('an @-mention of an agent of the conversation addresses it; a plain message does not', async () => {
    const server = installServer({ participants: [me, agent(2, 'Research'), agent(5, 'Writer')], message_groups: [group(1, 1, 'q'), group(2, 2, 'a')] });
    await renderWithQueries(<ThreadScreen />);
    const input = await screen.findByLabelText('Message');
    await fireEvent.changeText(input, 'ask @Wri');
    await fireEvent.press(await screen.findByText('Writer'));
    await fireEvent.changeText(screen.getByLabelText('Message'), 'ask @Writer to check');
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(sends(server)).toHaveLength(1));
    // The sticky target (Research answered last) stays the participant; the mention addresses Writer.
    expect(JSON.parse(sends(server)[0].body ?? '{}')).toMatchObject({ participant_id: 2, addressed_to: [5] });
  });

  it('a 422 addressee_not_in_conversation reads as a sentence, not a code', async () => {
    const server = fakeServer();
    server.on('POST', `/elitea_core/messages/prompt_lib/7/${UUID}`, { status: 422, body: { error: 'addressee_not_in_conversation' } });
    const prepared = prepareTurn({ projectId: 7, conversationUuid: UUID, participantId: 3, questionId: Q, userInput: 'hi', addressedTo: [99] });
    await expect(startTurn(prepared, { client: server.client })).rejects.toMatchObject({ failure: { kind: 'addressee_not_in_conversation' } });
    await expect(startTurn(prepared, { client: server.client })).rejects.toBeInstanceOf(TurnStartError);
  });
});

describe('#1624: a new conversation with several agents asks who answers', () => {
  it('shows "Choose an agent" and opens the picker on send instead of sending to the first agent', async () => {
    const server = installServer({ participants: [me, agent(2, 'Research'), agent(5, 'Writer')] });
    await renderWithQueries(<ThreadScreen />);
    expect(await screen.findByLabelText(TURN_TAKING_COPY.chooseAgent)).toBeTruthy();
    await fireEvent.changeText(screen.getByLabelText('Message'), 'hello team');
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));
    expect(await screen.findByText(TURN_TAKING_COPY.chooseAgentFirst)).toBeTruthy();
    expect(sends(server)).toHaveLength(0);
    // The picker is open: choosing Writer and sending again sends to it.
    await fireEvent.press(await screen.findByRole('radio', { name: 'Writer' }));
    expect(screen.getByLabelText('To: Writer')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(sends(server)).toHaveLength(1));
    expect(JSON.parse(sends(server)[0].body ?? '{}')).toMatchObject({ participant_id: 5, payload: { user_input: 'hello team' } });
  });

  it('one agent: unchanged, the message goes to it', async () => {
    const server = installServer({ participants: [me, agent(2, 'Research')] });
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.changeText(await screen.findByLabelText('Message'), 'hi');
    expect(screen.queryByLabelText(TURN_TAKING_COPY.chooseAgent)).toBeNull();
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(sends(server)).toHaveLength(1));
    expect(JSON.parse(sends(server)[0].body ?? '{}')).toMatchObject({ participant_id: 2 });
  });

  it('with a lead floor the chip names the lead, and nothing asks', async () => {
    installServer({
      participants: [me, agent(2, 'Research'), agent(5, 'Writer')],
      meta: { floor: { policy: 'lead', lead_participant_id: 5 } },
    });
    await renderWithQueries(<ThreadScreen />);
    expect(await screen.findByLabelText('To: Writer (lead)')).toBeTruthy();
  });
});

describe('side talk and system lines', () => {
  it('folds consecutive agent-to-agent messages into one expandable row; a system line is plain', async () => {
    installServer({
      participants: [me, agent(2, 'Research'), agent(5, 'Writer')],
      message_groups: [
        group(1, 1, 'Plan the launch'),
        group(2, 2, 'Writer, draft it', { author_kind: 'agent', visibility: 'side', cause: 'agent_mention', addressed_to: [5] }),
        group(3, 5, 'Draft done', { author_kind: 'agent', visibility: 'side', cause: 'agent_mention', addressed_to: [2] }),
        group(4, 2, 'Here is the plan', { author_kind: 'agent' }),
        group(5, 0, 'Agents stopped: too many turns in a row.', { author_kind: 'system', cause: 'system' }),
      ],
    });
    await renderWithQueries(<ThreadScreen />);
    const fold = await screen.findByText(TURN_TAKING_COPY.sideConversation(2));
    expect(screen.queryByText('Draft done')).toBeNull();
    await fireEvent.press(fold);
    expect(screen.getByText('Research → Writer')).toBeTruthy();
    expect(screen.getByText('Writer → Research')).toBeTruthy();
    expect(screen.getByText('Draft done')).toBeTruthy();
    expect(screen.getByTestId('system-line')).toBeTruthy();
    expect(screen.getByText('Here is the plan')).toBeTruthy();
  });
});

describe('lead toggle in the participants sheet', () => {
  it('badges the lead, and Make lead / Clear lead update the conversation meta keeping its other keys', async () => {
    const server = installServer({ participants: [] });
    server.on('PUT', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: {} });
    const onChanged = jest.fn();
    const participants = [me, agent(2, 'Research'), agent(5, 'Writer')] as never[];
    const sheet = (meta: unknown) => (
      <ParticipantsSheet
        visible
        onClose={() => {}}
        projectId={P}
        conversationId="3"
        participants={participants}
        selfUserId={7}
        offline={false}
        onChanged={onChanged}
        conversationMeta={meta}
      />
    );
    const view = await renderWithQueries(sheet({ instructions_mode: 'x' }));
    expect(screen.queryByText(/· Lead$/)).toBeNull();
    await fireEvent.press(screen.getByLabelText('Make Writer the lead'));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const put = server.requests.filter((r) => r.method === 'PUT');
    expect(JSON.parse(put[0].body ?? '{}')).toEqual({ meta: { instructions_mode: 'x', floor: { policy: 'lead', lead_participant_id: 5 } } });

    await view.rerender(<QueryClientProvider client={queries!}>{sheet({ instructions_mode: 'x', floor: { policy: 'lead', lead_participant_id: 5 } })}</QueryClientProvider>);
    expect(screen.getByText('Agent · Lead')).toBeTruthy();
    await fireEvent.press(screen.getByLabelText('Stop Writer being the lead'));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(2));
    expect(JSON.parse(server.requests.filter((r) => r.method === 'PUT')[1].body ?? '{}')).toEqual({ meta: { instructions_mode: 'x' } });
  });
});
