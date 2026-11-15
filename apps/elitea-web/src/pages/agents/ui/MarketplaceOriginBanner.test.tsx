import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';

import { MarketplaceOriginBanner } from './MarketplaceOriginBanner';

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
  it('says an installed definition is read-only and links the listing', async () => {
    const { getByTestId, getByRole, queryByTestId } = await renderBanner(ORIGIN);
    expect(getByTestId('marketplace-origin-line').textContent).toContain(
      'Installed from Changelog Writer v2.3. The definition is read-only. Fork it to change it.',
    );
    expect(getByRole('link', { name: 'Open the listing' }).getAttribute('href')).toBe('/marketplace/agents/7');
    expect(queryByTestId('marketplace-origin-withdrawn')).toBeNull();
    expect(queryByTestId('marketplace-origin-update')).toBeNull();
  });

  it('warns about a withdrawn listing, a yanked version and a newer one, in text', async () => {
    const { getByTestId } = await renderBanner({
      ...ORIGIN,
      listing_state: 'delisted',
      version_state: 'yanked',
      latest_version: { id: '400', name: 'v2.4', published_at: '2026-11-01T00:00:00Z' },
    });
    expect(getByTestId('marketplace-origin-withdrawn').textContent).toContain('Listing withdrawn');
    expect(getByTestId('marketplace-origin-yanked').textContent).toContain(
      'This version was yanked. Your agent keeps running.',
    );
    expect(getByTestId('marketplace-origin-update').textContent).toContain('Update available: v2.4.');
  });

  it('shows a fork as a line only', async () => {
    const { getByTestId } = await renderBanner({ ...ORIGIN, mode: 'forked' });
    expect(getByTestId('marketplace-origin-line').textContent).toContain('Forked from Changelog Writer v2.3.');
    expect(getByTestId('marketplace-origin-line').textContent).not.toContain('read-only');
  });
});

/** The banner inside a real router with the listing route, so its links build real hrefs. */
async function renderBanner(origin: MarketplaceOrigin) {
  const root = createRootRoute({ component: Outlet });
  const index = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => <MarketplaceOriginBanner origin={origin} />,
  });
  const listing = createRoute({
    getParentRoute: () => root,
    path: '/marketplace/$kind/$listingId',
    component: () => null,
  });
  const router = createRouter({
    routeTree: root.addChildren([index, listing]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  const view = render(<RouterProvider router={router} />);
  await waitFor(() => expect(view.getByTestId('marketplace-origin-line')).toBeTruthy());
  return view;
}
