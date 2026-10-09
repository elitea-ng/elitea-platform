import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createFakeAppIpc } from '@/shared/desktop/appEvents.fake';
import type { DoctorCheck } from '@/shared/desktop/doctorIpc';
import { createFakeDoctorIpc } from '@/shared/desktop/doctorIpc.fake';
import type { HostBridge, HostState } from '@/shared/desktop/hostBridge';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { DesktopShell } from './DesktopShell';

const STATE: HostState = {
  configured: true,
  origin: 'https://h.example',
  displayName: 'Acme',
  signedIn: false,
  clientVersion: '0.1.0',
  clientId: 'desktop',
  policy: null,
};

/** The bridge, with the spies returned apart from it so no method is read off the object. */
function hostBridge() {
  let rejectSignIn: (error: unknown) => void = () => undefined;
  const cancelSignIn = vi.fn<HostBridge['cancelSignIn']>().mockImplementation(() => {
    rejectSignIn('sign-in was cancelled or timed out');
    return Promise.resolve();
  });
  const bridge: HostBridge = {
    state: vi.fn<HostBridge['state']>().mockResolvedValue(STATE),
    connect: vi.fn<HostBridge['connect']>().mockResolvedValue({ origin: 'https://h.example', displayName: 'Acme', deploymentKind: 'self_hosted' }),
    signIn: vi.fn<HostBridge['signIn']>().mockImplementation(
      () =>
        new Promise<HostState>((_resolve, reject) => {
          rejectSignIn = reject;
        }),
    ),
    cancelSignIn,
    accessToken: vi.fn(),
    refresh: vi.fn(),
    signOut: vi.fn(),
    wipe: vi.fn(),
    openExternal: vi.fn(),
  };
  return { bridge, cancelSignIn, rejectSignIn: (error: unknown) => rejectSignIn(error) };
}

describe('DesktopShell sign-in', () => {
  it('Cancel abandons the browser sign-in and returns to the sign-in button without an error', async () => {
    const { bridge, cancelSignIn } = hostBridge();
    const user = userEvent.setup();
    renderWithTheme(<DesktopShell bridge={bridge} />);

    await user.click(await screen.findByRole('button', { name: 'Continue' }));
    await user.click(await screen.findByRole('button', { name: 'Sign in with your browser' }));
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));

    expect(cancelSignIn).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole('button', { name: 'Sign in with your browser' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('a sign-in that fails on its own still shows the error', async () => {
    const { bridge, rejectSignIn } = hostBridge();
    const user = userEvent.setup();
    renderWithTheme(<DesktopShell bridge={bridge} />);

    await user.click(await screen.findByRole('button', { name: 'Continue' }));
    await user.click(await screen.findByRole('button', { name: 'Sign in with your browser' }));
    rejectSignIn('sign-in was denied');

    expect(await screen.findByRole('alert')).toHaveTextContent('sign-in was denied');
  });
});

describe('DesktopShell diagnostics', () => {
  const BROKEN: DoctorCheck = {
    id: 'credentials',
    title: 'Stored sign-in',
    status: 'fail',
    message: 'credentials.json is a symbolic link, so the app will not read it or sign in over it.',
    fix_id: 'credentials.move_aside',
    fix_label: 'Move it aside',
  };
  const OK: DoctorCheck = { id: 'workspaces', title: 'Workspaces', status: 'ok', message: '2 folder(s), all reachable.' };
  const repaired = (checks: DoctorCheck[]): DoctorCheck[] =>
    checks.map((c) => (c.id === 'credentials' ? { id: c.id, title: c.title, status: 'ok' as const, message: 'No sign-in is stored on this computer yet.' } : c));

  it('says at launch that something needs attention, and the Doctor repairs it on a click', async () => {
    const { bridge } = hostBridge();
    const doctor = createFakeDoctorIpc([BROKEN, OK], { 'credentials.move_aside': repaired });
    const user = userEvent.setup();
    renderWithTheme(<DesktopShell bridge={bridge} doctor={doctor} />);

    // The launch check is local only (no network before a deployment is chosen).
    expect(await screen.findByText('Something needs attention.')).toBeInTheDocument();
    expect(doctor.calls.runs).toEqual(['local']);
    await user.click(screen.getByRole('button', { name: 'Run Diagnostics' }));

    const dialog = await screen.findByRole('dialog', { name: 'Diagnostics' });
    expect(await within(dialog).findByTestId('doctor-check-credentials')).toHaveAttribute('data-status', 'fail');
    expect(doctor.calls.fixes).toEqual([]);
    await user.click(within(dialog).getByRole('button', { name: 'Fix: Stored sign-in' }));

    expect(await within(dialog).findByText('Fixed.')).toBeInTheDocument();
    expect(doctor.calls.fixes).toEqual(['credentials.move_aside']);
    await waitFor(() => expect(within(dialog).getByTestId('doctor-check-credentials')).toHaveAttribute('data-status', 'ok'));
  });

  it('shows no notice when nothing failed', async () => {
    const { bridge } = hostBridge();
    const doctor = createFakeDoctorIpc([OK]);
    renderWithTheme(<DesktopShell bridge={bridge} doctor={doctor} />);
    await waitFor(() => expect(doctor.calls.runs).toEqual(['local']));
    expect(screen.queryByText('Something needs attention.')).not.toBeInTheDocument();
  });

  it('opens from Help › Run Diagnostics… on the connect screen', async () => {
    const { bridge } = hostBridge();
    const doctor = createFakeDoctorIpc([OK]);
    const appIpc = createFakeAppIpc();
    renderWithTheme(<DesktopShell bridge={bridge} doctor={doctor} appIpc={appIpc} />);
    await screen.findByRole('button', { name: 'Continue' });
    await waitFor(() => expect(appIpc.subscriberCount()).toBe(1));
    act(() => appIpc.emit({ id: 'run_diagnostics' }));
    expect(await screen.findByRole('dialog', { name: 'Diagnostics' })).toBeInTheDocument();
  });

  it('offers the Doctor next to a sign-in the stored file blocked', async () => {
    const { bridge, rejectSignIn } = hostBridge();
    const doctor = createFakeDoctorIpc([BROKEN], { 'credentials.move_aside': repaired });
    const user = userEvent.setup();
    renderWithTheme(<DesktopShell bridge={bridge} doctor={doctor} />);
    await user.click(await screen.findByRole('button', { name: 'Continue' }));
    await user.click(await screen.findByRole('button', { name: 'Sign in with your browser' }));
    rejectSignIn('could not use the stored sign-in: credentials.json is a symbolic link; open Help › Run Diagnostics… to repair it');

    const alert = await screen.findByText(/symbolic link; open Help/);
    await user.click(within(alert.closest('[role="alert"]') as HTMLElement).getByRole('button', { name: 'Run Diagnostics' }));
    expect(await screen.findByRole('dialog', { name: 'Diagnostics' })).toBeInTheDocument();
  });
});
