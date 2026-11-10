import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { ParticipantsSheet } from '@/features/chat/ParticipantsSheet';
import { participantAgentId } from '@/features/chat/messageModel';
import { SAMPLE_PUBLIC_AGENTS } from '@/features/designGallery/secondaryFixtures';
import { PublicAgentView } from '@/features/discover/PublicAgentView';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

// The two ways into an agent's memory (#1663): the participant sheet and a Discover agent.
const P = 7;
const me = { id: 1, entity_name: 'user', entity_meta: { id: 5 }, entity_settings: {}, meta: {} };
const agent = { id: 2, entity_name: 'application', entity_meta: { id: '41', project_id: P }, entity_settings: {}, meta: { name: 'Mira' } };
const pipeline = { id: 3, entity_name: 'pipeline', entity_meta: { id: 42, project_id: P }, entity_settings: {}, meta: { name: 'Flow' } };

let queries: QueryClient;
const withQueries = (ui: ReactElement) => <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
});
afterEach(() => queries.clear());

it("reads an agent participant's application id, digit string or number; not a pipeline's", () => {
  expect(participantAgentId(agent as never)).toBe(41);
  expect(participantAgentId({ ...agent, entity_meta: { id: 9 } } as never)).toBe(9);
  expect(participantAgentId(pipeline as never)).toBeNull();
  expect(participantAgentId(me as never)).toBeNull();
});

const sheet = (onAgentMemory?: (id: number, name: string) => void) => (
  <ParticipantsSheet
    visible
    onClose={() => {}}
    projectId={P}
    conversationId="3"
    participants={[me, agent, pipeline] as never[]}
    selfUserId={5}
    offline={false}
    onChanged={() => {}}
    onAgentMemory={onAgentMemory}
  />
);

it('the participant sheet offers "What it remembers" on an agent, and opens it with the application id', async () => {
  const open = jest.fn();
  await render(withQueries(sheet(open)));
  expect(screen.getAllByText('What it remembers')).toHaveLength(1);
  await fireEvent.press(screen.getByLabelText('What Mira remembers'));
  expect(open).toHaveBeenCalledWith(41, 'Mira');
});

it('without the 1.44 contract (no handler) the sheet offers nothing', async () => {
  await render(withQueries(sheet()));
  expect(screen.queryByText('What it remembers')).toBeNull();
});

it('a Discover agent offers "What it remembers" when the workspace speaks 1.44', async () => {
  const onMemory = jest.fn();
  const view = (memory?: () => void) =>
    withQueries(
      <PublicAgentView
        agent={SAMPLE_PUBLIC_AGENTS[0]}
        projectId={P}
        projectLabel="My project"
        welcome={null}
        starters={[]}
        busy={null}
        error={null}
        onStart={jest.fn()}
        onStarter={jest.fn()}
        onMemory={memory}
      />,
    );
  const rendered = await render(view(onMemory));
  await fireEvent.press(screen.getByTestId('public-agent-memory'));
  expect(onMemory).toHaveBeenCalledTimes(1);
  await rendered.rerender(view());
  expect(screen.queryByTestId('public-agent-memory')).toBeNull();
});
