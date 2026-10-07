import * as intentsMock from 'expo-app-intents';

import { AGENT_KIND, MAX_CATALOG_AGENTS, __resetAgentCatalogForTests, buildAgentCatalog, clearAgentCatalogAtLaunch, publishAgentCatalog } from '@/features/intents/agentCatalog';
import { endSession, registerRevokeHook, registerWipeHook, suspendActiveWorkspace } from '@/security/endSession';
import { bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const mock = intentsMock as unknown as { catalogs: Map<string, unknown[]>; pending: { id: string; name: string; params: Record<string, unknown>; createdAt: number }[] };
const agents = [
  { id: 5, name: 'Zefir Concierge', description: 'Public agent' },
  { id: 6, name: '  Contract\nReviewer ', description: '' },
  { id: 5, name: 'Duplicate', description: '' },
  { id: 7, name: '   ', description: '' },
];

beforeEach(() => {
  __resetSessionForTests();
  bindWorkspace('aaaaaaaaaaaa');
  __resetAgentCatalogForTests();
  mock.catalogs.clear();
  mock.pending.splice(0);
});

it('is EMPTY unless the workspace policy allows titles outside the app', () => {
  expect(buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: false, agents })).toEqual([]);
});

it('names the agents with workspace-scoped ids when titles are allowed', () => {
  expect(buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: true, agents })).toEqual([
    { id: 'aaaaaaaaaaaa:7:5', title: 'Zefir Concierge', hideInSpotlight: true },
    { id: 'aaaaaaaaaaaa:7:6', title: 'Contract Reviewer', hideInSpotlight: true },
  ]);
  const many = Array.from({ length: 30 }, (_, i) => ({ id: i + 1, name: `Agent ${i + 1}`, description: '' }));
  expect(buildAgentCatalog({ workspaceId: 'w', projectId: 7, allowTitles: true, agents: many })).toHaveLength(MAX_CATALOG_AGENTS);
});

it('publishes once per change', async () => {
  const entities = buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: true, agents });
  await publishAgentCatalog(entities, 'aaaaaaaaaaaa');
  await publishAgentCatalog(entities, 'aaaaaaaaaaaa');
  expect(intentsMock.setEntityCatalogAsync).toHaveBeenCalledTimes(1);
  expect(mock.catalogs.get(AGENT_KIND)).toHaveLength(2);
});

it('log out and a switch empty the catalog and drop pending invocations (a dictated question among them)', async () => {
  await publishAgentCatalog(buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: true, agents }), 'aaaaaaaaaaaa');
  mock.pending.push({ id: 'i1', name: 'askAgent', params: { text: 'secret question' }, createdAt: 1 });
  await endSession({ reason: 'user_logout' });
  expect(mock.catalogs.get(AGENT_KIND)).toEqual([]);
  expect(mock.pending).toEqual([]);

  bindWorkspace('aaaaaaaaaaaa');
  await publishAgentCatalog([{ id: 'aaaaaaaaaaaa:7:5', title: 'x' }], 'aaaaaaaaaaaa');
  await suspendActiveWorkspace();
  expect(mock.catalogs.get(AGENT_KIND)).toEqual([]);
});

it('agent names republished by a re-render while the session ends or switches do not outlive the wipe', async () => {
  const named = buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: true, agents });
  await publishAgentCatalog(named, 'aaaaaaaaaaaa');
  // A later wipe hook re-renders the signed-in layout, whose effect still sees the old workspace.
  const off = registerWipeHook('zz_rerender', () => publishAgentCatalog(named, 'aaaaaaaaaaaa'));
  try {
    await endSession({ reason: 'user_logout' });
    expect(mock.catalogs.get(AGENT_KIND)).toEqual([]);

    bindWorkspace('aaaaaaaaaaaa');
    await publishAgentCatalog([{ id: 'aaaaaaaaaaaa:7:5', title: 'x' }], 'aaaaaaaaaaaa');
    await suspendActiveWorkspace();
    expect(mock.catalogs.get(AGENT_KIND)).toEqual([]);
  } finally {
    off();
  }
});

it('publishes no names for a workspace that is not the bound one', async () => {
  bindWorkspace('bbbbbbbbbbbb');
  await publishAgentCatalog([{ id: 'aaaaaaaaaaaa:7:5', title: 'x' }], 'aaaaaaaaaaaa');
  expect(mock.catalogs.has(AGENT_KIND)).toBe(false);
});

it('empties the agent catalog BEFORE the server revoke of a log out (a kill during it leaves no agent names in Siri)', async () => {
  await publishAgentCatalog(buildAgentCatalog({ workspaceId: 'aaaaaaaaaaaa', projectId: 7, allowTitles: true, agents }), 'aaaaaaaaaaaa');
  let atRevoke: unknown[] | undefined;
  const off = registerRevokeHook(async () => {
    atRevoke = mock.catalogs.get(AGENT_KIND);
  });
  try {
    await endSession({ reason: 'user_logout' });
  } finally {
    off();
  }
  expect(atRevoke).toEqual([]);
});

it('clearAgentCatalogAtLaunch empties the catalog even when this process published none', async () => {
  mock.catalogs.set(AGENT_KIND, [{ id: 'aaaaaaaaaaaa:7:5', title: 'Left by a killed process' }]);
  await clearAgentCatalogAtLaunch();
  expect(mock.catalogs.get(AGENT_KIND)).toEqual([]);
});
