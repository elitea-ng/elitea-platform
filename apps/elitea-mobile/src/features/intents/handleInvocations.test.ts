import { readHandoff } from '@/features/intents/intentHandoff';
import { handleInvocations } from '@/features/intents/useAppIntentHandler';
import { __resetSessionForTests, setSession } from '@/state/session';

let mockUuid = 0;
jest.mock('expo-crypto', () => ({ randomUUID: () => `00000000-0000-4000-8000-${String(++mockUuid).padStart(12, '0')}` }));

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: 'aaaaaaaaaaaa', projectId: 7 });
});

const inv = (id: string, name: string, params: Record<string, unknown> = {}, createdAt = Date.now()) => ({ id, name, params, createdAt });

it('removes each invocation BEFORE opening its screen (a replay must not start a second conversation)', async () => {
  const order: string[] = [];
  const navigate = jest.fn((href: unknown) => {
    const h = href as { pathname?: string; params?: { intent?: string } };
    // /new gets the agent in memory, under a one-time id: show what the id stands for.
    order.push(`open ${h.pathname === '/new' ? `/new ${JSON.stringify(readHandoff(h.params?.intent))}` : JSON.stringify(href)}`);
  });
  const remove = jest.fn(async (id: string) => {
    order.push(`remove ${id}`);
  });
  await handleInvocations(
    [inv('a', 'newChat', { agentId: 'aaaaaaaaaaaa:7:5' }), inv('b', 'openNotifications'), inv('c', 'unknownIntent')],
    navigate,
    remove,
  );
  expect(order).toEqual([
    'remove a',
    'open /new {"agent":{"projectId":7,"agentId":5},"prefill":null}',
    'remove b',
    'open "/notifications"',
    'remove c',
  ]);
});

it('skips an invocation it could not dequeue, and handles the rest', async () => {
  const navigate = jest.fn();
  const remove = jest.fn(async (id: string) => {
    if (id === 'a') throw new Error('queue unreadable');
  });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  await handleInvocations([inv('a', 'openNotifications'), inv('b', 'askAgent', { text: 'Hi' })], navigate, remove);
  expect(navigate).toHaveBeenCalledTimes(1);
  const href = navigate.mock.calls[0]![0] as { pathname: string; params: { intent: string } };
  expect(href.pathname).toBe('/new');
  expect(readHandoff(href.params.intent)).toEqual({ agent: null, prefill: 'Hi' });
});

it('drops an invocation queued long ago (signed out then, another workspace now): removed, nothing opens', async () => {
  const navigate = jest.fn();
  const remove = jest.fn(async () => {});
  const hoursAgo = Date.now() - 3 * 60 * 60_000;
  await handleInvocations([inv('old', 'askAgent', { text: 'meant for another workspace' }, hoursAgo), inv('new', 'openNotifications')], navigate, remove);
  expect(remove).toHaveBeenCalledWith('old');
  expect(navigate).toHaveBeenCalledTimes(1);
  expect(navigate).toHaveBeenCalledWith('/notifications');
});
