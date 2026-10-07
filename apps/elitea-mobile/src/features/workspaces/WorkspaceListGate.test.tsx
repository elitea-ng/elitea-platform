import { fireEvent, render, screen } from '@testing-library/react-native';
import { Text } from 'react-native';

import { WorkspaceListGate, listNeedsUnlock } from '@/features/workspaces/WorkspaceListGate';
import { rowMark } from '@/features/workspaces/WorkspaceRow';
import type { WorkspaceSummary } from '@/state/workspaceList';

const ws = (requiresLock: boolean): WorkspaceSummary => ({
  id: 'aaaaaaaaaaaa',
  origin: 'https://a.example.com',
  name: 'Acme AI',
  logoMark: null,
  signedIn: true,
  requiresLock,
});

// WS-SEC-4: with no workspace active the device lock is stopped.
describe('WorkspaceListGate', () => {
  it('hides the list behind the device lock when a listed workspace requires it', async () => {
    const authenticate = jest.fn(async () => ({ ok: true as const }));
    await render(
      <WorkspaceListGate workspaces={[ws(false), ws(true)]} auth={{ authenticate }}>
        <Text>a.example.com</Text>
      </WorkspaceListGate>,
    );
    expect(screen.queryByText('a.example.com')).toBeNull();
    await fireEvent.press(screen.getByTestId('unlock-workspace-list'));
    expect(authenticate).toHaveBeenCalledTimes(1);
    expect(screen.getByText('a.example.com')).toBeTruthy();
  });

  it('keeps it hidden when the unlock is cancelled', async () => {
    const authenticate = jest.fn(async () => ({ ok: false as const, reason: 'cancelled' as const }));
    await render(
      <WorkspaceListGate workspaces={[ws(true)]} auth={{ authenticate }}>
        <Text>a.example.com</Text>
      </WorkspaceListGate>,
    );
    await fireEvent.press(screen.getByTestId('unlock-workspace-list'));
    expect(screen.queryByText('a.example.com')).toBeNull();
  });

  it('shows the list straight away when no workspace requires the lock', async () => {
    await render(
      <WorkspaceListGate workspaces={[ws(false)]}>
        <Text>a.example.com</Text>
      </WorkspaceListGate>,
    );
    expect(screen.getByText('a.example.com')).toBeTruthy();
    expect(listNeedsUnlock([ws(false)])).toBe(false);
  });
});

describe('rowMark', () => {
  const remote = { uri: 'https://a.example.com/brand/mark.svg', kind: 'svg' as const };
  const local = { uri: 'data:image/png;base64,AAAA', kind: 'raster' as const };

  it('never fetches the mark of a workspace that is not active', () => {
    expect(rowMark(remote, false)).toBeNull();
    expect(rowMark(local, false)).toBe(local);
    expect(rowMark(remote, true)).toBe(remote);
  });
});
