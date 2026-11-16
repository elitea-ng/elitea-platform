/**
 * One item of feedback (ADR-0051 decision 5): a review with its author's name
 * and rating, or a workforce report that reads "Workforce report from a hiring
 * project" and never names the project. Each says which version it is about.
 * The publisher's reply sits under it. The publisher and an administrator get
 * the steps they may take.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { MarketplaceFeedbackItem } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { FeedbackItemActions, type ItemRights } from './FeedbackItemActions';
import { itemRatingText, outcomeLabel, scorecardLines } from './feedbackModel';
import type { ActionRequest } from './useListingFeedback';

function ItemHead({ item }: { readonly item: MarketplaceFeedbackItem }): ReactNode {
  const isReview = item.kind === 'review';
  return (
    <Box sx={headSx}>
      <Typography component="span" variant="labelMedium">
        {isReview
          ? t('marketplace.feedback.reviewBy', 'Review by {{name}}', { name: item.author.name })
          : item.author.name}
      </Typography>
      <Typography component="span" variant="bodySmall" color="text.secondary">
        {t('marketplace.feedback.onVersion', 'on {{version}}', { version: item.version_name })}
      </Typography>
      <Typography component="span" variant="bodySmall" color="text.secondary">
        {new Date(item.created_at).toLocaleDateString()}
      </Typography>
      {isReview && (
        <Typography component="span" variant="bodySmall" data-testid={`marketplace-feedback-item-rating-${item.id}`}>
          {itemRatingText(item)}
        </Typography>
      )}
      {!isReview && item.outcome !== undefined && (
        <Chip size="small" variant="outlined" label={outcomeLabel(item.outcome)} />
      )}
      {item.author.kind === 'policy' && (
        <Chip
          size="small"
          variant="outlined"
          label={t('marketplace.feedback.autoPublished', 'Published by a project policy')}
        />
      )}
      {item.flagged && <Chip size="small" color="warning" label={t('marketplace.feedback.flagged', 'Flagged')} />}
      {item.state === 'hidden' && (
        <Chip size="small" color="error" label={t('marketplace.feedback.hidden', 'Hidden by an administrator')} />
      )}
      {item.mine && <Chip size="small" label={t('marketplace.feedback.mine', 'Yours')} />}
    </Box>
  );
}

function ReportFacts({ item }: { readonly item: MarketplaceFeedbackItem }): ReactNode {
  if (item.kind !== 'workforce_report') return null;
  const lines = scorecardLines(item.scorecard);
  const stage = item.stage ?? '';
  const reason = item.reason_code ?? '';
  if (lines.length === 0 && stage === '' && reason === '') return null;
  return (
    <Typography variant="bodySmall" color="text.secondary">
      {[
        stage === '' ? '' : t('marketplace.feedback.stage', 'Stage: {{stage}}', { stage }),
        reason === '' ? '' : t('marketplace.feedback.reason', 'Reason: {{reason}}', { reason }),
        ...lines,
      ]
        .filter((part) => part !== '')
        .join(' · ')}
    </Typography>
  );
}

export function FeedbackItemCard({
  item,
  rights,
  busy,
  onAction,
}: {
  readonly item: MarketplaceFeedbackItem;
  readonly rights: ItemRights;
  readonly busy: boolean;
  readonly onAction: (request: ActionRequest) => void;
}): ReactNode {
  return (
    <Box component="li" sx={cardSx} data-testid={`marketplace-feedback-item-${item.id}`}>
      <ItemHead item={item} />
      <ReportFacts item={item} />
      {item.body !== '' && (
        <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
          {item.body}
        </Typography>
      )}
      {item.flag_reason !== undefined && item.flag_reason !== '' && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.feedback.flagReason', 'Flag reason: {{reason}}', { reason: item.flag_reason })}
        </Typography>
      )}
      {item.hide_reason !== undefined && item.hide_reason !== '' && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.feedback.hideReason', 'Hidden because: {{reason}}', { reason: item.hide_reason })}
        </Typography>
      )}
      {item.reply !== null && (
        <Box sx={replySx} data-testid={`marketplace-feedback-reply-${item.id}`}>
          <Typography variant="labelSmall" color="text.secondary">
            {t('marketplace.feedback.publisherReply', 'Reply from the publisher')}
          </Typography>
          <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
            {item.reply.body}
          </Typography>
        </Box>
      )}
      <FeedbackItemActions item={item} rights={rights} busy={busy} onAction={onAction} />
    </Box>
  );
}

const cardSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  paddingBottom: '1rem',
  borderBottom: 1,
  borderColor: 'divider',
};
const headSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.5rem 0.75rem' };
const replySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  paddingLeft: '0.75rem',
  borderLeft: 2,
  borderColor: 'divider',
};
