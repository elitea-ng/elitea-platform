import { fireEvent, render, screen } from '@testing-library/react-native';
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

// DEF-4 (Agent Zefir E2E): a remote revoke wiped the app and landed on the
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
    fireEvent.press(screen.getByTestId('remove-bbbbbbbbbbbb'));
    expect(workspace.removeWorkspace).not.toHaveBeenCalled();
    const [title, body, buttons] = alert.mock.calls[0];
    expect(title).toBe('Remove b.example.com?');
    expect(body).toMatch(/other workspaces are not affected/);
    buttons!.find((b) => b.style === 'destructive')!.onPress!();
    expect(workspace.removeWorkspace).toHaveBeenCalledWith('bbbbbbbbbbbb');
  });

  it('adds a workspace through the connect flow', async () => {
    listModule.__setList([A]);
    setSession({ workspaceId: A.id, serverOrigin: A.origin });
    await render(<WorkspacesScreen />);
    fireEvent.press(screen.getByTestId('add-workspace'));
    expect(workspace.beginAddWorkspace).toHaveBeenCalled();
  });
});
