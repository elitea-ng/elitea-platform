import * as QuickActions from 'expo-quick-actions';
import { Platform } from 'react-native';

import { actionBelongsTo, applyQuickActions, buildQuickActions, clearQuickActions } from '@/features/quickActions/quickActions';
import { handleQuickAction } from '@/features/quickActions/useQuickActions';
import { endSession, registerRevokeHook, registerWipeHook, suspendActiveWorkspace } from '@/security/endSession';
import { bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-router', () => ({ router: { navigate: jest.fn() } }));

const setItems = QuickActions.setItems as jest.Mock;
const W = 'aaaaaaaaaaaa';
const recent = [
  { ref: '41', name: 'Zefir launch planning' },
  { ref: '35a5538d-993d-4c66-bf5f-ad8509c581d4', name: '  Quarterly\nreview  ' },
  { ref: '7', name: 'Third' },
];

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: W, projectId: 7 });
  bindWorkspace(W);
  setItems.mockClear();
});

describe('buildQuickActions', () => {
  it('by default (allow_system_surfaces off) offers New chat and Notifications with the unread count only', () => {
    const actions = buildQuickActions({ workspaceId: W, allowTitles: false, unread: 3, recent });
    expect(actions.map((a) => [a.id, a.title, a.subtitle ?? null, a.params.href])).toEqual([
      ['new-chat', 'New chat', null, '/new'],
      ['notifications', 'Notifications', '3 unread', '/notifications'],
    ]);
    expect(JSON.stringify(actions)).not.toContain('Zefir launch planning');
  });

  it('adds the two most recent chats by name when the policy allows titles', () => {
    const actions = buildQuickActions({ workspaceId: W, projectId: 7, allowTitles: true, unread: 0, recent });
    expect(actions.slice(2).map((a) => [a.title, a.params.href])).toEqual([
      ['Zefir launch planning', '/c/41'],
      ['Quarterly review', '/c/35a5538d-993d-4c66-bf5f-ad8509c581d4'],
    ]);
    expect(actions[1].subtitle).toBeNull();
    expect(actions.every((a) => a.params.workspace === W)).toBe(true);
    expect(actions.slice(2).every((a) => a.params.project === '7')).toBe(true);
  });

  it('offers no chat when the project the chats belong to is unknown', () => {
    const actions = buildQuickActions({ workspaceId: W, projectId: null, allowTitles: true, unread: 0, recent });
    expect(actions.map((a) => a.id)).toEqual(['new-chat', 'notifications']);
  });

  // QA-1: on Android a dynamic shortcut can be PINNED to the home screen, and
  // setDynamicShortcuts (all that setItems does there) never removes or
  // relabels a pinned one. A pinned conversation title would outlive the wipe,
  // a revocation and the policy turning titles off, so Android gets none.
  it('publishes no conversation title on Android, even when the policy allows titles', () => {
    jest.replaceProperty(Platform, 'OS', 'android');
    try {
      const actions = buildQuickActions({ workspaceId: W, allowTitles: true, unread: 2, recent });
      expect(actions.map((a) => a.id)).toEqual(['new-chat', 'notifications']);
      expect(actions.some((a) => a.id.startsWith('chat:'))).toBe(false);
    } finally {
      jest.restoreAllMocks();
    }
  });

  it('skips a chat with no name or an odd reference, and caps the count at 99+', () => {
    const actions = buildQuickActions({
      workspaceId: W,
      allowTitles: true,
      unread: 250,
      projectId: 7,
      recent: [{ ref: '../x', name: 'bad' }, { ref: '9', name: '   ' }, { ref: '10', name: 'Ok' }],
    });
    expect(actions.map((a) => a.title)).toEqual(['New chat', 'Notifications', 'Ok']);
    expect(actions[1].subtitle).toBe('99+ unread');
  });
});

describe('running an action', () => {
  it('opens its screen in the workspace it was built for, and nothing for another workspace', () => {
    const navigate = jest.fn();
    const [, notifications] = buildQuickActions({ workspaceId: W, allowTitles: false, unread: 0, recent: [] });
    expect(handleQuickAction(notifications, navigate)).toBe(true);
    expect(navigate).toHaveBeenCalledWith('/notifications');
    const foreign = buildQuickActions({ workspaceId: 'bbbbbbbbbbbb', allowTitles: false, unread: 0, recent: [] })[0];
    expect(handleQuickAction(foreign, navigate)).toBe(false);
    expect(actionBelongsTo({ id: 'x', title: 'x' }, W)).toBe(false);
    expect(navigate).toHaveBeenCalledTimes(1);
  });
});

// F2: conversation ids are per project (each project has its own tenant
// schema), so a chat action built for project 7 must not open /c/41 after the
// launch restored another project: that is a 404 or another conversation.
describe('a chat action and the selected project', () => {
  it('opens the chat only in the project it was built for', () => {
    const navigate = jest.fn();
    const chat = buildQuickActions({ workspaceId: W, projectId: 7, allowTitles: true, unread: 0, recent })[2];
    setSession({ projectId: 12 });
    expect(handleQuickAction(chat, navigate)).toBe(false);
    expect(navigate).not.toHaveBeenCalled();
    setSession({ projectId: 7 });
    expect(handleQuickAction(chat, navigate)).toBe(true);
    expect(navigate).toHaveBeenCalledWith('/c/41');
  });

  it('a chat action that names no project (an older build) opens nothing', () => {
    const navigate = jest.fn();
    const legacy = { id: 'chat:41', title: 'Old', params: { href: '/c/41', workspace: W } };
    expect(handleQuickAction(legacy, navigate)).toBe(false);
    expect(navigate).not.toHaveBeenCalled();
  });

  it('the fixed actions are not tied to a project', () => {
    const navigate = jest.fn();
    const [newChat] = buildQuickActions({ workspaceId: W, projectId: 7, allowTitles: false, unread: 0, recent: [] });
    setSession({ projectId: 12 });
    expect(handleQuickAction(newChat, navigate)).toBe(true);
  });
});

describe('applying the items', () => {
  it('sets them once per change and clears them', async () => {
    const actions = buildQuickActions({ workspaceId: W, allowTitles: false, unread: 1, recent: [] });
    await applyQuickActions(W, actions);
    await applyQuickActions(W, actions);
    expect(setItems).toHaveBeenCalledTimes(1);
    await clearQuickActions();
    expect(setItems).toHaveBeenLastCalledWith([]);
  });

  it('clears them when the workspace is wiped or switched away (a registered wipe hook)', async () => {
    await applyQuickActions(W, buildQuickActions({ workspaceId: W, allowTitles: false, unread: 5, recent: [] }));
    setItems.mockClear();
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    await suspendActiveWorkspace();
    expect(setItems).toHaveBeenCalledWith([]);
  });

  // QA-SEC-1: a kill during the capped server revoke must not leave a wiped
  // workspace's chat titles on the home screen.
  it('clears them before the server revoke of a log out', async () => {
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    bindWorkspace(W);
    await applyQuickActions(W, buildQuickActions({ workspaceId: W, projectId: 7, allowTitles: true, unread: 0, recent }));
    setItems.mockClear();
    let clearedBeforeRevoke = false;
    const unregister = registerRevokeHook(async () => {
      clearedBeforeRevoke = setItems.mock.calls.some((call) => JSON.stringify(call[0]) === '[]');
    });
    try {
      await endSession({ reason: 'user_logout' });
    } finally {
      unregister();
    }
    expect(clearedBeforeRevoke).toBe(true);
  });

  // #42: a later wipe hook (live turns cleared, the store closed, the query
  // cache cleared) re-renders the signed-in layout while the session still
  // names the old workspace; its effect must not put the titles back.
  it('items republished by a re-render while the session ends do not outlive the wipe', async () => {
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    const titled = (unread: number) => buildQuickActions({ workspaceId: W, projectId: 7, allowTitles: true, unread, recent });
    await applyQuickActions(W, titled(0));
    const off = registerWipeHook('zz_rerender', () => applyQuickActions(W, titled(7)));
    try {
      await endSession({ reason: 'user_logout' });
      expect(setItems).toHaveBeenLastCalledWith([]);

      bindWorkspace(W);
      setSession({ serverOrigin: 'https://ai.example.com', workspaceId: W, projectId: 7 });
      await applyQuickActions(W, titled(3));
      await suspendActiveWorkspace();
      expect(setItems).toHaveBeenLastCalledWith([]);
    } finally {
      off();
    }
  });

  it('sets nothing for a workspace that is not the bound one', async () => {
    bindWorkspace('bbbbbbbbbbbb');
    await applyQuickActions(W, buildQuickActions({ workspaceId: W, allowTitles: false, unread: 9, recent: [] }));
    expect(setItems).not.toHaveBeenCalled();
    bindWorkspace(null);
  });

  it('sets nothing when an item was built for another workspace than the one named', async () => {
    await applyQuickActions(W, buildQuickActions({ workspaceId: 'bbbbbbbbbbbb', allowTitles: false, unread: 9, recent: [] }));
    expect(setItems).not.toHaveBeenCalled();
  });
});
