/**
 * #6672: Settings › Usage "Refresh data". The button must issue a NEW request
 * for the active scope (not re-render the cache), be disabled while that
 * request runs, and on failure keep the figures already shown and say so.
 * #6682 rides along: a sub-cent spend must not print as $0.00.
 */
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import * as runtimeConfig from '@/shared/config';
import { server } from '@/test/setup';

import type { UsageScope } from './api/projectUsageApi';
import Usage from './Usage';
import { UsageRefreshButton } from './UsageRefreshButton';

const BASE = '/api/v2';
const PROJECT = '7';
const USAGE_URL = `${BASE}/elitea_core/usage/prompt_lib/:projectId/usage`;

const requested: string[] = [];
let spend = 1.25;
let failNext = false;
let gate: Promise<void> | null = null;

beforeEach(() => {
  requested.length = 0;
  spend = 1.25;
  failNext = false;
  gate = null;
  configureGeneratedClient({ baseUrl: BASE });
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: BASE,
      vite_base_uri: '/',
      vite_public_project_id: '1',
      allow_project_own_llms: false,
    },
  });
  server.use(
    http.get(USAGE_URL, async ({ request }) => {
      requested.push(new URL(request.url).searchParams.get('scope') ?? '');
      if (gate !== null) await gate;
      if (failNext) return HttpResponse.json({ error: 'boom' }, { status: 500 });
      return HttpResponse.json({
        can_see_amounts: true,
        spend,
        effective_limit: 10,
        remaining: 10 - spend,
        percent_used: spend * 10,
        warning_pct: 80,
        spend_available: true,
      });
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  resetGeneratedClient();
});

let client: QueryClient | undefined;

/** Hands the test the page's own QueryClient, to drive a background refetch. */
function ClientProbe() {
  client = useQueryClient();
  return null;
}

function renderPage(scope: UsageScope = 'project') {
  return render(
    <AppProviders>
      <ClientProbe />
      <UsageRefreshButton projectId={PROJECT} scope={scope} />
      <Usage projectId={PROJECT} scope={scope} />
    </AppProviders>,
  );
}

describe('Settings > Usage refresh (#6672)', () => {
  it('fetches the active scope again and renders the new figures', async () => {
    renderPage('user');
    expect(await screen.findByText(/^\D*1\.25$/)).toBeInTheDocument();
    expect(requested).toEqual(['user']);

    spend = 2.5;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh data' }));

    expect(await screen.findByText(/^\D*2\.50$/)).toBeInTheDocument();
    expect(requested).toEqual(['user', 'user']);
  });

  it('is disabled and shows progress while the request is in flight', async () => {
    renderPage();
    await screen.findByText(/^\D*1\.25$/);

    let release: () => void = () => undefined;
    gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const button = screen.getByRole('button', { name: 'Refresh data' });
    await userEvent.click(button);

    await waitFor(() => expect(button).toBeDisabled());
    expect(screen.getByTestId('settings-usage-refreshing')).toBeInTheDocument();

    release();
    await waitFor(() => expect(button).not.toBeDisabled());
    expect(requested).toEqual(['project', 'project']);
  });

  it('keeps the previous figures and shows a toast when the refresh fails', async () => {
    renderPage();
    await screen.findByText(/^\D*1\.25$/);

    failNext = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh data' }));

    expect(await screen.findByText('Unable to refresh usage data. Please try again.')).toBeInTheDocument();
    expect(screen.getByText(/^\D*1\.25$/)).toBeInTheDocument();
    // The load-failure panel is for "nothing to show", not a failed refresh…
    expect(screen.queryByTestId('settings-usage-error')).not.toBeInTheDocument();
    // …but the figures still say they are not current.
    expect(screen.getByTestId('settings-usage-stale')).toBeInTheDocument();
  });

  // A click the user is watching: the app's default 5xx retry (one more
  // request after a 1s backoff) only delayed the toast.
  it('reports a failed refresh without retrying a server error first', async () => {
    renderPage();
    await screen.findByText(/^\D*1\.25$/);

    failNext = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh data' }));

    expect(await screen.findByTestId('settings-usage-refresh-error')).toBeInTheDocument();
    expect(requested).toEqual(['project', 'project']);
  });

  // A refetch with no toast — window refocus, a remount past the stale time —
  // failing over figures already shown must not leave them unmarked.
  it('marks the figures stale when a background refetch fails', async () => {
    renderPage();
    await screen.findByText(/^\D*1\.25$/);
    expect(screen.queryByTestId('settings-usage-stale')).not.toBeInTheDocument();

    failNext = true;
    void client?.refetchQueries({ queryKey: ['settings', 'usage'] });

    // The app default retries a 5xx once after 1s before settling.
    expect(await screen.findByTestId('settings-usage-stale', {}, { timeout: 4000 })).toBeInTheDocument();
    expect(screen.getByText(/^\D*1\.25$/)).toBeInTheDocument();
    expect(screen.queryByTestId('settings-usage-error')).not.toBeInTheDocument();
    expect(screen.queryByTestId('settings-usage-refresh-error')).not.toBeInTheDocument();
  });

  // MUI's Snackbar renders an empty SnackbarContent when it has no child, so
  // the Alert must stay mounted while the toast closes.
  it('keeps the toast content mounted while it closes', async () => {
    renderPage();
    await screen.findByText(/^\D*1\.25$/);

    failNext = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh data' }));
    const toast = await screen.findByTestId('settings-usage-refresh-error');

    await userEvent.click(within(toast).getByRole('button', { name: /close/i }));
    // Mid-exit, no bare SnackbarContent (an empty role="alert" bar) replaces
    // the Alert.
    for (const alert of screen.queryAllByRole('alert')) expect(alert).not.toHaveTextContent(/^$/);
  });
});

describe('Settings > Usage small amounts (#6682)', () => {
  it('does not print a sub-cent spend as zero', async () => {
    spend = 0.004;
    renderPage();
    expect(await screen.findByText(/^\D*0\.004$/)).toBeInTheDocument();
  });

  it('prints a sub-micro-dollar spend as an upper bound', async () => {
    spend = 8e-8;
    renderPage();
    expect(await screen.findByText(/^< \D*0\.000001$/)).toBeInTheDocument();
  });
});
