/**
 * The top of the Feedback section (ADR-0051 decision 5): the rating of the
 * chosen version as text, its distribution as five rows of text and bars, and
 * the signals beside it, so a high rating with a high dismissal rate shows.
 * Nothing here is colour or an icon alone.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { MarketplaceFeedback, MarketplaceRatingBucket, MarketplaceSignals } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { distributionRows, reviewRatingText, signalFigures } from './feedbackModel';

/** The bucket and the signals the filter points at: one version, or the whole listing. */
function chosenFigures(
  feedback: MarketplaceFeedback,
  versionId: string,
): { readonly bucket: MarketplaceRatingBucket; readonly signals: MarketplaceSignals | undefined } {
  if (versionId === '') return { bucket: feedback.overall, signals: feedback.signals.listing };
  return {
    bucket: feedback.versions.find((bucket) => bucket.version_id === versionId) ?? feedback.overall,
    signals: feedback.signals.versions[versionId],
  };
}

function Distribution({ bucket }: { readonly bucket: MarketplaceRatingBucket }): ReactNode {
  return (
    <Box component="ul" sx={listSx} aria-label={t('marketplace.feedback.distribution', 'Reviews by rating')}>
      {distributionRows(bucket).map((row) => (
        <Box component="li" key={row.stars} sx={rowSx} data-testid={`marketplace-feedback-stars-${row.stars}`}>
          <Typography component="span" variant="bodySmall" sx={starsSx}>
            {t('marketplace.feedback.starsLabel', '{{count}} stars', { count: row.stars })}
          </Typography>
          <Box aria-hidden="true" sx={trackSx}>
            <Box sx={{ height: '100%', backgroundColor: 'primary.main', width: `${row.share}%` }} />
          </Box>
          <Typography component="span" variant="bodySmall" sx={countSx}>
            {row.count}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function Signals({ signals }: { readonly signals: MarketplaceSignals | undefined }): ReactNode {
  return (
    <Box component="dl" sx={signalsSx} data-testid="marketplace-feedback-signals">
      {signalFigures(signals).map((figure) => (
        <Box key={figure.key} sx={figureSx}>
          <Typography component="dt" variant="bodySmall" color="text.secondary">
            {figure.label}
          </Typography>
          <Typography component="dd" variant="labelMedium" sx={{ margin: 0 }}>
            {figure.value}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

export function FeedbackSummary({
  feedback,
  versionId,
}: {
  readonly feedback: MarketplaceFeedback;
  readonly versionId: string;
}): ReactNode {
  const { bucket, signals } = chosenFigures(feedback, versionId);
  return (
    <Box sx={summarySx} data-testid="marketplace-feedback-summary">
      <Box sx={leftSx}>
        <Typography variant="bodyMedium" data-testid="marketplace-feedback-rating">
          {reviewRatingText(bucket)}
        </Typography>
        {bucket.report_count > 0 && (
          <Typography variant="bodySmall" color="text.secondary">
            {t('marketplace.feedback.reportCount', '{{count}} workforce reports', { count: bucket.report_count })}
          </Typography>
        )}
        <Distribution bucket={bucket} />
      </Box>
      <Signals signals={signals} />
    </Box>
  );
}

const summarySx: SxProps<Theme> = {
  display: 'flex',
  flexWrap: 'wrap',
  gap: '1.5rem 3rem',
  alignItems: 'flex-start',
};
const leftSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', minWidth: '16rem', flex: 1 };
const listSx: SxProps<Theme> = {
  listStyle: 'none',
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
};
const rowSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: '4.5rem 1fr 2rem',
  alignItems: 'center',
  gap: '0.5rem',
};
const starsSx: SxProps<Theme> = { color: 'text.secondary' };
const countSx: SxProps<Theme> = { textAlign: 'right' };
const trackSx: SxProps<Theme> = {
  height: '0.5rem',
  borderRadius: 'var(--el-shape-radiusSm, 0px)',
  backgroundColor: 'action.hover',
  overflow: 'hidden',
};
const signalsSx: SxProps<Theme> = {
  margin: 0,
  display: 'grid',
  gridTemplateColumns: 'repeat(2, minmax(8rem, 1fr))',
  gap: '0.75rem 1.5rem',
};
const figureSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column' };
