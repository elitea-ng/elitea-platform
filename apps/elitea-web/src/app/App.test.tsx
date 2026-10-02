import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import { DEFAULT_BRAND_PACK } from '@/shared/brand';
import { eliteaFetch } from '@/shared/api/generated/mutator';

import { App } from './App';
import { AppProviders } from './providers';
import { installWebStorageShim } from '../test/webstorage';

installWebStorageShim();

const ALL_KEYS = [
  'VITE_SERVER_URL',
  'VITE_BASE_URI',
  'VITE_SOCKET_SERVER',
  'VITE_SOCKET_PATH',
  'VITE_PUBLIC_PROJECT_ID',
] as const;

const g = globalThis as unknown as Record<string, unknown>;
const realProcessEnv = (g['process'] as { env: Record<string, string | undefined> }).env;

/**
 * The DOM fingerprint a mounted `AppProviders` leaves behind: a `<style>`
 * emitted by `CssBaseline` carrying the default pack's real primary colour
 * (`BrandThemeProvider.test.tsx`'s RED/GREEN (b) technique). This is NOT
 * `InitColorSchemeScript`'s `<script>` — that component is verified inert
 * for this app's `createRoot()` bootstrap (see `BrandThemeProvider.tsx`'s
 * header and its own test), so its absence proves nothing either way about
 * whether `AppProviders` mounted. No `vi.mock()` involved anywhere in this
 * file (R-M1 bans it outside `src/**\/__mocks__/`) — this is real, rendered
 * DOM evidence.
 */
function providerFingerprintPresent(): boolean {
  const styleText = [...document.querySelectorAll('style')].map((s) => s.textContent ?? '').join('\n');
  return styleText.toLowerCase().includes(`--el-palette-primary-main:${DEFAULT_BRAND_PACK.schemes.dark['primary.main']}`);
}

beforeEach(() => {
  resetConfigForTests();
  for (const key of ALL_KEYS) {
    delete realProcessEnv[key];
  }
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
});

describe('App', () => {
  it('RED (control): when AppProviders IS mounted directly, its DOM fingerprint (brand-colour stylesheet) is present — proves the detection technique below is meaningful, not vacuous', () => {
    render(
      <AppProviders>
        <p>probe</p>
      </AppProviders>,
    );

    expect(providerFingerprintPresent()).toBe(true);
  });

  it('RED/GREEN (d): the config-missing short-circuit still works — MissingEnvPage renders and NONE of AppProviders\' fingerprint appears (F3\'s original gate is preserved, not regressed)', () => {
    // No VITE_* vars stubbed: getConfig() resolves 'missing' for all three
    // required keys, exactly like F3's own missing-env-page.test.tsx setup.
    render(<App />);

    // GREEN: MissingEnvPage's real, byte-exact copy (parity item COPY-468)
    // renders — the same assertions unit F3's own test makes.
    expect(screen.getByRole('alert').textContent).toBe('[Error]');
    expect(screen.getByRole('list')).toBeTruthy();

    // GREEN: the fingerprint the RED control case above proved meaningful is
    // NOT present — AppProviders (theme/i18n/query client/error boundary)
    // never mounted.
    expect(providerFingerprintPresent()).toBe(false);
  });

  it('mounts AppProviders around the real router when config resolves (the non-missing branch)', async () => {
    vi.stubEnv('VITE_SERVER_URL', '/api/v2');
    vi.stubEnv('VITE_BASE_URI', '/app/');
    vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');

    render(<App />);

    // MissingEnvPage must NOT render on this branch.
    expect(screen.queryByRole('alert')).toBeNull();
    // AppProviders mounted: the brand-colour fingerprint is present, proven
    // meaningful by the RED control case above — same as that case, just
    // reached through App's real config-ok branch instead of directly.
    await waitFor(() => expect(providerFingerprintPresent()).toBe(true));
  });

  /**
   * Issue #136 B. `configureGeneratedClient({ baseUrl })` was called with no
   * `reauthenticate`, so `http.ts`'s `runReauth()` returned false before doing
   * anything: `needsReauth()` was dead for every 401/403 the app ever saw, and
   * `createAuthPopupController` had no production call site at all.
   *
   * Observed through the real, un-mocked path — `eliteaFetch` is the mutator
   * every generated hook calls, and `window.open` is what the controller
   * reaches for. Returning `null` from the stub is the popup-blocked case,
   * which settles the flight synchronously, so the assertion needs no timers:
   * the URL was requested (that is the wiring under test) and the request
   * still fails as an auth failure (re-auth did not succeed, so nothing is
   * silently swallowed).
   */
  it('configures the generated client with the re-auth flow, so a 401 reaches the popup controller', async () => {
    vi.stubEnv('VITE_SERVER_URL', '/api/v2');
    vi.stubEnv('VITE_BASE_URI', '/app/');
    vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');

    const openedUrls: string[] = [];
    vi.stubGlobal('open', (url?: string | URL) => {
      openedUrls.push(String(url));
      return null; // popup blocked
    });
    vi.stubGlobal('fetch', () =>
      Promise.resolve(new Response('Unauthorized', { status: 401 })),
    );

    render(<App />);
    await waitFor(() => expect(providerFingerprintPresent()).toBe(true));

    await expect(eliteaFetch('/probe-reauth')).rejects.toThrow(/auth failure \(401\)/);

    // More than one flight is expected and correct here: the router's own
    // mounted hooks 401 against the stubbed fetch too, and each SEQUENTIAL
    // failure is entitled to its own flight (single-flight covers CONCURRENT
    // failures and is asserted in `auth/popup.test.ts`). What matters is that
    // every popup the app opens is the real sign-in entry point.
    expect(openedUrls.length).toBeGreaterThan(0);
    for (const raw of openedUrls) {
      const opened = new URL(raw, 'http://localhost');
      expect(opened.pathname).toBe('/auth/login');
      // `VITE_BASE_URI` is `/app/`; the trailing slash must not survive into
      // the callback target or the route it names cannot match. Under vitest
      // `import.meta.env.DEV` is true, so `getAppBasename()` returns '' and
      // the target is root-relative — the assertion is on the SHAPE, which is
      // what breaks if the concatenation regresses.
      const target = opened.searchParams.get('target_to') ?? '';
      expect(target).not.toContain('//auth-callback');
      expect(target).toMatch(/^\/(?:app\/)?auth-callback\?auth_state=[0-9a-f-]{36}$/);
    }
  });

  it('renders the missing variables in C7 contract order under their old UPPER_CASE names', () => {
    vi.stubEnv('VITE_BASE_URI', '/app/'); // leave the other two required keys missing

    render(<App />);

    const items = screen.getAllByRole('listitem');
    expect(items.map((item) => item.textContent)).toEqual([
      'VITE_SERVER_URL',
      'VITE_PUBLIC_PROJECT_ID',
    ]);
  });
});
