import { act, fireEvent, render, screen } from '@testing-library/react-native';
import { Alert } from 'react-native';

import ConnectScreen from '@/app/connect';
import WorkspacesScreen from '@/app/(app)/workspaces';
import { __resetSessionForTests, resetSession, setSession } from '@/state/session';
import { __resetWorkspaceListForTests } from '@/state/workspaceList';

jest.mock('expo-router', () => ({
  useLocalSearchParams: () => ({}),
  router: { back: jest.fn(), canGoBack: () => true, push: jest.fn() },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('@/state/workspace', () => ({
  switchToWorkspace: jest.fn(async () => true),
  returnToPreviousWorkspace: jest.fn(async () => true),
  beginAddWorkspace: jest.fn(async () => {}),
  removeWorkspace: jest.fn(async () => true),
}));
// The list as the switcher store holds it (refresh is a no-op here).
jest.mock('@/state/workspaceList', () => {
  const actual = jest.requireActual('@/state/workspaceList');
  let list: unknown[] = [];
  return {
    ...actual,
    refreshWorkspaceList: jest.fn(async () => {}),
    useWorkspaceList: () => list,
    getWorkspaceList: () => list,
    __setList: (next: unknown[]) => {
      list = next;
    },
  };
});

const workspace = jest.requireMock<{
  switchToWorkspace: jest.Mock;
  returnToPreviousWorkspace: jest.Mock;
  beginAddWorkspace: jest.Mock;
  removeWorkspace: jest.Mock;
}>('@/state/workspace');
const listModule = jest.requireMock<{ __setList: (next: unknown[]) => void }>('@/state/workspaceList');

const A = { id: 'aaaaaaaaaaaa', origin: 'https://a.example.com', name: null, logoMark: null, signedIn: true };
const B = { id: 'bbbbbbbbbbbb', origin: 'https://b.example.com', name: 'Acme AI', logoMark: null, signedIn: false };

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceListForTests();
  listModule.__setList([]);
  jest.clearAllMocks();
});

// DEF-4 (mobile E2E): a remote revoke wiped the app and landed on the
// connect screen with no word of why. Every end of a session removes that
// workspace, so the connect screen is where the reason has to be said.
describe('connect screen after a session ended', () => {
  it('says which workspace signed this device out after a remote revoke', async () => {
    resetSession({ endReason: 'device_revoked', lastOriginHint: null, endedOrigin: 'https://ai.example.com' });
    await render(<ConnectScreen />);
    expect(screen.getByRole('alert')).toHaveTextContent(/signed out by the workspace ai\.example\.com/i);
  });

  it('says the sign-in was no longer accepted and keeps the address', async () => {
    resetSession({ endReason: 'credential_rejected', lastOriginHint: 'https://ai.example.com' });
    await render(<ConnectScreen />);
    expect(screen.getByRole('alert')).toHaveTextContent(/no longer accepts your sign-in/i);
    expect(screen.getByLabelText('Workspace address').props.value).toBe('https://ai.example.com');
  });

  it('shows no notice after the user logged out or on a fresh install', async () => {
    resetSession({ endReason: 'user_logout', lastOriginHint: null });
    const { unmount } = await render(<ConnectScreen />);
    expect(screen.queryByRole('alert')).toBeNull();
    await unmount();
    __resetSessionForTests();
    await render(<ConnectScreen />);
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('connect screen with other workspaces connected', () => {
  it('lists them with their hosts, and opening one switches without a wipe', async () => {
    listModule.__setList([A, B]);
    await render(<ConnectScreen />);
    expect(screen.getByText('Add a workspace')).toBeTruthy();
    expect(screen.getByText('a.example.com')).toBeTruthy();
    expect(screen.getByText('b.example.com')).toBeTruthy();
    expect(screen.getByText('Acme AI')).toBeTruthy();
    fireEvent.press(screen.getByTestId('workspace-bbbbbbbbbbbb'));
    expect(workspace.switchToWorkspace).toHaveBeenCalledWith('bbbbbbbbbbbb');
  });

  it('offers Back to the workspace that was active before "Add workspace"', async () => {
    listModule.__setList([A, B]);
    setSession({ returnWorkspaceId: A.id });
    await render(<ConnectScreen />);
    fireEvent.press(screen.getByTestId('connect-back'));
    expect(screen.getByTestId('connect-back')).toHaveTextContent(/Back to a\.example\.com/);
    expect(workspace.returnToPreviousWorkspace).toHaveBeenCalled();
  });
});

describe('workspace switcher', () => {
  it('shows every workspace with its host, marks the active one, and switches', async () => {
    listModule.__setList([A, B]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    await render(<WorkspacesScreen />);
    expect(screen.getByLabelText(/a\.example\.com\. Active/)).toBeTruthy();
    expect(screen.getByLabelText(/Acme AI, b\.example\.com\. Signed out/)).toBeTruthy();
    fireEvent.press(screen.getByTestId('workspace-bbbbbbbbbbbb'));
    expect(workspace.switchToWorkspace).toHaveBeenCalledWith('bbbbbbbbbbbb');
  });

  it('asks before removing a workspace, then removes only that one', async () => {
    listModule.__setList([A, B]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
    await render(<WorkspacesScreen />);
    // No always-visible destructive button on the rows (EH-7): Remove is the swipe and the row's action.
    expect(screen.queryByRole('button', { name: /^Remove/ })).toBeNull();
    const row = screen.getByTestId('workspace-bbbbbbbbbbbb');
    expect(row.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Remove' }]);
    await fireEvent(row, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    expect(workspace.removeWorkspace).not.toHaveBeenCalled();
    const [title, body, buttons] = alert.mock.calls[0];
    expect(title).toBe('Remove b.example.com?');
    expect(body).toMatch(/other workspaces are not affected/);
    buttons!.find((b) => b.style === 'destructive')!.onPress!();
    expect(workspace.removeWorkspace).toHaveBeenCalledWith('bbbbbbbbbbbb');
  });

  it('keeps the row when the removal is cancelled', async () => {
    listModule.__setList([A, B]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
    await render(<WorkspacesScreen />);
    await fireEvent(screen.getByTestId('workspace-bbbbbbbbbbbb'), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    const buttons = alert.mock.calls[0][2]!;
    await act(async () => {
      buttons.find((b) => b.style === 'cancel')!.onPress!();
    });
    expect(workspace.removeWorkspace).not.toHaveBeenCalled();
    expect(screen.getByTestId('workspace-bbbbbbbbbbbb').props.accessibilityState.busy).toBeFalsy();
  });

  it('adds a workspace through the connect flow', async () => {
    listModule.__setList([A]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    await render(<WorkspacesScreen />);
    fireEvent.press(screen.getByTestId('add-workspace'));
    expect(workspace.beginAddWorkspace).toHaveBeenCalled();
  });
});

describe('a managed server (MDM / EMM server_url, ADR-0052 W1b.3)', () => {
  const { refreshManagedConfig } = jest.requireActual<typeof import('@/security/managedConfig')>('@/security/managedConfig');
  afterEach(() => refreshManagedConfig(() => null));

  it('shows the managed address read only, "Managed by your organization"', async () => {
    refreshManagedConfig(() => ({ server_url: 'https://ai.corp.example.com' }));
    await render(<ConnectScreen />);
    const field = screen.getByLabelText('Workspace address');
    expect(field.props.value).toBe('https://ai.corp.example.com');
    expect(field.props.editable).toBe(false);
    expect(screen.getByTestId('connect-managed-hint')).toHaveTextContent('Managed by your organization.');
    // Typing does not change it.
    await fireEvent.changeText(field, 'https://other.example.com');
    expect(screen.getByLabelText('Workspace address').props.value).toBe('https://ai.corp.example.com');
  });

  it('offers no other server: not the last address, nor a connect link, nor another listed workspace', async () => {
    refreshManagedConfig(() => ({ server_url: 'ai.corp.example.com' }));
    listModule.__setList([A, { ...B, origin: 'https://ai.corp.example.com' }]);
    setSession({ lastOriginHint: 'https://last.example.com', pendingConnectOrigin: 'https://other.example.com' });
    await render(<ConnectScreen />);
    expect(screen.getByLabelText('Workspace address').props.value).toBe('https://ai.corp.example.com');
    expect(screen.queryByTestId(`workspace-${A.id}`)).toBeNull();
    expect(screen.getByTestId(`workspace-${B.id}`)).toBeTruthy();
    await act(async () => {});
    expect(screen.queryByTestId('connect-confirm-screen')).toBeNull();
  });

  it('ignores a server_url that is not a workspace address (the field stays the user\'s)', async () => {
    refreshManagedConfig(() => ({ server_url: 'http://ai.corp.example.com' }));
    await render(<ConnectScreen />);
    expect(screen.queryByTestId('connect-managed-hint')).toBeNull();
    expect(screen.getByTestId('connect-enter-address')).toBeTruthy();
  });

  it('without a managed server the address is the user\'s to type', async () => {
    setSession({ lastOriginHint: 'https://last.example.com' });
    await render(<ConnectScreen />);
    const field = screen.getByLabelText('Workspace address');
    expect(field.props.editable).toBe(true);
    await fireEvent.changeText(field, 'https://other.example.com');
    expect(screen.getByLabelText('Workspace address').props.value).toBe('https://other.example.com');
    expect(screen.queryByTestId('connect-managed-hint')).toBeNull();
  });

  it('with lock_server the switcher offers no "Add workspace"', async () => {
    refreshManagedConfig(() => ({ server_url: 'https://a.example.com', lock_server: true }));
    listModule.__setList([A]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    await render(<WorkspacesScreen />);
    expect(screen.queryByTestId('add-workspace')).toBeNull();
    expect(screen.getByTestId('add-workspace-managed')).toBeTruthy();
  });

  it('server_url alone keeps "Add workspace"', async () => {
    refreshManagedConfig(() => ({ server_url: 'https://a.example.com' }));
    listModule.__setList([A]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    await render(<WorkspacesScreen />);
    expect(screen.getByTestId('add-workspace')).toBeTruthy();
  });
});

describe('connect screen after the managed server lock', () => {
  it('says the organization allows only its own workspace', async () => {
    resetSession({ endReason: 'managed_server', lastOriginHint: null, endedOrigin: 'https://b.example.com' });
    await render(<ConnectScreen />);
    expect(screen.getByRole('alert')).toHaveTextContent(/allows only its own workspace\. b\.example\.com was signed out/i);
  });
});
