import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import ResourceVersionInfo from './ResourceVersionInfo';

describe('ResourceVersionInfo', () => {
  it('renders only the "Help Center" title when no versionLabel is given (default props)', () => {
    renderWithTheme(<ResourceVersionInfo />);

    expect(screen.getByText('Help Center')).toBeInTheDocument();
    expect(screen.queryByTestId('copy-version-info')).not.toBeInTheDocument();
    expect(screen.queryByTestId('resource-version-info-icon')).not.toBeInTheDocument();
  });

  it('shows the version label and a copy-to-clipboard control once versionLabel is passed (finding #2)', () => {
    renderWithTheme(<ResourceVersionInfo versionLabel="Version: 2.3.0 (2026-01-15)" />);

    // Appears twice: once as the header's own label, once as the copy
    // button's visible text (CopyToClipboardButton shows `value` as its
    // clickable content) — both are asserted to make sure neither is lost.
    expect(screen.getAllByText('Version: 2.3.0 (2026-01-15)')).toHaveLength(2);
    expect(screen.getByTestId('copy-version-info')).toBeInTheDocument();
  });

  it("lists every component's name and version in the info-icon tooltip (finding #2)", async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ResourceVersionInfo
        versionLabel="Version: 2.3.0"
        components={[
          { name: 'elitea-main', version: '1.4.2' },
          { name: 'migrations' },
        ]}
      />,
    );

    await user.hover(screen.getByTestId('resource-version-info-icon'));

    expect(await screen.findByText('elitea-main: 1.4.2')).toBeInTheDocument();
    // A component with no reported version falls back to the em-dash placeholder.
    expect(await screen.findByText('migrations: —')).toBeInTheDocument();
  });

  it('shows no tooltip content when there are no components to list', async () => {
    const user = userEvent.setup();
    renderWithTheme(<ResourceVersionInfo versionLabel="Version: 2.3.0" />);

    await user.hover(screen.getByTestId('resource-version-info-icon'));
    // Give any (unwanted) tooltip content a chance to mount before asserting its absence.
    await new Promise(resolve => setTimeout(resolve, 50));
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
  });

  it('copies the version label to the clipboard when the copy control is clicked', async () => {
    const user = userEvent.setup();
    renderWithTheme(<ResourceVersionInfo versionLabel="Version: 2.3.0" />);

    await user.click(screen.getByTestId('copy-version-info'));
    await vi.waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe('Version: 2.3.0');
    });
  });
});
