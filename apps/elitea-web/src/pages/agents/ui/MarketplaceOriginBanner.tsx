import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import { Link } from '@tanstack/react-router';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The origin notices on top of a hired agent's editor (ADR-0051 decision 3).
 * Every notice is text, so a screen reader hears what a colour would say.
 * An installed agent says its definition is read-only and why; a forked one
 * says only where it came from. A yanked or deprecated version, a withdrawn
 * listing and a newer version each get their own line.
 */
export interface MarketplaceOriginBannerProps {
  readonly origin: MarketplaceOrigin;
}

export function MarketplaceOriginBanner({ origin }: MarketplaceOriginBannerProps): ReactNode {
  const installed = origin.mode === 'installed';
  const latest = origin.latest_version;
  const line = installed
    ? t(
        'pages.agents.origin.installed',
        'Installed from {{listing}} {{version}}. The definition is read-only. Fork it to change it.',
        {
          listing: origin.listing_name,
          version: origin.version_name,
        },
      )
    : t('pages.agents.origin.forked', 'Forked from {{listing}} {{version}}.', {
        listing: origin.listing_name,
        version: origin.version_name,
      });
  return (
    <Box
      component="section"
      aria-label={t('pages.agents.origin.groupLabel', 'Where this agent came from')}
      data-testid="marketplace-origin-banner"
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}
    >
      <Alert severity="info" data-testid="marketplace-origin-line">
        {line}{' '}
        <Link to="/marketplace/$kind/$listingId" params={{ kind: 'agents', listingId: origin.listing_id }}>
          {t('pages.agents.origin.openListing', 'Open the listing')}
        </Link>
      </Alert>
      {origin.listing_state === 'delisted' && (
        <Alert severity="warning" data-testid="marketplace-origin-withdrawn">
          {t(
            'pages.agents.origin.withdrawn',
            'Listing withdrawn. The publisher took it down. Your agent keeps running on its copy.',
          )}
        </Alert>
      )}
      {origin.version_state === 'yanked' && (
        <Alert severity="warning" data-testid="marketplace-origin-yanked">
          {t('pages.agents.origin.yanked', 'This version was yanked. Your agent keeps running.')}
        </Alert>
      )}
      {origin.version_state === 'deprecated' && (
        <Alert severity="warning" data-testid="marketplace-origin-deprecated">
          {t('pages.agents.origin.deprecated', 'This version is deprecated. Check the listing for a newer one.')}
        </Alert>
      )}
      {latest !== undefined && latest !== null && (
        <Alert severity="info" data-testid="marketplace-origin-update">
          {t('pages.agents.origin.updateAvailable', 'Update available: {{version}}.', { version: latest.name })}{' '}
          <Link
            to="/marketplace/$kind/$listingId"
            params={{ kind: 'agents', listingId: origin.listing_id }}
            hash="versions"
          >
            {t('pages.agents.origin.seeVersions', 'See the versions')}
          </Link>
        </Alert>
      )}
    </Box>
  );
}
