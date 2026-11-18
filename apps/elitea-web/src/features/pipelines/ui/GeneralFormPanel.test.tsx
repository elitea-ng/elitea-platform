import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { ViewMode } from '@/shared/lib/enums';

import { GeneralFormPanel } from './GeneralFormPanel';

/** jsdom's default `window.innerWidth` (1024) is below `MIN_LARGE_WINDOW_WIDTH` (1200), so `useIsSmallWindow` reports `true` by default — widen the window so the collapse toggle (`!isSmallWindow`-gated) actually renders. */
beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1280 });
});

afterEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1024 });
});

describe('GeneralFormPanel', () => {
  it('renders the configuration-form slot with applicationId/viewMode', () => {
    const renderConfigurationForm = vi.fn(() => <div data-testid="form-slot" />);
    renderWithTheme(
      <GeneralFormPanel
        applicationId="42"
        onCollapsed={vi.fn()}
        viewMode={ViewMode.Owner}
        renderConfigurationForm={renderConfigurationForm}
      />,
    );

    expect(screen.getByTestId('form-slot')).toBeInTheDocument();
    expect(renderConfigurationForm).toHaveBeenCalledWith({ applicationId: '42', viewMode: 'owner' });
  });

  it('scrolls the settings inside the panel instead of the page row', () => {
    renderWithTheme(<GeneralFormPanel applicationId="42" onCollapsed={vi.fn()} renderConfigurationForm={() => <div />} />);
    expect(getComputedStyle(screen.getByTestId('pipeline-config-tab')).overflowY).toBe('auto');
  });

  it('collapsing hides the slot content and calls onCollapsed(true)', async () => {
    const user = userEvent.setup();
    const onCollapsed = vi.fn();
    renderWithTheme(
      <GeneralFormPanel
        applicationId="42"
        onCollapsed={onCollapsed}
        renderConfigurationForm={() => <div data-testid="form-slot" />}
      />,
    );

    expect(screen.getByTestId('form-slot')).toBeInTheDocument();
    await user.click(screen.getByRole('button'));

    expect(onCollapsed).toHaveBeenCalledWith(true);
    expect(screen.queryByTestId('form-slot')).not.toBeInTheDocument();
  });

  // #135: this button was icon-only with no aria-label/title/text, which axe
  // reports as a critical `button-name` (WCAG 4.1.2) failure.
  it('names the collapse toggle for a screen reader, and the name follows the state', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <GeneralFormPanel
        applicationId="42"
        onCollapsed={vi.fn()}
        renderConfigurationForm={() => <div data-testid="form-slot" />}
      />,
    );

    const button = screen.getByRole('button', { name: 'Collapse the configuration panel' });
    await user.click(button);

    expect(screen.getByRole('button', { name: 'Expand the configuration panel' })).toBeInTheDocument();
  });

  it('clicking the collapse toggle a second time expands again', async () => {
    const user = userEvent.setup();
    const onCollapsed = vi.fn();
    renderWithTheme(
      <GeneralFormPanel
        applicationId="42"
        onCollapsed={onCollapsed}
        renderConfigurationForm={() => <div data-testid="form-slot" />}
      />,
    );

    const button = screen.getByRole('button');
    await user.click(button);
    await user.click(button);

    expect(onCollapsed).toHaveBeenNthCalledWith(1, true);
    expect(onCollapsed).toHaveBeenNthCalledWith(2, false);
    expect(screen.getByTestId('form-slot')).toBeInTheDocument();
  });
});
