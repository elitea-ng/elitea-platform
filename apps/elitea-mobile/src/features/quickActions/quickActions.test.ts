import * as QuickActions from 'expo-quick-actions';
import { Platform } from 'react-native';

import { actionBelongsTo, applyQuickActions, buildQuickActions, clearQuickActions } from '@/features/quickActions/quickActions';
import { handleQuickAction } from '@/features/quickActions/useQuickActions';
import { suspendActiveWorkspace } from '@/security/endSession';
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
    const actions = buildQuickActions({ workspaceId: W, allowTitles: true, unread: 0, recent });
    expect(actions.slice(2).map((a) => [a.title, a.params.href])).toEqual([
      ['Zefir launch planning', '/c/41'],
      ['Quarterly review', '/c/35a5538d-993d-4c66-bf5f-ad8509c581d4'],
    ]);
    expect(actions[1].subtitle).toBeNull();
    expect(actions.every((a) => a.params.workspace === W)).toBe(true);
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

describe('applying the items', () => {
  it('sets them once per change and clears them', async () => {
    const actions = buildQuickActions({ workspaceId: W, allowTitles: false, unread: 1, recent: [] });
    await applyQuickActions(actions);
    await applyQuickActions(actions);
    expect(setItems).toHaveBeenCalledTimes(1);
    await clearQuickActions();
    expect(setItems).toHaveBeenLastCalledWith([]);
  });

  it('clears them when the workspace is wiped or switched away (a registered wipe hook)', async () => {
    await applyQuickActions(buildQuickActions({ workspaceId: W, allowTitles: false, unread: 5, recent: [] }));
    setItems.mockClear();
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    await suspendActiveWorkspace();
    expect(setItems).toHaveBeenCalledWith([]);
  });
});
