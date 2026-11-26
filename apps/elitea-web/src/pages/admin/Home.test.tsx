/**
 * Admin › Home (ADR-0059 decision 6; R3.3a, issue 2533) through the REAL
 * admin router, from each built-in role's capability document: the golden
 * files the server's own tests write
 * (`services/elitea-main/internal/api/v2/adminaccess/testdata/console`).
 *
 *  - Each role sees one card per area its document opens, with the items it
 *    opens there.
 *  - Only the queues the document gives at `write` show, each with the
 *    total its list route returns.
 *  - The empty document (`emergency_access`, whose one area has no page
 *    yet) shows the no-access page: the support contact, no administrator
 *    name, no navigation.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { render, screen, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetClientDiscoveryMockHandler,
  getGetClientDiscoveryResponseMock,
} from '@/shared/api/generated/discovery/discovery.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';
import { testTimeout } from '@/test/timeouts';

import { createAdminRouter } from './router';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GOLDEN = join(process.cwd(), '../../services/elitea-main/internal/api/v2/adminaccess/testdata/console');

interface ConsoleDocument {
  readonly areas: { readonly id: string; readonly items: { readonly id: string; level: string }[] }[];
  readonly [key: string]: unknown;
}

function golden(role: string): ConsoleDocument {
  return JSON.parse(readFileSync(join(GOLDEN, `${role}.golden.json`), 'utf8')) as ConsoleDocument;
}

function serveDocument(document: ConsoleDocument): void {
  server.use(http.get('*/admin/console', () => HttpResponse.json(document)));
}

function mountAdmin(path: string): ReturnType<typeof createAdminRouter> {
  window.history.replaceState(null, '', `/admin/app${path}`);
  const router = createAdminRouter();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return router;
}

/** The queue reads, each with a total of its own so a mixed-up count shows. */
function serveQueues(): void {
  server.use(
    http.get('*/admin/moderation_statuses/administration', () => HttpResponse.json({ rows: [], total: 5 })),
    http.get('*/admin/connector_requests', () => HttpResponse.json({ rows: [], total: 2 })),
    http.get('*/admin/delegation_review', () =>
      HttpResponse.json({
        pending: 3,
        legacy: 0,
        stale: 4,
        mode: 'enforce',
        mode_stored: false,
        principal_bound: true,
        report_window_days: 14,
        projects: [],
      }),
    ),
  );
}

function discoveryWithContact(contact: string) {
  const document = getGetClientDiscoveryResponseMock();
  return { ...document, client_policy: { ...document.client_policy, support_contact: contact } };
}

function renderedCards(): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const card of screen.queryAllByTestId(/^admin-home-area-(?!label)/)) {
    const id = card.getAttribute('data-testid')!.replace('admin-home-area-', '');
    out[id] = within(card)
      .queryAllByTestId(/^admin-home-item-/)
      .map((link) => link.getAttribute('data-testid')!.replace('admin-home-item-', ''));
  }
  return out;
}

function renderedQueues(): string[] {
  return screen
    .queryAllByTestId(/^admin-home-queue-(?!count-)/)
    .map((queue) => queue.getAttribute('data-testid')!.replace('admin-home-queue-', ''));
}

/** The cards a document shows: each area with the items it opens, as the document lists them. */
function expectedCards(document: ConsoleDocument): Record<string, string[]> {
  return Object.fromEntries(document.areas.map((area) => [area.id, area.items.map((entry) => entry.id)]));
}

const QUEUES_AT_WRITE: Record<string, readonly string[]> = {
  super_admin: ['app-requests', 'connector-requests', 'delegation-review'],
  admin: ['app-requests', 'connector-requests', 'delegation-review'],
  llm_manager: [],
  project_administrator: ['app-requests'],
  security_auditor: [],
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  serveQueues();
  server.use(getGetClientDiscoveryMockHandler(discoveryWithContact('Service desk: help@example.com')));
});

afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
});

describe('Admin › Home', () => {
  for (const [role, queues] of Object.entries(QUEUES_AT_WRITE)) {
    it(
      `shows the ${role} persona a card per area of its document, and the queues it decides`,
      async () => {
        const document = golden(role);
        serveDocument(document);
        mountAdmin('/');
        expect(await screen.findByTestId('admin-home', undefined, { timeout: 10_000 })).toBeInTheDocument();
        expect(screen.getByRole('heading', { level: 1, name: 'Home' })).toBeInTheDocument();
        // Identity-trust has no page of its own, so its card lists what the navigation does.
        const cards = expectedCards(document);
        if (cards.people_access) cards.people_access = cards.people_access.filter((id) => id !== 'identity-trust');
        expect(renderedCards()).toEqual(cards);
        expect(renderedQueues()).toEqual(queues);
        expect(screen.getByTestId('admin-nav')).toBeInTheDocument();
      },
      testTimeout(30_000),
    );
  }

  it('counts each queue from the total of its own list', async () => {
    serveDocument(golden('super_admin'));
    mountAdmin('/');
    expect(await screen.findByTestId('admin-home-queue-count-app-requests', undefined, { timeout: 10_000 })).toBeInTheDocument();
    await screen.findByText('5');
    expect(screen.getByTestId('admin-home-queue-count-app-requests')).toHaveTextContent('5');
    expect(await screen.findByText('2')).toBeInTheDocument();
    expect(screen.getByTestId('admin-home-queue-count-connector-requests')).toHaveTextContent('2');
    // Pending grants plus grants whose callee changed: 3 + 4.
    expect(await screen.findByText('7')).toBeInTheDocument();
    expect(screen.getByTestId('admin-home-queue-count-delegation-review')).toHaveTextContent('7');
  }, testTimeout(30_000));

  it('says a count could not be read rather than showing zero', async () => {
    server.use(http.get('*/admin/connector_requests', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    serveDocument(golden('super_admin'));
    mountAdmin('/');
    expect(await screen.findByText('Could not be read', undefined, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.getByTestId('admin-home-queue-count-connector-requests')).toHaveTextContent('Could not be read');
  }, testTimeout(30_000));

  it('does not show a queue the caller can read but not decide', async () => {
    const document = golden('super_admin');
    const readOnly: ConsoleDocument = {
      ...document,
      areas: document.areas.map((area) => ({
        ...area,
        items: area.items.map((entry) => (entry.id === 'connector-requests' ? { ...entry, level: 'read' } : entry)),
      })),
    };
    serveDocument(readOnly);
    mountAdmin('/');
    expect(await screen.findByTestId('admin-home', undefined, { timeout: 10_000 })).toBeInTheDocument();
    expect(renderedQueues()).toEqual(['app-requests', 'delegation-review']);
    // The page itself stays on its area's card: reading it is still open.
    expect(screen.getByTestId('admin-home-item-connector-requests')).toBeInTheDocument();
  }, testTimeout(30_000));

  it('shows the empty document the no-access page: the support contact, no administrator, no navigation', async () => {
    const requests: string[] = [];
    server.events.on('request:start', ({ request }) => requests.push(new URL(request.url).pathname));
    serveDocument(golden('emergency_access'));
    mountAdmin('/');
    expect(await screen.findByTestId('admin-no-access', undefined, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'You have no administration access' })).toBeInTheDocument();
    const link = await screen.findByRole('link', { name: 'Service desk: help@example.com' });
    expect(link).toHaveAttribute('href', 'mailto:help@example.com');
    expect(screen.queryByTestId('admin-nav')).not.toBeInTheDocument();
    expect(screen.queryByTestId('admin-home')).not.toBeInTheDocument();
    // No administrator: the page reads no listing that names one.
    expect(requests.filter((path) => path.includes('/admin/access/'))).toEqual([]);
    server.events.removeAllListeners();
  }, testTimeout(30_000));

  it('falls back to a fixed sentence when the deployment names no support contact', async () => {
    server.use(getGetClientDiscoveryMockHandler(discoveryWithContact('')));
    serveDocument(golden('emergency_access'));
    mountAdmin('/');
    expect(await screen.findByTestId('admin-item-denied-contact', undefined, { timeout: 10_000 })).toHaveTextContent(
      'To ask for administration access, contact the people who manage this deployment.',
    );
  }, testTimeout(30_000));
});
