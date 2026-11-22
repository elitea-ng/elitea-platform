import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithQueryAndTheme as render } from '../../../test/renderWithQueryAndTheme';
import type { UseMicrosoftLoginResult } from '../model/useMicrosoftLogin';
import { MicrosoftLoginPanel } from './MicrosoftLoginPanel';

const SESSION = {
  account: 'Ada Lovelace',
  tenant: 'tenant-1',
  expiresAt: new Date(Date.UTC(2026, 10, 20, 12, 0, 0)).toISOString(),
  app: 'custom',
  tokenKey: 'k',
};

function login(overrides: Partial<UseMicrosoftLoginResult> = {}): UseMicrosoftLoginResult {
  return {
    phase: 'idle',
    failure: undefined,
    session: undefined,
    busy: false,
    login: vi.fn(),
    cancel: vi.fn(),
    clear: vi.fn(),
    ...overrides,
  };
}

const props = { signedIn: false, needsLogin: false, saved: true, canLogin: true, onSignOut: vi.fn() };

describe('MicrosoftLoginPanel', () => {
  it('offers Sign in with Microsoft for a saved credential, and Save and sign in for a new one', () => {
    const start = vi.fn();
    const { rerender } = render(<MicrosoftLoginPanel {...props} login={login({ login: start })} />);
    fireEvent.click(screen.getByRole('button', { name: 'Sign in with Microsoft' }));
    expect(start).toHaveBeenCalledTimes(1);

    rerender(<MicrosoftLoginPanel {...props} saved={false} login={login({ login: start })} />);
    expect(screen.getByRole('button', { name: 'Save and sign in with Microsoft' })).toBeInTheDocument();
  });

  it('shows who is signed in, the tenant, who runs act as, and when the sign-in ends', () => {
    render(<MicrosoftLoginPanel {...props} signedIn login={login({ session: SESSION })} />);
    expect(screen.getByText('Signed in as Ada Lovelace')).toBeInTheDocument();
    expect(screen.getByText('Microsoft tenant: tenant-1')).toBeInTheDocument();
    expect(screen.getByText('Runs act as Ada Lovelace')).toBeInTheDocument();
    expect(screen.getByText(/The sign-in lasts until/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Sign in with Microsoft' })).not.toBeInTheDocument();
  });

  it('signs in again, and signs out', () => {
    const again = vi.fn();
    const out = vi.fn();
    render(<MicrosoftLoginPanel {...props} signedIn onSignOut={out} login={login({ session: SESSION, login: again })} />);
    fireEvent.click(screen.getByRole('button', { name: 'Sign in again' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(again).toHaveBeenCalledTimes(1);
    expect(out).toHaveBeenCalledTimes(1);
  });

  it('says the sign-in was refused and asks for a new one when the check refused the token', () => {
    render(<MicrosoftLoginPanel {...props} signedIn needsLogin login={login({ session: SESSION })} />);
    expect(screen.getByTestId('microsoft-login-expired')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in with Microsoft' })).toBeInTheDocument();
    expect(screen.queryByTestId('microsoft-login-summary')).not.toBeInTheDocument();
  });

  it('shows progress and a Cancel while a sign-in is running', () => {
    const cancel = vi.fn();
    render(<MicrosoftLoginPanel {...props} login={login({ phase: 'waiting', busy: true, cancel })} />);
    expect(screen.getByTestId('microsoft-login-progress')).toHaveTextContent('Waiting for you to sign in');
    expect(screen.getByTestId('microsoft-login-button')).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(cancel).toHaveBeenCalledTimes(1);
  });

  it('explains a failure in words, never a raw code', () => {
    render(<MicrosoftLoginPanel {...props} login={login({ phase: 'failed', failure: 'custom_apps_disabled' })} />);
    expect(screen.getByRole('alert')).toHaveTextContent('An administrator turned off Custom app sign-in');
    expect(screen.getByRole('alert')).not.toHaveTextContent('custom_apps_disabled');
  });

  it('falls back to a generic message for a code it does not know', () => {
    render(<MicrosoftLoginPanel {...props} login={login({ phase: 'failed', failure: 'who_knows' })} />);
    expect(screen.getByRole('alert')).toHaveTextContent('The Microsoft sign-in did not finish');
  });

  it('shows "Login managed by a project admin" and no button to a person who cannot update the credential', () => {
    render(<MicrosoftLoginPanel {...props} canLogin={false} login={login()} />);
    expect(screen.getByText('Login managed by a project admin')).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});
