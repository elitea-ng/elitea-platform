import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { AccessibilityInfo, Text } from 'react-native';

import { applyAgentStateFrame, avatarMotion, type AvatarStates } from '@elitea/client-core/conversation';
import type { SseEvent } from '@elitea/client-core/stream';
import { DesignProvider } from '@/design/DesignProvider';
import { AgentMotion } from '@/features/chat/AgentMotion';
import { parseFrame } from '@/features/chat/frames';
import { liveActivityLabel } from '@/features/chat/liveActivity';
import { toChatMessages } from '@/features/chat/messageModel';
import { ParticipantsSheet } from '@/features/chat/ParticipantsSheet';
import { LiveReply, PersistedMessage } from '@/features/chat/ThreadRows';
import { sampleLive } from '@/features/designGallery/chatFixtures';
import { agentExecutorOf, ranOutsideSandbox } from '@/features/localWork/model';
import { TEAM_COPY } from '@/features/localWork/teamCopy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

// ADR-0061 Parity (#3069): a Local work thread's team, viewed read-only on mobile.
const P = 7;
const me = { id: 1, entity_name: 'user', entity_meta: { id: 5 }, entity_settings: {}, meta: {} };
const mira = { id: 77, entity_name: 'application', entity_meta: { id: 50, name: 'Mira' }, entity_settings: { version_id: 9 }, meta: {} };
const doctor = { id: 88, entity_name: 'application', entity_meta: { id: 60, name: 'CI Doctor' }, entity_settings: { version_id: 10, executor: 'cloud' }, meta: {} };
const LEAD_META = { floor: { policy: 'lead', lead_participant_id: 88 } };

let queries: QueryClient;
let server: FakeServer;
const withQueries = (ui: ReactElement) => <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
});
afterEach(() => {
  queries.clear();
  jest.restoreAllMocks();
});

describe('the thread info sheet of a Local work thread', () => {
  it('shows each agent with a Local or Cloud badge and the lead, and offers no lead change or removal', async () => {
    await render(
      withQueries(
        <ParticipantsSheet
          visible
          onClose={() => {}}
          projectId={P}
          conversationId="3"
          participants={[me, mira, doctor] as never[]}
          selfUserId={5}
          offline={false}
          onChanged={() => {}}
          conversationMeta={LEAD_META}
          teamView
        />,
      ),
    );
    expect(within(screen.getByTestId('executor-badge-77')).getByText(TEAM_COPY.executorLocal)).toBeTruthy();
    expect(within(screen.getByTestId('executor-badge-88')).getByText(TEAM_COPY.executorCloud)).toBeTruthy();
    expect(screen.getByText('Agent · Cloud · Lead')).toBeTruthy();
    expect(screen.getByText('Agent · Local')).toBeTruthy();
    expect(screen.queryByText('Make lead')).toBeNull();
    expect(screen.queryByText('Clear lead')).toBeNull();
    expect(screen.queryByText('Remove')).toBeNull();
    expect(screen.queryByText('Add agent')).toBeNull();
  });

  it('reads where an agent runs from its mapping', () => {
    expect(agentExecutorOf(doctor)).toBe('cloud');
    expect(agentExecutorOf(mira)).toBe('local');
    expect(agentExecutorOf({ entity_settings: null })).toBe('local');
  });
});

describe('the "Ran outside the sandbox" marker', () => {
  const detail = (meta: unknown) => ({
    id: 3,
    participants: [me, mira],
    message_groups: [{ id: 10, uuid: 'g-10', author_participant_id: 77, created_at: '2026-11-25T10:00:00Z', meta, message_items: [{ item_type: 'text_message', item_details: { content: 'Done.' } }] }],
  });

  it('marks the flagged answer only, and nothing while the field is absent', async () => {
    expect(ranOutsideSandbox({ local_work: { ran_outside_sandbox: true } })).toBe(true);
    for (const meta of [undefined, null, {}, { local_work: {} }, { local_work: { ran_outside_sandbox: 'yes' } }]) {
      expect(ranOutsideSandbox(meta)).toBe(false);
    }
    const [flagged] = toChatMessages(detail({ local_work: { ran_outside_sandbox: true } }) as never);
    const [plain] = toChatMessages(detail({ executed_by: 'desktop' }) as never);
    expect(flagged?.ranOutsideSandbox).toBe(true);
    expect(plain?.ranOutsideSandbox).toBeUndefined();
    const props = { highlight: { people: [], agents: [] }, opening: null, onOpenFile: null, tools: [], onOpenTool: () => {}, onActions: () => {} };
    const view = await render(<PersistedMessage message={flagged!} {...props} />);
    expect(screen.getByTestId('ran-outside-sandbox')).toBeTruthy();
    expect(screen.getByText(TEAM_COPY.ranOutsideSandbox)).toBeTruthy();
    await view.rerender(<PersistedMessage message={plain!} {...props} />);
    expect(screen.queryByTestId('ran-outside-sandbox')).toBeNull();
  });
});

describe('the live reply of a cloud thread', () => {
  const frame = (data: object): SseEvent => ({ event: 'execution.node_event', data: JSON.stringify(data) }) as SseEvent;
  const stateOf = (data: object, states: AvatarStates = {}) => {
    const parsed = parseFrame(frame(data));
    if (parsed.kind !== 'node') throw new Error('not a node frame');
    return applyAgentStateFrame(states, parsed.node, 1_000);
  };
  const names = (id: number) => ({ 77: 'Mira', 88: 'CI Doctor' })[id];
  const replyProps = { author: 'Mira', onRetry: null, onReconnect: () => {}, onDismiss: () => {}, onOpenTool: () => {}, usageAvailable: true } as const;

  it('an agent_state frame with activity.tool says "Running <tool>"', async () => {
    const states = stateOf({ type: 'agent_state', participant_id: 77, addressed_to: [], state: 'working', activity: { tool: 'jira_get_issue', tool_kind: 'read' }, response_metadata: {} });
    const label = liveActivityLabel(states, 77, 1_000, names);
    expect(label).toBe('Running jira_get_issue');
    await render(<LiveReply live={sampleLive('thinking')} {...replyProps} activity={label} />);
    expect(screen.getByText('Mira · Running jira_get_issue')).toBeTruthy();
  });

  it('names the agent waited on, and shows no label before any frame', () => {
    const states = stateOf({ type: 'agent_state', participant_id: 77, state: 'waiting', activity: { waiting_on: 'agent', peer_participant_id: 88 } });
    expect(liveActivityLabel(states, 77, 1_000, names)).toBe('Waiting for CI Doctor');
    expect(liveActivityLabel({}, 77, 1_000, names)).toBeNull();
    expect(liveActivityLabel(states, null, 1_000, names)).toBeNull();
  });

  it('under Reduce Motion shows the still and still says what the agent does', async () => {
    jest.spyOn(AccessibilityInfo, 'isReduceMotionEnabled').mockResolvedValue(true);
    const states = stateOf({ type: 'agent_state', participant_id: 77, state: 'working', activity: { tool: 'grep' } });
    const motion = avatarMotion({ participantId: 77, newest: true }, states, 1_000, { reducedMotion: true });
    expect(motion).toBe('still');
    await render(
      <DesignProvider>
        <LiveReply
          live={sampleLive('thinking')}
          {...replyProps}
          avatar={
            <AgentMotion motion={motion} size={28}>
              <Text>M</Text>
            </AgentMotion>
          }
          activity={liveActivityLabel(states, 77, 1_000, names)}
        />
      </DesignProvider>,
    );
    expect(await screen.findByText('Mira · Running grep')).toBeTruthy();
    expect(screen.queryByTestId('agent-motion-working', { includeHiddenElements: true })).toBeNull();
  });
});
