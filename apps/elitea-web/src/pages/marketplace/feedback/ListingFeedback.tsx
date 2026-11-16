/**
 * The listing page's **Feedback** section (ADR-0051 decision 5): the rating of
 * a version and its distribution, the signals beside it, the newest reviews and
 * workforce reports with a version filter, and the review form for a person
 * whose project hired the listing. Each item says what kind it is, so a reader
 * can find a dismissed agent's workforce report from here.
 *
 * A review names its author. A workforce report reads "from a hiring project".
 * No project name is ever on this page.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Typography from '@mui/material/Typography';

import type { MarketplaceFeedback, MarketplaceListingDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { FeedbackItemCard } from './FeedbackItemCard';
import { FeedbackSummary } from './FeedbackSummary';
import { ReviewForm } from './ReviewForm';
import { useFeedbackAction, useListingFeedback, type FeedbackFilters, type FeedbackKind } from './useListingFeedback';

function Filters({
  feedback,
  filters,
  onChange,
}: {
  readonly feedback: MarketplaceFeedback;
  readonly filters: FeedbackFilters;
  readonly onChange: (filters: FeedbackFilters) => void;
}): ReactNode {
  return (
    <Box sx={filtersSx}>
      <TextField
        select
        size="small"
        label={t('marketplace.feedback.versionFilter', 'Feedback for')}
        value={filters.versionId}
        onChange={(event) => onChange({ ...filters, versionId: event.target.value })}
        data-testid="marketplace-feedback-version"
        sx={{ minWidth: '10rem' }}
      >
        <MenuItem value="">{t('marketplace.feedback.allVersions', 'All versions')}</MenuItem>
        {feedback.versions.map((bucket) => (
          <MenuItem key={bucket.version_id} value={bucket.version_id}>
            {bucket.version_name}
          </MenuItem>
        ))}
      </TextField>
      <ToggleButtonGroup
        exclusive
        size="small"
        value={filters.kind}
        aria-label={t('marketplace.feedback.kindFilter', 'Kind of feedback')}
        onChange={(_event, kind: FeedbackKind | null) => kind !== null && onChange({ ...filters, kind })}
      >
        <ToggleButton value="all" data-testid="marketplace-feedback-kind-all">
          {t('marketplace.feedback.kindAll', 'All')}
        </ToggleButton>
        <ToggleButton value="review" data-testid="marketplace-feedback-kind-review">
          {t('marketplace.feedback.kindReviews', 'Reviews')}
        </ToggleButton>
        <ToggleButton value="workforce_report" data-testid="marketplace-feedback-kind-report">
          {t('marketplace.feedback.kindReports', 'Workforce reports')}
        </ToggleButton>
      </ToggleButtonGroup>
    </Box>
  );
}

function emptyText(filters: FeedbackFilters): string {
  if (filters.kind === 'workforce_report') return t('marketplace.feedback.noReports', 'No workforce reports yet.');
  if (filters.kind === 'review') return t('marketplace.feedback.noReviewsYet', 'No reviews yet.');
  return t('marketplace.feedback.none', 'No feedback yet. Reviews come from people whose project hired this listing.');
}

export function ListingFeedback({
  detail,
  projectId,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
}): ReactNode {
  const listingId = detail.listing.id;
  const [filters, setFilters] = useState<FeedbackFilters>({ versionId: '', kind: 'all' });
  const data = useListingFeedback(projectId, listingId, filters);
  const act = useFeedbackAction(projectId, listingId, () => undefined);
  const { first } = data;
  if (first === undefined) {
    if (data.isError) {
      return (
        <Alert
          severity="error"
          action={<Button onClick={data.refetch}>{t('marketplace.feedback.retry', 'Retry')}</Button>}
        >
          {t('marketplace.feedback.loadFailed', 'The feedback could not be loaded.')}
        </Alert>
      );
    }
    return <LinearProgress />;
  }
  const rights = { canReply: first.can_reply, canFlag: first.can_flag, canModerate: first.can_moderate };
  return (
    <Box sx={sectionSx} data-testid="marketplace-feedback">
      <FeedbackSummary feedback={first} versionId={filters.versionId} />
      <Typography variant="bodySmall" color="text.secondary">
        {t('marketplace.feedback.saved', 'Saved by {{count}}', { count: detail.listing.likes })}
        {' · '}
        {t('marketplace.feedback.hires', 'Hired by {{count}} projects', { count: detail.listing.hires })}
      </Typography>
      {first.can_review && (
        <ReviewForm
          key={first.my_review?.id ?? 'new'}
          projectId={projectId}
          listingId={listingId}
          versions={first.review_versions}
          existing={first.my_review ?? null}
        />
      )}
      <Filters feedback={first} filters={filters} onChange={setFilters} />
      {act.isError && (
        <Alert severity="error">{t('marketplace.feedback.stepFailed', 'The change failed. Try again.')}</Alert>
      )}
      {data.items.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid="marketplace-feedback-empty">
          {emptyText(filters)}
        </Typography>
      ) : (
        <Box component="ul" sx={listSx} data-testid="marketplace-feedback-list">
          {data.items.map((item) => (
            <FeedbackItemCard
              key={item.id}
              item={item}
              rights={rights}
              busy={act.isPending}
              onAction={(request) => act.mutate(request)}
            />
          ))}
        </Box>
      )}
      {data.hasNextPage && (
        <Button
          size="small"
          sx={{ alignSelf: 'flex-start' }}
          disabled={data.isFetchingNextPage}
          onClick={data.fetchNextPage}
          data-testid="marketplace-feedback-more"
        >
          {t('marketplace.feedback.showMore', 'Show more')}
        </Button>
      )}
    </Box>
  );
}

const sectionSx = { display: 'flex', flexDirection: 'column', gap: 2 } as const;
const filtersSx = { display: 'flex', gap: 2, flexWrap: 'wrap', alignItems: 'center' } as const;
const listSx = {
  listStyle: 'none',
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
} as const;
