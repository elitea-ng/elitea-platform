import type { ReactElement } from 'react';

import { afterEach, describe, expect, it } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { ThemeProvider } from '@mui/material/styles';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import HelpCenterPage, { resolveLinks } from './HelpCenterPage';
import { RESOURCE_CARD_CONFIGS } from './lib/ResourceCardConfig';
import { resourcesVersionLabel } from './lib/useResourcesConfig';

const documentationConfig = RESOURCE_CARD_CONFIGS[0]!;
// #891: the only two cards left with NO `defaultLinks` (no real video/tour
// content exists to default to) — the right subjects for a "still returns
// [], not a fabricated fallback" assertion.
const videoLibraryConfig = RESOURCE_CARD_CONFIGS[2]!;

/**
 * The Help Center now READS its cards from
 * `GET /admin/plugin_config_values/prompt_lib/resources` — the section the admin
 * Configuration page writes (unit A14, issue #200). Before that the hook made no
 * request at all, and the endpoint it would have called returned chat and upload
 * limits, which is why every card rendered "No links configured".
 *
 * So these tests assert the WIRING, not just the layout: a page that fell back to
 * its defaults on every response would still render six cards.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderHelpCenter(ui: ReactElement): RenderResult {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        {ui}
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  resetGeneratedClient();
});

function serveResourceValues(values: Record<string, unknown>): void {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/plugin_config_values/prompt_lib/resources', () =>
      HttpResponse.json({ values }),
    ),
  );
}

describe('resolveLinks (finding #3 regression)', () => {
  it('returns an empty array when the config value for the key is absent and the card has no defaultLinks', () => {
    expect(resolveLinks(videoLibraryConfig, {})).toEqual([]);
  });

  it('returns an empty array when the config value is present but not an array, and the card has no defaultLinks', () => {
    expect(resolveLinks(videoLibraryConfig, { [videoLibraryConfig.linksKey]: 'not-an-array' })).toEqual([]);
  });

  it('returns the configured links verbatim once a real admin config value provides them', () => {
    const links = [
      { title: 'API Reference', url: 'https://docs.example.com/api' },
      { title: 'Legacy guide' },
    ];
    expect(resolveLinks(documentationConfig, { [documentationConfig.linksKey]: links })).toEqual(links);
  });

  it("only reads the given card's own linksKey, not another card's", () => {
    const releaseNotesConfig = RESOURCE_CARD_CONFIGS[1]!;
    const links = [{ title: 'v2.3.0 notes', url: 'https://example.com/changelog' }];
    expect(resolveLinks(videoLibraryConfig, { [releaseNotesConfig.linksKey]: links })).toEqual([]);
  });

  /*
   * #891/ELITEA-0967,0972,0973,0975: a fresh deployment (no admin config at
   * all) used to leave every card's link list empty. Cards that ship a real
   * default (`ResourceCardConfig.ts`) now fall back to it instead.
   */
  it("falls back to the card's own defaultLinks when the config value is absent", () => {
    expect(resolveLinks(documentationConfig, {})).toEqual(documentationConfig.defaultLinks);
    expect(documentationConfig.defaultLinks?.length).toBeGreaterThan(0);
  });

  /*
   * The real shape on the wire: `GET /admin/plugin_config_values/prompt_lib/
   * resources` answers every card's `linksKey` as `[]`, not an absent key,
   * on a fresh deployment (confirmed live) — an empty array must fall back
   * to defaultLinks too, or #891's fix never actually applies in production.
   */
  it("also falls back to defaultLinks when the config value is an EMPTY array (the real fresh-deployment shape)", () => {
    expect(resolveLinks(documentationConfig, { [documentationConfig.linksKey]: [] })).toEqual(
      documentationConfig.defaultLinks,
    );
  });

  it("an admin-configured value still wins over the card's defaultLinks", () => {
    const links = [{ title: 'Admin override', url: 'https://example.com/override' }];
    expect(resolveLinks(documentationConfig, { [documentationConfig.linksKey]: links })).toEqual(links);
  });
});

describe('resourcesVersionLabel', () => {
  it('is empty when neither value is configured, rather than a bare "Version:"', () => {
    expect(resourcesVersionLabel({})).toBe('');
    expect(resourcesVersionLabel({ resources_information_version: '   ' })).toBe('');
  });

  it('combines the version and the upgrade date the administrator set', () => {
    expect(
      resourcesVersionLabel({
        resources_information_version: '8.2.0',
        resources_information_upgrade_date: '2026-08-01',
      }),
    ).toBe('Version: 8.2.0 (2026-08-01)');
  });

  it('renders either one alone', () => {
    expect(resourcesVersionLabel({ resources_information_version: '8.2.0' })).toBe('Version: 8.2.0');
    expect(resourcesVersionLabel({ resources_information_upgrade_date: '2026-08-01' })).toBe(
      'Last upgrade: 2026-08-01',
    );
  });
});

describe('HelpCenterPage', () => {
  it('renders the links an administrator configured, from the endpoint', async () => {
    serveResourceValues({
      [documentationConfig.linksKey]: [
        { title: 'Getting started', url: 'https://docs.example.com/start' },
      ],
    });

    renderHelpCenter(<HelpCenterPage />);

    // The assertion the pre-A14 hook could not pass: it made no request, so no
    // response could ever put a link on screen.
    const link = await screen.findByRole('link', { name: 'Getting started' });
    expect(link).toHaveAttribute('href', 'https://docs.example.com/start');
  });

  it('hides a card the administrator disabled', async () => {
    const videoConfig = RESOURCE_CARD_CONFIGS[2]!;
    serveResourceValues({ [videoConfig.enabledKey]: false });

    renderHelpCenter(<HelpCenterPage />);

    await waitFor(() => {
      expect(screen.queryByText(videoConfig.defaultTitle)).not.toBeInTheDocument();
    });
    // The others are untouched — a disabled flag must not blank the page.
    expect(screen.getByText(documentationConfig.defaultTitle)).toBeInTheDocument();
  });

  /* onetest: ELITEA-NONE (#890) — axe `scrollable-region-focusable`: the content
   * region (`contentSx`'s `overflowY: auto` Box) must itself be reachable and
   * scrollable by a keyboard-only user, not just by mouse/trackpad. No dedicated
   * e2e case pins this (see `shell.resources.spec.ts`'s RES01 comment); regression-
   * tested here instead. */
  it('the scrollable content region is keyboard-focusable (#890)', async () => {
    serveResourceValues({});
    renderHelpCenter(<HelpCenterPage />);

    const region = await screen.findByRole('region', { name: 'Help Center content' });
    expect(region).toHaveAttribute('tabindex', '0');
  });

  it('falls back to every card enabled with no links when the read fails', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('*/admin/plugin_config_values/prompt_lib/resources', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );

    renderHelpCenter(<HelpCenterPage />);

    // Failing OPEN is deliberate: hiding the Help Center's contents on a
    // transient error is worse than showing them unconfigured, which is what an
    // unconfigured platform shows anyway.
    for (const config of RESOURCE_CARD_CONFIGS) {
      expect(await screen.findByText(config.defaultTitle)).toBeInTheDocument();
    }
    // #891: cards with a real `defaultLinks` (Documentation, Release Notes,
    // Tutorials) now render THAT instead of the placeholder — only the two
    // cards with no real content left (Video Library, Interactive Tours)
    // still show it.
    const cardsWithNoDefault = RESOURCE_CARD_CONFIGS.filter((config) => config.defaultLinks === undefined);
    expect(screen.getAllByText('No links configured')).toHaveLength(cardsWithNoDefault.length);
    expect(screen.getByRole('link', { name: 'Getting Started' })).toBeInTheDocument();
    expect(screen.getByText('Latest', { exact: true })).toBeInTheDocument();
  });
});
