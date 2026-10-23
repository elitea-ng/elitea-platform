import { act, render } from '@testing-library/react-native';

import { TurnController, clearLiveTurns, setLiveTurn, turnKey } from '@/features/chat/turnController';
import { __setWidgetSinkForTests, type WidgetSink } from '@/features/surfaces/surfaceWriter';
import { useWidgetSnapshot } from '@/features/surfaces/useWidgetSnapshot';
import { resetPolicy, setPolicy } from '@/security/policy';
import { bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
let mockUnread = 0;
jest.mock('@/api/queries/notifications', () => ({ useUnseenCount: () => ({ data: mockUnread }) }));

function Probe() {
  useWidgetSnapshot();
  return null;
}

let sink: { updateTimeline: jest.Mock };
const lastProps = () => sink.updateTimeline.mock.lastCall?.[0][0].props;

beforeEach(() => {
  __resetSessionForTests();
  // The app binds a workspace before the session names it (src/state/workspace.tsx).
  bindWorkspace('aaaaaaaaaaaa');
  resetPolicy();
  clearLiveTurns();
  mockUnread = 0;
  sink = { updateTimeline: jest.fn() };
  __setWidgetSinkForTests(sink as WidgetSink);
});

it('writes the active workspace counts, and follows a live turn', async () => {
  mockUnread = 3;
  setSession({ serverOrigin: 'https://acme-legal.elitea.example', workspaceId: 'aaaaaaaaaaaa', projectId: 7 });
  await render(<Probe />);
  expect(lastProps()).toMatchObject({ signedIn: true, unread: 3, running: 0, approvals: 0, host: '' });

  const controller = TurnController.create({ projectId: 7, conversationUuid: 'aaaaaaaa-0000-4000-8000-000000000001', participantId: 1, userInput: 'hi', questionId: '0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e' });
  await act(async () => setLiveTurn(turnKey(7, 'aaaaaaaa-0000-4000-8000-000000000001'), controller));
  // A new controller is 'sending' until its stream settles.
  expect(lastProps()).toMatchObject({ running: 1 });
  await act(async () => setLiveTurn(turnKey(7, 'aaaaaaaa-0000-4000-8000-000000000001'), null));
  expect(lastProps()).toMatchObject({ running: 0 });
});

it('shows the host only once the workspace policy allows titles', async () => {
  setSession({ serverOrigin: 'https://acme-legal.elitea.example', workspaceId: 'aaaaaaaaaaaa', projectId: 7 });
  await render(<Probe />);
  expect(lastProps().host).toBe('');
  await act(async () => setPolicy({ allow_system_surfaces: true }, 'server'));
  expect(lastProps().host).toBe('acme-legal.elitea.example');
});

it('signed out: the signed-out snapshot', async () => {
  await render(<Probe />);
  expect(lastProps()).toMatchObject({ signedIn: false });
});
