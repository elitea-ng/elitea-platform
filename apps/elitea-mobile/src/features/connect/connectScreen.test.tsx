import { render, screen } from '@testing-library/react-native';

import ConnectScreen from '@/app/connect';
import { __resetSessionForTests, resetSession } from '@/state/session';

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({}) }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);

beforeEach(() => __resetSessionForTests());

// DEF-4 (Agent Zefir E2E): a remote revoke wiped the app and landed on the
// connect screen with no word of why. Every end of a session clears the
// origin, so the connect screen is where the reason has to be said.
describe('connect screen after a session ended', () => {
  it('says the workspace signed this device out after a remote revoke', async () => {
    resetSession({ endReason: 'device_revoked', lastOriginHint: null });
    await render(<ConnectScreen />);
    expect(screen.getByRole('alert')).toHaveTextContent(/signed out by the workspace/i);
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
