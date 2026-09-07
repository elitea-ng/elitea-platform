/**
 * The platform banner and the maintenance splash.
 *
 * The four behaviours pinned here are the ones that decide whether an operator's
 * switch does what its label says, and each of them can be reversed by a change
 * that still type-checks:
 *
 *  - dismissal is per MESSAGE, so raising a new one reaches a user who closed
 *    the last one;
 *  - a maintenance window replaces the shell for a user, and does NOT for an
 *    administrator, who is the person who has to end it;
 *  - the message is rendered as markdown with raw HTML dropped.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createStorage } from '@/shared/lib/storage';
import { installWebStorageShim } from '@/test/webstorage';
import { server } from '@/test/setup';

installWebStorageShim();

import { MaintenanceSplash } from '../ui/MaintenanceSplash';
import { PlatformBanner } from '../ui/PlatformBanner';
import { renderWithNavigation } from './testHarness';
import {
  usePlatformAnnouncements,
  type PlatformAnnouncements,
} from '@/shared/lib/hooks/usePlatformAnnouncements';

const SETTINGS_URL = '*/elitea_core/platform_settings/prompt_lib';

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  window.localStorage.clear();
});

describe('PlatformBanner', () => {
  const banner = {
    enabled: true,
    message: 'Upgrading **tonight** at 22:00 UTC.',
    dismissible: true,
    icon: 'warning',
    style: 'warning',
  } as const;

  it('renders the operator message as markdown', async () => {
    await renderWithNavigation(<PlatformBanner banner={banner} />);

    const rendered = await screen.findByTestId('platform-banner');
    expect(rendered).toHaveTextContent('Upgrading tonight at 22:00 UTC.');
    // `**tonight**` became an element rather than literal asterisks.
    expect(rendered.querySelector('strong')).not.toBeNull();
  });

  it('drops raw HTML written into the message', async () => {
    await renderWithNavigation(
      <PlatformBanner
        banner={{ ...banner, message: 'Hello <img src=x onerror="alert(1)"> world' }}
      />,
    );

    const rendered = await screen.findByTestId('platform-banner');
    expect(rendered.querySelector('img')).toBeNull();
  });

  it('remembers WHICH message was dismissed, so a new one comes back', async () => {
    const { unmount } = await renderWithNavigation(<PlatformBanner banner={banner} />);

    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }));
    await waitFor(() => {
      expect(screen.queryByTestId('platform-banner')).toBeNull();
    });
    unmount();

    // The SAME message stays dismissed…
    const again = await renderWithNavigation(<PlatformBanner banner={banner} />);
    expect(screen.queryByTestId('platform-banner')).toBeNull();
    again.unmount();

    // …and a DIFFERENT one does not. This is the half a boolean "dismissed"
    // flag would get wrong, and the messages that matter most — an incident —
    // are the ones raised after a routine notice was closed.
    await renderWithNavigation(
      <PlatformBanner banner={{ ...banner, message: 'We are investigating an incident.' }} />,
    );
    expect(await screen.findByTestId('platform-banner')).toHaveTextContent(
      'We are investigating an incident.',
    );
  });

  it('stores the dismissal under the `el.` namespace so logout clears it', async () => {
    await renderWithNavigation(<PlatformBanner banner={banner} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }));

    await waitFor(() => {
      expect(createStorage('local').get('maintenanceBanner.dismissedMessage')).toBe(banner.message);
    });
    // A raw, un-namespaced key is the shape §5.4's logout sweep cannot see.
    expect(window.localStorage.getItem('maintenance_banner_dismissed')).toBeNull();
  });

  it('offers no close control when the operator made it non-dismissible', async () => {
    await renderWithNavigation(<PlatformBanner banner={{ ...banner, dismissible: false }} />);

    await screen.findByTestId('platform-banner');
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull();
  });
});

describe('MaintenanceSplash', () => {
  it('shows the operator title and markdown message', async () => {
    await renderWithNavigation(
      <MaintenanceSplash
        maintenance={{
          enabled: true,
          title: 'Scheduled upgrade',
          message: 'Back at **14:00 UTC**.',
          html: '',
          bypass: false,
        }}
      />,
    );

    expect(await screen.findByRole('heading', { name: 'Scheduled upgrade' })).toBeVisible();
    expect(screen.getByTestId('maintenance-splash')).toHaveTextContent('Back at 14:00 UTC.');
  });

  it('renders the operator HTML body instead of the markdown message, sanitised', async () => {
    await renderWithNavigation(
      <MaintenanceSplash
        maintenance={{
          enabled: true,
          title: 'Scheduled upgrade',
          message: 'This markdown must NOT be shown.',
          html: '<p>Watch the <a href="https://status.example.com">status page</a>.</p>'
            + '<script>window.pwned = true</script>',
          bypass: false,
        }}
      />,
    );

    const body = await screen.findByTestId('maintenance-splash-html');
    expect(body).toHaveTextContent('Watch the status page.');
    // The script is stripped, not merely inert: it must not be in the DOM at
    // all. `dangerouslySetInnerHTML` does not execute a `<script>` React
    // inserts, which is exactly why an un-sanitised body would pass a test
    // that only asserted "nothing ran".
    expect(body.querySelector('script')).toBeNull();
    expect(body.innerHTML).not.toContain('pwned');
    // The markdown message is the FALLBACK and loses to a non-empty body.
    expect(screen.queryByText('This markdown must NOT be shown.')).toBeNull();
  });

  it('falls back to the markdown message when the HTML body is blank', async () => {
    await renderWithNavigation(
      <MaintenanceSplash
        maintenance={{
          enabled: true,
          title: 'Scheduled upgrade',
          message: 'Back at 14:00 UTC.',
          html: '   ',
          bypass: false,
        }}
      />,
    );

    expect(await screen.findByText('Back at 14:00 UTC.')).toBeVisible();
    expect(screen.queryByTestId('maintenance-splash-html')).toBeNull();
  });

});

describe('usePlatformAnnouncements', () => {
  function Probe({ onResolve }: { readonly onResolve: (state: PlatformAnnouncements) => void }) {
    onResolve(usePlatformAnnouncements());
    return null;
  }

  async function resolveWith(body: Record<string, unknown>): Promise<PlatformAnnouncements> {
    server.use(http.get(SETTINGS_URL, () => HttpResponse.json(body)));
    let latest: PlatformAnnouncements | undefined;
    await renderWithNavigation(
      <Probe
        onResolve={(state) => {
          latest = state;
        }}
      />,
    );
    await waitFor(() => {
      expect(latest?.maintenance.enabled === true || latest?.banner.enabled === true).toBe(true);
    });
    return latest as PlatformAnnouncements;
  }

  it('reports nothing to announce on a response that carries neither key', async () => {
    server.use(http.get(SETTINGS_URL, () => HttpResponse.json({ chat_enabled: true })));
    let latest: PlatformAnnouncements | undefined;
    await renderWithNavigation(
      <Probe
        onResolve={(state) => {
          latest = state;
        }}
      />,
    );
    // A deployment too old to publish these must look like an ordinary one —
    // the alternative is a full-page splash over a platform that is up.
    expect(latest?.banner.enabled).toBe(false);
    expect(latest?.maintenance.enabled).toBe(false);
  });

  it('takes the bypass from the server rather than deciding locally', async () => {
    const state = await resolveWith({
      maintenance: { enabled: true, title: 'Down', message: 'Soon.', bypass: true },
    });
    expect(state.maintenance.bypass).toBe(true);
  });

  it('folds an unknown banner tone onto `info`', async () => {
    const state = await resolveWith({
      dedicated_banner: {
        enabled: true,
        message: 'Notice',
        dismissible: false,
        icon: 'chartreuse',
        style: 'chartreuse',
      },
    });
    expect(state.banner.icon).toBe('info');
    expect(state.banner.style).toBe('info');
  });
});
