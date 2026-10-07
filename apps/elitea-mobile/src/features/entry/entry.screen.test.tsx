import { fireEvent, render, screen } from '@testing-library/react-native';
import { Text } from 'react-native';

import { SignInView, type NativeSignInState } from '@/features/entry/SignInView';
import { LockView, unlockAction } from '@/security/lock/LockGate';
import type { LockState } from '@/security/lock/lockController';

jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
jest.mock('@/security/lock/useLockController', () => ({ lockController: { unlock: jest.fn(), bypassForDevelopment: jest.fn() } }));
jest.mock('@/state/workspace', () => ({ removeAllWorkspaces: jest.fn() }));

const ORIGIN = 'https://acme.elitea.ai';

function signIn(native: NativeSignInState, developer: boolean = false) {
  return (
    <SignInView
      host="acme.elitea.ai"
      origin={ORIGIN}
      name="Acme Corp AI"
      logo={null}
      brand={null}
      native={native}
      developer={developer ? <Text>token form</Text> : null}
      onUseDifferentWorkspace={jest.fn()}
    />
  );
}

describe('sign-in (docs/screens.md §1c)', () => {
  it('names the workspace, keeps its host prominent, and has one primary action', async () => {
    const onSignIn = jest.fn();
    await render(signIn({ kind: 'ready', busy: false, error: null, onSignIn }));
    expect(screen.getByRole('header', { name: 'Sign in to Acme Corp AI' })).toBeTruthy();
    expect(screen.getByLabelText('acme.elitea.ai')).toBeTruthy();
    await fireEvent.press(screen.getByTestId('native-sign-in'));
    expect(onSignIn).toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Use a different workspace' })).toBeTruthy();
  });

  it('keeps developer sign-in off the default path, behind "Other sign-in options"', async () => {
    await render(signIn({ kind: 'ready', busy: false, error: null, onSignIn: jest.fn() }, true));
    expect(screen.queryByText('token form')).toBeNull();
    await fireEvent.press(screen.getByTestId('other-sign-in'));
    expect(screen.getByText('token form')).toBeTruthy();
  });

  it('reads busy while signing in, and announces an error', async () => {
    await render(signIn({ kind: 'ready', busy: true, error: 'Sign-in was declined.', onSignIn: jest.fn() }));
    expect(screen.getByTestId('native-sign-in').props.accessibilityState).toMatchObject({ busy: true });
    expect(screen.getByRole('alert')).toHaveTextContent('Sign-in was declined.');
  });
});

const LOCKED: LockState = { status: 'locked', authInFlight: false, lastFailure: null, bypassed: false };

describe('lock screen (docs/screens.md §9; audit 2.5)', () => {
  it('names the unlock for the device biometric on each platform', () => {
    expect(unlockAction('face', 'ios')).toEqual({ title: 'Unlock with Face ID', icon: 'faceId' });
    expect(unlockAction('fingerprint', 'ios').title).toBe('Unlock with Touch ID');
    expect(unlockAction('fingerprint', 'android').title).toBe('Unlock with fingerprint');
    expect(unlockAction('face', 'android').title).toBe('Unlock with face');
    expect(unlockAction(null, 'ios').title).toBe('Unlock with passcode');
  });

  it('shows the workspace host, one unlock button, and the wipe only as a small confirm-first button', async () => {
    const onUnlock = jest.fn();
    const onReset = jest.fn();
    await render(
      <LockView state={LOCKED} workspace={{ origin: ORIGIN, name: null, logoMark: null, brand: null }} biometry="face" onUnlock={onUnlock} onReset={onReset} />,
    );
    expect(screen.getByRole('header', { name: 'Agent Zefir is locked' })).toBeTruthy();
    expect(screen.getByLabelText('Workspace acme.elitea.ai')).toBeTruthy();
    await fireEvent.press(screen.getByTestId('unlock'));
    expect(onUnlock).toHaveBeenCalled();
    const reset = screen.getByRole('button', { name: "Can't unlock?" });
    expect(reset.props.accessibilityHint).toMatch(/after you confirm/);
    await fireEvent.press(reset);
    expect(onReset).toHaveBeenCalled();
  });

  it('says why the last attempt failed', async () => {
    await render(<LockView state={{ ...LOCKED, lastFailure: 'lockout' }} workspace={null} biometry={null} onUnlock={jest.fn()} onReset={jest.fn()} />);
    expect(screen.getByRole('alert')).toHaveTextContent(/Too many attempts/);
  });

  it('draws nothing but the ground while the lock is still deciding', async () => {
    await render(<LockView state={{ ...LOCKED, status: 'deciding' }} workspace={null} biometry={null} onUnlock={jest.fn()} onReset={jest.fn()} />);
    expect(screen.queryByRole('button')).toBeNull();
  });
});
