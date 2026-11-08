import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createFakeComputerIpc } from '@/shared/desktop/computerIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import ComputerUsePage from './ComputerUsePage';

const toggle = (): HTMLInputElement => screen.getByRole('switch', { name: 'Let the agent use this computer' });

describe('Settings › Computer use', () => {
  it('is off by default and turns on with Accessibility; without Screen Recording it names the tools not offered', async () => {
    const ipc = createFakeComputerIpc();
    const user = userEvent.setup();
    renderWithTheme(<ComputerUsePage ipc={ipc} />);

    expect(await screen.findByRole('heading', { name: 'Computer use' })).toBeInTheDocument();
    await waitFor(() => expect(toggle()).not.toBeChecked());
    expect(screen.getByTestId('computer-grant-accessibility')).toHaveAttribute('data-grant', 'granted');
    expect(screen.queryByTestId('computer-open-accessibility')).toBeNull();
    expect(screen.getByTestId('computer-grant-screen-recording')).toHaveTextContent('Screen Recording: Not granted');
    expect(screen.getByTestId('computer-grant-screen-recording')).toHaveTextContent('computer_screenshot, computer_click_at');
    expect(screen.getByTestId('computer-stop-shortcut')).toHaveTextContent('⌃⌥⎋');

    await user.click(screen.getByTestId('computer-open-screen-recording'));
    await user.click(screen.getByTestId('computer-open-input-monitoring'));
    expect(ipc.calls.opened).toEqual(['screen_recording', 'input_monitoring']);

    await user.click(toggle());
    await waitFor(() => expect(toggle()).toBeChecked());
    await user.click(toggle());
    await waitFor(() => expect(toggle()).not.toBeChecked());
  });

  it('cannot be turned on without Accessibility, links to its pane, and reads the grant again on focus', async () => {
    const ipc = createFakeComputerIpc({ accessibility: 'denied' });
    const user = userEvent.setup();
    renderWithTheme(<ComputerUsePage ipc={ipc} />);

    expect(await screen.findByTestId('computer-needs-accessibility')).toHaveTextContent('Grant Accessibility first.');
    expect(toggle()).toBeDisabled();
    await user.click(screen.getByTestId('computer-open-accessibility'));
    expect(ipc.calls.opened).toEqual(['accessibility']);

    ipc.grant({ accessibility: 'granted' });
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    await waitFor(() => expect(toggle()).toBeEnabled());
    expect(screen.queryByTestId('computer-needs-accessibility')).toBeNull();
  });

  it('says when the organisation does not allow it', async () => {
    renderWithTheme(<ComputerUsePage ipc={createFakeComputerIpc({ policy_allows: false })} />);
    expect(await screen.findByTestId('computer-policy-off')).toHaveTextContent('Native client policy');
    expect(toggle()).toBeDisabled();
  });

  it('shows a refusal from the host', async () => {
    const ipc = createFakeComputerIpc({ accessibility: 'granted' });
    const user = userEvent.setup();
    renderWithTheme(<ComputerUsePage ipc={ipc} />);
    await screen.findByRole('switch', { name: 'Let the agent use this computer' });
    // The grant was withdrawn after the page read it: the host refuses.
    ipc.grant({ accessibility: 'denied' });
    await user.click(toggle());
    expect(await screen.findByTestId('computer-error')).toHaveTextContent('Grant Accessibility to Elitea first.');
  });

  it('lists the always-allowed apps and revokes one', async () => {
    const ipc = createFakeComputerIpc({
      remembered: [
        { bundle_id: 'com.apple.textedit', sees: true, acts: true },
        { bundle_id: 'com.apple.notes', sees: true, acts: false },
        { bundle_id: 'com.apple.calculator', sees: false, acts: true },
      ],
    });
    const user = userEvent.setup();
    renderWithTheme(<ComputerUsePage ipc={ipc} />);

    const rows = await screen.findAllByTestId('computer-remembered-app');
    expect(rows.map((row) => [row.dataset.bundleId, row.textContent])).toEqual([
      ['com.apple.textedit', expect.stringContaining('Always see and use')],
      ['com.apple.notes', expect.stringMatching(/Always see(?! and)/)],
      ['com.apple.calculator', expect.stringContaining('Always use')],
    ]);

    await user.click(screen.getByRole('button', { name: 'Revoke com.apple.textedit' }));
    await waitFor(() => expect(screen.getAllByTestId('computer-remembered-app')).toHaveLength(2));
    expect(ipc.calls.forgotten).toEqual(['com.apple.textedit']);
  });

  it('says so when no app is always allowed, and when not on macOS', async () => {
    renderWithTheme(<ComputerUsePage ipc={createFakeComputerIpc({ supported: false })} />);
    expect(await screen.findByTestId('computer-remembered-empty')).toBeInTheDocument();
    expect(screen.getByText('Computer use runs on macOS, Windows and Linux only.')).toBeInTheDocument();
    expect(screen.queryByTestId('computer-grant-accessibility')).toBeNull();
  });

  it('says it runs in the desktop app outside the host', () => {
    renderWithTheme(<ComputerUsePage />);
    expect(screen.getByText('Computer use runs in the desktop app.')).toBeInTheDocument();
  });
});
