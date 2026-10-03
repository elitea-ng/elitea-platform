import { act, cleanup, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../../shared/ui/lib/testTheme';
import { NotFoundPage } from '../__404';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('NotFoundPage', () => {
  it('renders the 404 hierarchy: title, helpful text, product name', () => {
    renderWithTheme(<NotFoundPage />);

    expect(screen.getByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument();
    expect(screen.getByText('404')).toBeInTheDocument();
    expect(screen.getByText(/does not exist or may have been moved/)).toBeInTheDocument();
    expect(screen.getByTestId('brand-logo-mark')).toBeInTheDocument();
  });

  it('offers a primary home button pointing at the app home', () => {
    renderWithTheme(<NotFoundPage />);
    expect(screen.getByRole('link', { name: 'Go to the home page' })).toHaveAttribute('href', '/');
  });

  it('"Go back" calls history.back when the tab has history', async () => {
    vi.spyOn(window.history, 'length', 'get').mockReturnValue(3);
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    renderWithTheme(<NotFoundPage />);

    await userEvent.click(screen.getByRole('button', { name: 'Go back' }));
    expect(back).toHaveBeenCalledOnce();
  });

  it('hides "Go back" when there is no history to return to', () => {
    vi.spyOn(window.history, 'length', 'get').mockReturnValue(1);
    renderWithTheme(<NotFoundPage />);
    expect(screen.queryByRole('button', { name: 'Go back' })).toBeNull();
  });

  it('never redirects on a timer', () => {
    vi.useFakeTimers();
    renderWithTheme(<NotFoundPage />);

    // No timer is ever armed, so nothing can navigate away.
    expect(vi.getTimerCount()).toBe(0);
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(screen.getByRole('heading', { name: 'Page not found' })).toBeInTheDocument();
    expect(screen.queryByRole('status')).toBeNull();
  });
});
