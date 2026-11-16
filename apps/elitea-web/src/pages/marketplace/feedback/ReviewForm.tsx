/**
 * A verified hire's review (ADR-0051 decision 5): a rating from 1 to 5, an
 * optional text, and the hired version it is about. A person has one review per
 * listing, so a saved review opens here to be edited. The rating is a radio
 * group whose every option is a word, so a screen reader and a keyboard reach it.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControl from '@mui/material/FormControl';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormLabel from '@mui/material/FormLabel';
import MenuItem from '@mui/material/MenuItem';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { MarketplaceFeedbackItem, MarketplaceReviewVersion } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useSaveReview, type ReviewDraft } from './useListingFeedback';

interface ReviewFormProps {
  readonly projectId: string | undefined;
  readonly listingId: string;
  /** The versions the caller's project hired, newest first. */
  readonly versions: readonly MarketplaceReviewVersion[];
  /** The caller's saved review, when they wrote one. */
  readonly existing: MarketplaceFeedbackItem | null;
}

function RatingField({
  rating,
  onChange,
}: {
  readonly rating: number;
  readonly onChange: (rating: number) => void;
}): ReactNode {
  return (
    <FormControl>
      <FormLabel id="marketplace-review-rating-label">{t('marketplace.feedback.ratingLabel', 'Your rating')}</FormLabel>
      <RadioGroup
        row
        aria-labelledby="marketplace-review-rating-label"
        value={rating === 0 ? '' : String(rating)}
        onChange={(event) => onChange(Number(event.target.value))}
      >
        {[1, 2, 3, 4, 5].map((stars) => (
          <FormControlLabel
            key={stars}
            value={String(stars)}
            control={<Radio size="small" />}
            label={t('marketplace.feedback.ratingOption', '{{count}} out of 5', { count: stars })}
          />
        ))}
      </RadioGroup>
    </FormControl>
  );
}

function VersionField({
  versions,
  value,
  onChange,
}: {
  readonly versions: readonly MarketplaceReviewVersion[];
  readonly value: string;
  readonly onChange: (versionId: string) => void;
}): ReactNode {
  if (versions.length < 2) return null;
  return (
    <TextField
      select
      size="small"
      label={t('marketplace.feedback.reviewVersion', 'Version you used')}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      sx={{ maxWidth: '16rem' }}
    >
      {versions.map((version) => (
        <MenuItem key={version.id} value={version.id}>
          {version.name}
        </MenuItem>
      ))}
    </TextField>
  );
}

/** The draft a form starts from: the saved review, or the newest hired version and nothing else. */
function initialDraft(
  existing: MarketplaceFeedbackItem | null,
  versions: readonly MarketplaceReviewVersion[],
): ReviewDraft {
  if (existing === null) return { versionId: versions[0]?.id ?? '', rating: 0, body: '' };
  // The saved version may be one this project did not hire (the review is per person, the hire per
  // project), and the server refuses a version it did not hire.
  const hired = versions.some((version) => version.id === existing.version_id);
  return {
    versionId: hired ? existing.version_id : (versions[0]?.id ?? ''),
    rating: existing.rating ?? 0,
    body: existing.body,
  };
}

export function ReviewForm({ projectId, listingId, versions, existing }: ReviewFormProps): ReactNode {
  const [draft, setDraft] = useState(() => initialDraft(existing, versions));
  const [saved, setSaved] = useState(false);
  const save = useSaveReview(projectId, listingId, () => setSaved(true));
  const edit = (change: Partial<ReviewDraft>): void => {
    setSaved(false);
    setDraft((current) => ({ ...current, ...change }));
  };
  const isNew = existing === null;
  return (
    <Box
      component="form"
      sx={formSx}
      data-testid="marketplace-review-form"
      onSubmit={(event) => {
        event.preventDefault();
        if (draft.rating >= 1 && !save.isPending) save.mutate(draft);
      }}
    >
      <Typography component="h3" variant="labelMedium">
        {isNew
          ? t('marketplace.feedback.writeReview', 'Review this listing')
          : t('marketplace.feedback.editReview', 'Edit your review')}
      </Typography>
      <VersionField versions={versions} value={draft.versionId} onChange={(versionId) => edit({ versionId })} />
      <RatingField rating={draft.rating} onChange={(rating) => edit({ rating })} />
      <TextField
        multiline
        minRows={3}
        size="small"
        label={t('marketplace.feedback.reviewText', 'What worked and what did not (optional)')}
        value={draft.body}
        onChange={(event) => edit({ body: event.target.value })}
        slotProps={{ htmlInput: { maxLength: 2000 } }}
      />
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
        <Button
          type="submit"
          variant="contained"
          size="small"
          disabled={draft.rating < 1 || save.isPending}
          data-testid="marketplace-review-save"
        >
          {isNew
            ? t('marketplace.feedback.postReview', 'Post review')
            : t('marketplace.feedback.updateReview', 'Update review')}
        </Button>
        {saved && (
          <Typography component="output" variant="bodySmall">
            {t('marketplace.feedback.reviewSaved', 'Your review is saved.')}
          </Typography>
        )}
      </Box>
      {save.isError && (
        <Alert severity="error">
          {t('marketplace.feedback.reviewFailed', 'The review could not be saved. Try again.')}
        </Alert>
      )}
    </Box>
  );
}

const formSx = { display: 'flex', flexDirection: 'column', gap: 1.5, maxWidth: '36rem' } as const;
