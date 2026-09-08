/**
 * The standalone splash page, at what the operator can change about it.
 *
 * This entry is built by `vite build --mode maintenance` into a single
 * document an ingress serves when the platform is down. It had NO test, and it
 * had no way to show anything an operator wrote today — its copy came from
 * build-time `VITE_MAINTENANCE_*` variables, which cannot be changed without a
 * redeploy. Both cases below are about that: the published copy wins when the
 * platform answers, and the page renders exactly as before when it does not.
 */
import { ThemeProvider } from '@mui/material/styles';
import { configure, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import MaintenancePage from './MaintenancePage';
import theme from '../theme';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

function renderPage() {
  render(
    <ThemeProvider theme={theme} defaultMode="light">
      <MaintenancePage />
    </ThemeProvider>,
  );
}

function stubSettings(body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(body) }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('MaintenancePage (standalone entry)', () => {
  it('shows its own headline when the platform does not answer', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')));
    renderPage();

    expect(await screen.findByText('Elitea is under maintenance!')).toBeVisible();
    // Nothing published, so neither operator-copy element mounts.
    await waitFor(() => {
      expect(screen.queryByTestId('maintenance-published-html')).toBeNull();
    });
    expect(screen.queryByTestId('maintenance-published-message')).toBeNull();
  });

  it('shows the operator title and HTML body when the platform publishes them', async () => {
    stubSettings({
      maintenance: {
        enabled: true,
        title: 'Scheduled database upgrade',
        html: '<p>Watch the <a href="https://status.example.com">status page</a>.</p>',
        message: 'This message loses to the HTML body.',
      },
    });
    renderPage();

    expect(await screen.findByText('Scheduled database upgrade')).toBeVisible();
    const body = await screen.findByTestId('maintenance-published-html');
    expect(body).toHaveTextContent('Watch the status page.');
    // The page's own headline is REPLACED, not shown beside the operator's.
    expect(screen.queryByText('Elitea is under maintenance!')).toBeNull();
    expect(screen.queryByTestId('maintenance-published-message')).toBeNull();
  });

  it('sanitises the published body before injecting it', async () => {
    stubSettings({
      maintenance: {
        title: '',
        html: '<p>Back soon.</p><script>window.pwned = true</script>',
        message: '',
      },
    });
    renderPage();

    const body = await screen.findByTestId('maintenance-published-html');
    expect(body).toHaveTextContent('Back soon.');
    expect(body.querySelector('script')).toBeNull();
    expect(body.innerHTML).not.toContain('pwned');
  });

  it('falls back to the published message when no HTML body was authored', async () => {
    stubSettings({
      maintenance: { title: '', html: '', message: 'Back at 14:00 UTC.' },
    });
    renderPage();

    expect(await screen.findByTestId('maintenance-published-message')).toHaveTextContent(
      'Back at 14:00 UTC.',
    );
    // No HTML body means the page keeps its own headline.
    expect(screen.getByText('Elitea is under maintenance!')).toBeVisible();
  });
});
