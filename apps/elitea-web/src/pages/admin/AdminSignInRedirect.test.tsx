import { cleanup, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AdminAccessUnavailable } from './AdminAccessUnavailable';
import { AdminSignInRedirect, signInUrl } from './AdminSignInRedirect';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  window.history.replaceState(null, '', '/');
});

describe('AdminSignInRedirect', () => {
  it('sends the browser to /auth/login with the deep link as target_to, immediately', () => {
    window.history.replaceState(null, '', '/admin/app/users?page=2');
    const redirect = vi.fn();
    renderWithTheme(<AdminSignInRedirect redirect={redirect} />);

    expect(redirect).toHaveBeenCalledOnce();
    const url = new URL(String(redirect.mock.calls[0]?.[0]), 'https://host.example');
    expect(url.pathname).toBe('/auth/login');
    const target = url.searchParams.get('target_to') ?? '';
    expect(target.startsWith('/admin/app/users?')).toBe(true);
    expect(new URLSearchParams(target.split('?')[1]).get('page')).toBe('2');
    expect(screen.queryByText('Nice Try, Hacker!')).toBeNull();
  });

  it('only ever builds a same-origin relative target (no open redirect)', () => {
    const url = signInUrl({ pathname: '//evil.example/x', search: '' });
    const target = new URL(url, 'https://host.example').searchParams.get('target_to') ?? '';
    expect(target.startsWith('/')).toBe(true);
    expect(target.startsWith('//')).toBe(false);
    expect(url.startsWith('/auth/login?')).toBe(true);
  });

  it('does not redirect twice: a page that already retried offers a manual sign-in link', () => {
    window.history.replaceState(null, '', '/admin/app/?auth_retry=1');
    const redirect = vi.fn();
    renderWithTheme(<AdminSignInRedirect redirect={redirect} />);

    expect(redirect).not.toHaveBeenCalled();
    expect(screen.getByRole('link', { name: 'Sign in' })).toHaveAttribute(
      'href',
      expect.stringContaining('/auth/login?target_to='),
    );
  });
});

describe('AdminAccessUnavailable', () => {
  it('is neutral and "Try again" reloads; no timer, no redirect', () => {
    vi.useFakeTimers();
    const reload = vi.fn();
    renderWithTheme(<AdminAccessUnavailable reload={reload} />);

    expect(screen.getByRole('heading', { name: "We couldn't check your permissions" })).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(reload).toHaveBeenCalledOnce();
  });
});
