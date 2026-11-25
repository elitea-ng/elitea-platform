import { act, cleanup, fireEvent, screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BRAND_PACK_GLOBAL } from '@/shared/brand';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import {
  getGetAdminConsoleItemMockHandler,
  getGetAdminConsoleItemResponseMock,
} from '@/shared/api/generated/admin/admin.msw';
import {
  getGetClientDiscoveryMockHandler,
  getGetClientDiscoveryResponseMock,
} from '@/shared/api/generated/discovery/discovery.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderAdminRoute } from './__tests__/testRouter';
import { AdminAccessDenied } from './AdminAccessDenied';
import { AdminItemAccessDenied } from './AdminItemAccessDenied';

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

/* -- one item a typed URL names (ADR-0059 decision 6, R3.2, issue 2532) -- */

function discoveryWithContact(contact: string) {
  const document = getGetClientDiscoveryResponseMock();
  return { ...document, client_policy: { ...document.client_policy, support_contact: contact } };
}

describe('AdminItemAccessDenied', () => {
  let rolesListingRequests = 0;

  beforeEach(() => {
    vi.useRealTimers();
    configureGeneratedClient({ baseUrl: 'https://elitea.example' });
    rolesListingRequests = 0;
    server.use(
      // The item and its granting roles: the contract's example.
      getGetAdminConsoleItemMockHandler(getGetAdminConsoleItemResponseMock()),
      getGetClientDiscoveryMockHandler(discoveryWithContact('Service desk: help@example.com')),
      // The listing that names administrators: this page must never read it.
      http.get('*/admin/access/roles', () => {
        rolesListingRequests += 1;
        return HttpResponse.json({ total: 0, rows: [] });
      }),
    );
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('names the item, a granting role and the support contact, and no person', async () => {
    renderAdminRoute(<AdminItemAccessDenied itemId="budgets-alerts" label="Budgets" />);

    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('You cannot open Budgets');
    const roles = await screen.findByTestId('admin-item-denied-roles');
    expect(roles).toHaveTextContent('LLM manager');
    expect(roles).toHaveTextContent('budget_reader');

    const link = await screen.findByRole('link', { name: 'Service desk: help@example.com' });
    expect(link).toHaveAttribute('href', 'mailto:help@example.com');

    expect(rolesListingRequests).toBe(0);
    expect(screen.getByTestId('admin-item-denied')).not.toHaveTextContent(/@autotest\.local|dev@elitea\.ai/);
  });

  it('says whom to ask when the deployment has no support contact', async () => {
    server.use(getGetClientDiscoveryMockHandler(discoveryWithContact('')));
    renderAdminRoute(<AdminItemAccessDenied itemId="budgets-alerts" label="Budgets" />);

    expect(await screen.findByTestId('admin-item-denied-contact')).toHaveTextContent(
      'contact the people who manage this deployment',
    );
  });

  it('says the roles could not be read rather than that none grants it', async () => {
    server.use(http.get('*/admin/console/items/:itemId', () => new HttpResponse(null, { status: 500 })));
    renderAdminRoute(<AdminItemAccessDenied itemId="budgets-alerts" label="Budgets" />);

    expect(await screen.findByTestId('admin-item-denied-roles-error')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-item-denied-no-role')).toBeNull();
  });
});
