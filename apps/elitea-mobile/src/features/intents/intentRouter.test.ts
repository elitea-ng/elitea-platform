import { MAX_PREFILL, agentEntityId, parseAgentEntityId, routeIntent } from '@/features/intents/intentRouter';

const WS = 'aaaaaaaaaaaa';
const inv = (name: string, params: Record<string, unknown> = {}) => ({ name, params });

describe('routeIntent (open-app intents only)', () => {
  it('Open notifications', () => {
    expect(routeIntent(inv('openNotifications'), WS)).toEqual({ kind: 'open', href: '/notifications' });
  });

  it('New chat with an agent of the active workspace', () => {
    const action = routeIntent(inv('newChat', { agentId: `${WS}:90501:5` }), WS);
    expect(action).toEqual({ kind: 'newChat', agent: { projectId: 90501, agentId: 5 }, prefill: null });
  });

  it("an agent of another workspace (a shortcut saved before a switch) opens the active workspace's picker", () => {
    expect(routeIntent(inv('newChat', { agentId: 'bbbbbbbbbbbb:90501:5' }), WS)).toEqual({ kind: 'newChat', agent: null, prefill: null });
    expect(routeIntent(inv('newChat', { agentId: `${WS}:90501:5` }), null)).toEqual({ kind: 'newChat', agent: null, prefill: null });
  });

  it('Ask <agent> carries the question to the composer, bounded; it is never sent by the intent', () => {
    const action = routeIntent(inv('askAgent', { agentId: `${WS}:7:3`, text: '  Summarise the\r\nweek  ' }), WS);
    expect(action).toEqual({ kind: 'newChat', agent: { projectId: 7, agentId: 3 }, prefill: 'Summarise the\nweek' });
    const long = routeIntent(inv('askAgent', { text: 'x'.repeat(MAX_PREFILL + 10) }), WS);
    expect((long as { prefill: string }).prefill).toHaveLength(MAX_PREFILL);
    expect(routeIntent(inv('askAgent', { text: '   ' }), WS)).toEqual({ kind: 'newChat', agent: null, prefill: null });
  });

  it('Ask <agent> by voice carries no question (Siri never collects one): an empty composer for that agent', () => {
    expect(routeIntent(inv('askAgent', { agentId: `${WS}:7:3` }), WS)).toEqual({ kind: 'newChat', agent: { projectId: 7, agentId: 3 }, prefill: null });
  });

  it('discards what this build does not know', () => {
    expect(routeIntent(inv('deleteEverything'), WS)).toEqual({ kind: 'discard' });
  });
});

describe('agent entity ids', () => {
  it('round-trips and refuses malformed ids', () => {
    expect(parseAgentEntityId(agentEntityId({ workspaceId: WS, projectId: 7, agentId: 3 }))).toEqual({ workspaceId: WS, projectId: 7, agentId: 3 });
    for (const bad of ['', 'x', `${WS}:0:3`, `${WS}:7:-1`, `${WS}:7`, `a b:7:3`, 42, null]) expect(parseAgentEntityId(bad)).toBeNull();
  });
});
