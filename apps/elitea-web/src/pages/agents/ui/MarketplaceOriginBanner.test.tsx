import { describe, expect, it, vi } from 'vitest';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { MarketplaceOriginBanner } from './MarketplaceOriginBanner';

vi.mock('@tanstack/react-router', () => ({
  useRouter: () => ({
    buildLocation: ({ params }: { params: { listingId: string } }) => ({
      href: `/marketplace/agents/${params.listingId}`,
    }),
  }),
}));

const ORIGIN: MarketplaceOrigin = {
  mode: 'installed',
  listing_id: '7',
  listing_name: 'Changelog Writer',
  listing_state: 'listed',
  version_id: '311',
  version_name: 'v2.3',
  version_state: 'available',
  digest: 'abc',
  latest_version: null,
};

describe('MarketplaceOriginBanner', () => {
  it('says an installed definition is read-only and links the listing', () => {
    const { getByTestId, getByRole, queryByTestId } = renderWithTheme(<MarketplaceOriginBanner origin={ORIGIN} />);
    expect(getByTestId('marketplace-origin-line').textContent).toContain(
      'Installed from Changelog Writer v2.3. The definition is read-only. Fork it to change it.',
    );
    expect(getByRole('link', { name: 'Open the listing' }).getAttribute('href')).toBe('/marketplace/agents/7');
    expect(queryByTestId('marketplace-origin-withdrawn')).toBeNull();
    expect(queryByTestId('marketplace-origin-update')).toBeNull();
  });

  it('warns about a withdrawn listing, a yanked version and a newer one, in text', () => {
    const { getByTestId } = renderWithTheme(
      <MarketplaceOriginBanner
        origin={{
          ...ORIGIN,
          listing_state: 'delisted',
          version_state: 'yanked',
          latest_version: { id: '400', name: 'v2.4', published_at: '2026-11-01T00:00:00Z' },
        }}
      />,
    );
    expect(getByTestId('marketplace-origin-withdrawn').textContent).toContain('Listing withdrawn');
    expect(getByTestId('marketplace-origin-yanked').textContent).toContain(
      'This version was yanked. Your agent keeps running.',
    );
    expect(getByTestId('marketplace-origin-update').textContent).toContain('Update available: v2.4.');
  });

  it('shows a fork as a line only', () => {
    const { getByTestId } = renderWithTheme(<MarketplaceOriginBanner origin={{ ...ORIGIN, mode: 'forked' }} />);
    expect(getByTestId('marketplace-origin-line').textContent).toContain('Forked from Changelog Writer v2.3.');
    expect(getByTestId('marketplace-origin-line').textContent).not.toContain('read-only');
  });
});
