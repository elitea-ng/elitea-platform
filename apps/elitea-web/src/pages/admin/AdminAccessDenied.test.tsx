import { act, cleanup, fireEvent, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BRAND_PACK_GLOBAL } from '@/shared/brand';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AdminAccessDenied } from './AdminAccessDenied';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('AdminAccessDenied', () => {
  it('renders the title, explanation, brand mark and the immediate-return link', () => {
    renderWithTheme(<AdminAccessDenied redirect={vi.fn()} />);

    expect(screen.getByRole('heading', { level: 1, name: 'Nice Try, Hacker!' })).toBeInTheDocument();
    expect(
      screen.getByText('Your account does not hold an administration permission for this console.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to the app now' })).toHaveAttribute('href', '/app/');
    expect(screen.getByTestId('brand-logo-mark')).toBeInTheDocument();
  });

  it('counts down in a polite live region and redirects to /app/ after 5 s', () => {
    const redirect = vi.fn();
    renderWithTheme(<AdminAccessDenied redirect={redirect} />);

    const status = screen.getByRole('status');
    expect(status).toHaveAttribute('aria-live', 'polite');
    expect(status).toHaveTextContent('Taking you back to the app in 5 s…');

    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(status).toHaveTextContent('in 4 s');
    expect(redirect).not.toHaveBeenCalled();

    // One tick per act: each second re-arms the next timer after a render.
    for (let i = 0; i < 4; i += 1) {
      act(() => {
        vi.advanceTimersByTime(1000);
      });
    }
    expect(redirect).toHaveBeenCalledExactlyOnceWith('/app/');
  });

  it('"Stay on this page" cancels the redirect for good (WCAG 2.2.1)', () => {
    const redirect = vi.fn();
    renderWithTheme(<AdminAccessDenied redirect={redirect} />);

    fireEvent.click(screen.getByRole('button', { name: 'Stay on this page' }));
    act(() => {
      vi.advanceTimersByTime(30_000);
    });

    expect(redirect).not.toHaveBeenCalled();
    expect(screen.getByRole('status')).toHaveTextContent('Automatic redirect cancelled.');
    expect(screen.queryByRole('button', { name: 'Stay on this page' })).toBeNull();
  });

  it('activating "Stay" while it has focus moves focus to the primary link; nothing navigates after 5 s (WCAG 2.4.3)', () => {
    const redirect = vi.fn();
    renderWithTheme(<AdminAccessDenied redirect={redirect} />);

    const stay = screen.getByRole('button', { name: 'Stay on this page' });
    stay.focus();
    expect(stay).toHaveFocus();
    // A native <button> turns Enter/Space into a click.
    fireEvent.click(stay);

    expect(screen.getByRole('link', { name: 'Back to the app now' })).toHaveFocus();
    expect(screen.getByRole('status')).toHaveTextContent('Automatic redirect cancelled.');
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    expect(redirect).not.toHaveBeenCalled();
  });

  it('resolves the brand pack once, not on every countdown tick', () => {
    (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL] = { id: 'broken' };
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    renderWithTheme(<AdminAccessDenied redirect={vi.fn()} />);
    const afterMount = warn.mock.calls.length;

    for (let i = 0; i < 3; i += 1) {
      act(() => {
        vi.advanceTimersByTime(1000);
      });
    }

    expect(screen.getByRole('status')).toHaveTextContent('in 2 s');
    expect(warn.mock.calls.length).toBe(afterMount);
    delete (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL];
    warn.mockRestore();
  });
});
