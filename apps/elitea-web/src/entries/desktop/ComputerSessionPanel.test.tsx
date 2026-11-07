import { act, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createFakeComputerIpc } from '@/shared/desktop/computerIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ComputerSessionPanel, panelText } from './ComputerSessionPanel';

const active = { phase: 'active' as const, paused_by: null, app: 'TextEdit', action: 'type 12 characters' };

describe('the computer session panel', () => {
  it('shows the app and the last action with Pause and Stop, and pauses on a click', async () => {
    const ipc = createFakeComputerIpc({}, active);
    const user = userEvent.setup();
    renderWithTheme(<ComputerSessionPanel ipc={ipc} />);

    expect(await screen.findByText('Using TextEdit')).toBeInTheDocument();
    expect(screen.getByTestId('computer-panel-detail')).toHaveTextContent('type 12 characters');
    expect(screen.getByTestId('computer-panel-stop')).toBeInTheDocument();
    expect(screen.queryByTestId('computer-panel-resume')).toBeNull();

    await user.click(screen.getByTestId('computer-panel-pause'));
    expect(await screen.findByText('Paused')).toBeInTheDocument();
    expect(screen.getByTestId('computer-panel')).toHaveAttribute('data-phase', 'paused');
    expect(screen.queryByTestId('computer-panel-pause')).toBeNull();
    expect(ipc.calls.actions).toEqual(['get', 'pause']);
  });

  it('says the person is using the computer when their input paused it, and only Resume lifts it', async () => {
    const ipc = createFakeComputerIpc({}, active);
    const user = userEvent.setup();
    renderWithTheme(<ComputerSessionPanel ipc={ipc} />);
    await screen.findByText('Using TextEdit');

    act(() => ipc.emit({ ...active, phase: 'paused', paused_by: 'user_active' }));
    expect(await screen.findByText('Paused, you are using the computer')).toBeInTheDocument();
    expect(screen.getByText('The agent waits until you press Resume.')).toBeInTheDocument();

    await user.click(screen.getByTestId('computer-panel-resume'));
    expect(await screen.findByText('Using TextEdit')).toBeInTheDocument();
    expect(ipc.calls.actions).toEqual(['get', 'resume']);
  });

  it('ends with Stop, and the shortcut ends it the same way', async () => {
    const ipc = createFakeComputerIpc({}, active);
    const user = userEvent.setup();
    renderWithTheme(<ComputerSessionPanel ipc={ipc} />);
    await screen.findByText('Using TextEdit');

    await user.click(screen.getByTestId('computer-panel-stop'));
    expect(await screen.findByText('Stopped')).toBeInTheDocument();
    expect(screen.queryByRole('button')).toBeNull();

    act(() => ipc.emit({ phase: 'active', paused_by: null, app: null, action: null }));
    expect(await screen.findByText('Using the computer')).toBeInTheDocument();
    act(() => ipc.emit({ phase: 'stopped', paused_by: null, app: null, action: null }));
    expect(await screen.findByText('Stopped')).toBeInTheDocument();
  });

  it('names every state', () => {
    expect(panelText({ phase: 'idle', paused_by: null, app: null, action: null }).title).toBe('Not using the computer');
    expect(panelText({ phase: 'paused', paused_by: 'button', app: null, action: null }).title).toBe('Paused');
    expect(panelText({ ...active, action: null }).detail).toBeNull();
  });
});
