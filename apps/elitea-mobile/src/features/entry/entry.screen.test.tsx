import { fireEvent, render, screen } from '@testing-library/react-native';
import { Text } from 'react-native';

import { ConnectConfirm } from '@/features/connect/ConnectConfirm';
import { ConnectWelcome } from '@/features/entry/ConnectWelcome';
import { EntryScreen } from '@/features/entry/EntryScreen';
import { SignInView, type NativeSignInState } from '@/features/entry/SignInView';
import { LockView, unlockAction } from '@/security/lock/LockGate';
import type { LockState } from '@/security/lock/lockController';

jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
let mockFontScale = 1;
jest.mock('react-native/Libraries/Utilities/useWindowDimensions', () => ({
  __esModule: true,
  default: () => ({ width: 375, height: 667, scale: 2, fontScale: mockFontScale }),
}));
afterEach(() => {
  mockFontScale = 1;
});
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

  it('scrolls when its content outgrows the screen, with the unlock and wipe inside the scroll view (EH-2)', async () => {
    await render(<LockView state={{ ...LOCKED, status: 'no_device_security' } as LockState} workspace={null} biometry={null} onUnlock={jest.fn()} onReset={jest.fn()} />);
    const scroll = screen.getByTestId('lock-scroll');
    expect(scroll.props.contentContainerStyle).toEqual(expect.arrayContaining([expect.objectContaining({ flexGrow: 1, justifyContent: 'space-between' })]));
    expect(screen.getByRole('button', { name: 'Log out & wipe' })).toBeTruthy();
    expect(screen.getByTestId('lock-scroll')).toContainElement(screen.getByRole('button', { name: 'Log out & wipe' }));
  });

  it('draws nothing but the ground while the lock is still deciding', async () => {
    await render(<LockView state={{ ...LOCKED, status: 'deciding' }} workspace={null} biometry={null} onUnlock={jest.fn()} onReset={jest.fn()} />);
    expect(screen.queryByRole('button')).toBeNull();
  });
});

describe('entry frame (docs/screens.md §1)', () => {
  it('keeps the footer in the scroll view by default and pins it under the content with pinFooter', async () => {
    await render(
      <EntryScreen footer={<Text>Footer</Text>}>
        <Text>Body</Text>
      </EntryScreen>,
    );
    expect(screen.queryByTestId('entry-pinned-footer')).toBeNull();
    expect(screen.getByTestId('entry-scroll')).toContainElement(screen.getByText('Footer'));
    await render(
      <EntryScreen pinFooter footer={<Text>Footer</Text>}>
        <Text>Body</Text>
      </EntryScreen>,
    );
    expect(screen.getByTestId('entry-pinned-footer')).toContainElement(screen.getByText('Footer'));
    expect(screen.getByTestId('entry-scroll')).not.toContainElement(screen.getByText('Footer'));
  });

  it('pins Connect on screen on the confirm screen at accessibility sizes (EH-5)', async () => {
    const check = { kind: 'failed', origin: 'https://acme.elitea.ai', error: 'not_supported' } as const;
    const ui = <ConnectConfirm check={check} devSignIn busy={false} onConfirm={jest.fn()} onBack={jest.fn()} onUseCanonical={jest.fn()} />;
    await render(ui);
    expect(screen.queryByTestId('entry-pinned-footer')).toBeNull();
    mockFontScale = 3;
    await render(ui);
    expect(screen.getByTestId('entry-pinned-footer')).toContainElement(screen.getByRole('button', { name: 'Use a different address' }));
  });

  it('puts "Back to {host}" at the top leading edge, outside the centred column (EH-6)', async () => {
    const onBack = jest.fn();
    await render(
      <ConnectWelcome
        adding
        backTo={{ host: 'zefir.elitea.ai', onPress: onBack }}
        address=""
        onAddressChange={jest.fn()}
        error={null}
        managedHint={false}
        busy={false}
        onContinue={jest.fn()}
        onPrivacy={jest.fn()}
        onHelp={jest.fn()}
      />,
    );
    const back = screen.getByTestId('connect-back');
    expect(screen.getByTestId('entry-back-bar')).toContainElement(back);
    expect(screen.getByTestId('entry-scroll')).not.toContainElement(back);
    await fireEvent.press(back);
    expect(onBack).toHaveBeenCalled();
  });
});
