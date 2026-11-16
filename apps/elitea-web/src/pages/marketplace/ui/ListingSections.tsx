/**
 * The smaller sections of a listing page (ADR-0051 decision 8): the delisted
 * banner, the administrator's Feature and Delist controls, Feedback and **Used
 * by this project**.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';

import { updateMarketplaceListing } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceHired, MarketplaceListingDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { invalidateMarketplace } from '../invalidate';
import { ratingText } from '../listing';
import type { MarketplaceKind } from '../types';

/** What a delisted listing means for the reader. */
export function DelistedBanner({ detail }: { readonly detail: MarketplaceListingDetail }): ReactNode {
  if (detail.listing.state !== 'delisted') return null;
  return (
    <Alert severity="warning" data-testid="marketplace-delisted">
      {t(
        'marketplace.listing.delisted',
        'The publisher withdrew this listing. It cannot be hired. Agents installed from it keep running.',
      )}
    </Alert>
  );
}

/** Feature and Delist: a platform administrator's two edits of a listing. */
export function ListingAdminBar({
  projectId,
  detail,
}: {
  readonly projectId: string | undefined;
  readonly detail: MarketplaceListingDetail;
}): ReactNode {
  const queryClient = useQueryClient();
  const update = useMutation({
    mutationFn: (body: { readonly featured?: boolean; readonly state?: string }) =>
      updateMarketplaceListing(projectId ?? '', detail.listing.id, body),
    onSuccess: () => invalidateMarketplace(queryClient),
  });
  if (!detail.can_moderate) return null;
  const { featured, state } = detail.listing;
  return (
    <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap', alignItems: 'center' }} data-testid="marketplace-admin-bar">
      <Button
        size="small"
        variant="outlined"
        disabled={update.isPending}
        onClick={() => update.mutate({ featured: !featured })}
        data-testid="marketplace-feature"
      >
        {featured
          ? t('marketplace.admin.unfeature', 'Remove from Featured')
          : t('marketplace.admin.feature', 'Feature')}
      </Button>
      <Button
        size="small"
        variant="outlined"
        color="warning"
        disabled={update.isPending}
        onClick={() => update.mutate({ state: state === 'delisted' ? 'listed' : 'delisted' })}
        data-testid="marketplace-delist"
      >
        {state === 'delisted' ? t('marketplace.admin.list', 'List again') : t('marketplace.admin.delist', 'Delist')}
      </Button>
      {update.isError && (
        <Typography variant="bodySmall" color="error">
          {t('marketplace.admin.failed', 'The change failed. Try again.')}
        </Typography>
      )}
    </Box>
  );
}

/** Feedback: the rating as text and the saves. Reviews and workforce reports arrive in a later phase. */
export function ListingFeedback({ detail }: { readonly detail: MarketplaceListingDetail }): ReactNode {
  const { listing } = detail;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="marketplace-feedback">
      <Typography variant="bodyMedium">
        {ratingText({ ratingAvg: listing.rating_avg ?? null, ratingCount: listing.rating_count })}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('marketplace.feedback.saved', 'Saved by {{count}}', { count: listing.likes })}
        {' · '}
        {t('marketplace.feedback.hires', 'Hired by {{count}} projects', { count: listing.hires })}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('marketplace.feedback.later', 'Reviews and workforce reports from hiring projects come in a later release.')}
      </Typography>
    </Box>
  );
}

function usedByLabel(entry: MarketplaceHired): string {
  return t('marketplace.usedBy.item', '{{mode}} {{version}}', {
    mode:
      entry.mode === 'fork' ? t('marketplace.usedBy.fork', 'Fork of') : t('marketplace.usedBy.install', 'Installed'),
    version: entry.version_name,
  });
}

/** Used by this project: the agents this project hired from the listing. */
export function ListingUsedBy({
  hired,
  kind,
}: {
  readonly hired: readonly MarketplaceHired[];
  readonly kind: MarketplaceKind;
}): ReactNode {
  if (hired.length === 0) return null;
  return (
    <Box component="ul" sx={{ margin: 0, paddingLeft: '1.25rem' }} data-testid="marketplace-used-by">
      {hired.map((entry) => (
        <li key={`${entry.mode}-${entry.application_id}`}>
          {kind === 'skills' ? (
            <Link to="/skills/$tab/$skillId" params={{ tab: 'all', skillId: entry.application_id }}>
              {usedByLabel(entry)}
            </Link>
          ) : (
            <Link to="/agents/$tab/$agentId" params={{ tab: 'all', agentId: entry.application_id }}>
              {usedByLabel(entry)}
            </Link>
          )}
        </li>
      ))}
    </Box>
  );
}
