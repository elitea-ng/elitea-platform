/**
 * TourContent component — renders the current tip image, markdown text, and
 * navigation buttons for the onboarding tip carousel.
 * Port of `apps/elitea-ui/src/[fsd]/features/onboarding/ui/TourContent.jsx`
 * (Wave-2 unit A13).
 *
 * Uses MUI's standard IconButton (no custom `variant` prop — the old app had
 * a custom IconButton that accepted it).
 */

import { memo } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import ArrowForwardIcon from '@mui/icons-material/ArrowForward';
import { Box, IconButton, Typography } from '@mui/material';

import { onboardingTips } from '@/features/onboarding/lib/constants/onboardingTips.constants';
import { Markdown } from '@/shared/ui/Markdown';
import type { Theme } from '@mui/material/styles';

/** Props for {@link TourContent}. */
interface TourContentProps {
  currentStep: number;
  onNext: () => void;
  onPrevious: () => void;
}

const TourContent = memo<TourContentProps>(({ currentStep, onNext, onPrevious }) => {
  const tipIndex = currentStep - 1;
  const tip = onboardingTips[tipIndex];

  if (!tip) {
    return null;
  }

  return (
    <>
      <Box sx={styles.imageWrapper}>
        <Box
          component="img"
          src={tip.image}
          alt={`Elitea tip ${currentStep}`}
          sx={styles.image}
        />
      </Box>
      <Typography
        component="div"
        variant="bodyMedium"
        sx={styles.title}
      >
        <Markdown>{tip.tip}</Markdown>
      </Typography>
      <Box sx={styles.footer}>
        <IconButton
          onClick={onPrevious}
          disabled={currentStep === 1}
          aria-label="Previous tip"
          sx={styles.navButton}
        >
          <ArrowBackIcon />
        </IconButton>
        <Typography
          variant="bodyMedium"
          sx={styles.pageIndicator}
        >
          {currentStep} / {onboardingTips.length}
        </Typography>
        <IconButton
          onClick={onNext}
          disabled={currentStep === onboardingTips.length}
          aria-label="Next tip"
          sx={styles.navButton}
        >
          <ArrowForwardIcon />
        </IconButton>
      </Box>
    </>
  );
});

TourContent.displayName = 'TourContent';

const styles = {
  imageWrapper: {
    position: 'relative',
    flex: 1,
    width: '100%',
    minHeight: 0,
    display: 'flex',
  },
  image: {
    flex: 1,
    width: '100%',
    maxWidth: '100%',
    minHeight: 0,
    objectFit: 'contain',
  },
  title: {
    flexShrink: 0,
    width: '100%',
    textAlign: 'center',
    color: 'text.secondary',
    '& p': {
      marginBottom: '0.25rem',
    },
  },
  footer: {
    flexShrink: 0,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '1rem',
    width: '100%',
  },
  navButton: {
    minWidth: '2rem',
    width: '2rem',
    height: '2rem',
    padding: 0,
    fontSize: (theme: Theme) => theme.typography.headingLarge.fontSize,
    marginLeft: '0rem',
    color: 'text.secondary',
    '&:disabled': {
      color: 'text.disabled',
    },
  },
  pageIndicator: {
    color: 'text.primary',
    minWidth: '3rem',
    textAlign: 'center',
  },
};

export default TourContent;
