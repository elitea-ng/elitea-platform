/**
 * OnboardingTour — the full-screen tip carousel displayed after the user opts in.
 * Port of `apps/elitea-ui/src/[fsd]/features/onboarding/ui/OnboardingTour.jsx`
 * (Wave-2 unit A13).
 *
 * Replaces the old app's chunked lazy-load (`ChunkHelpers.lazyWithRetry`) with
 * a direct import. Full-screen dialog provides ESC-to-close and a paper style
 * matching the brand palette.
 *
 * Note: MUI's standard IconButton does not accept `variant`. The old app used
 * a custom IconButton that did.  We remove `variant`/`color` from the
 * IconButton calls and style via `sx` props instead.
 */

import { memo, useState } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import FullscreenOutlinedIcon from '@mui/icons-material/FullscreenOutlined';
import { Box, Dialog, DialogContent, IconButton, Typography } from '@mui/material';

import TourContent from './TourContent';
import { onboardingTips } from '../lib/constants/onboardingTips.constants';

const OnboardingTour = memo(() => {
  const [currentStep, setCurrentStep] = useState(1);
  const [isTourFullScreen, setIsTourFullScreen] = useState(false);

  const onNext = () => {
    if (currentStep < onboardingTips.length) {
      setCurrentStep(currentStep + 1);
    }
  };

  const onPrevious = () => {
    if (currentStep > 1) {
      setCurrentStep(currentStep - 1);
    }
  };

  const onCloseTourFullScreen = () => {
    setIsTourFullScreen(false);
  };

  const handleKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      onCloseTourFullScreen();
    }
  };

  return (
    <>
      <Box sx={styles.wrapper}>
        <IconButton
          onClick={() => setIsTourFullScreen(true)}
          sx={styles.tourFullScreenButton}
          aria-label="View tour in full screen"
        >
          <FullscreenOutlinedIcon fontSize="small" />
        </IconButton>
        <Box sx={styles.container}>
          <TourContent
            currentStep={currentStep}
            onNext={onNext}
            onPrevious={onPrevious}
          />
        </Box>
      </Box>

      <Dialog
        fullScreen
        open={isTourFullScreen}
        onClose={onCloseTourFullScreen}
        onKeyDown={handleKeyDown}
        slotProps={tourDialogSlotProps}
      >
        <Box sx={styles.tourDialogHeader}>
          <Typography
            color="text.secondary"
            variant="headingMedium"
          >
            Onboarding tips
          </Typography>
          <IconButton
            onClick={onCloseTourFullScreen}
            aria-label="Close full screen tour"
            sx={styles.closeButton}
          >
            <CloseIcon sx={styles.closeIcon} />
          </IconButton>
        </Box>

        <DialogContent sx={styles.tourDialogContent}>
          <Box sx={styles.tourContentWrapper}>
            <TourContent
              currentStep={currentStep}
              onNext={onNext}
              onPrevious={onPrevious}
            />
          </Box>
        </DialogContent>
      </Dialog>
    </>
  );
});

OnboardingTour.displayName = 'OnboardingTour';

const styles = {
  wrapper: {
    position: 'relative',
    display: 'flex',
    flexDirection: 'column',
    width: '100%',
    height: '100%',
    flex: 1,
  },
  container: {
    display: 'flex',
    flexDirection: 'column',
    justifyContent: 'flex-start',
    alignItems: 'center',
    width: '100%',
    boxSizing: 'border-box' as const,
    gap: '1.5rem',
    flex: 1,
    overflow: 'hidden',
  },
  tourFullScreenButton: {
    position: 'absolute' as const,
    top: 0,
    right: 0,
    zIndex: 100,
    backgroundColor: 'background.paper',
    '&:hover': {
      backgroundColor: 'action.hover',
    },
  },
  closeButton: {
    marginLeft: '0rem',
  },
  closeIcon: {
    width: '1rem',
    height: '1rem',
  },
  tourDialogPaper: {
    backgroundColor: 'background.default',
  },
  tourDialogHeader: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    padding: '1.5rem 2rem',
    borderBottom: (theme: { palette: NonNullable<unknown> }) =>
      `.0625rem solid ${(theme.palette as Record<string, Record<string, string>>).border?.lines ?? '#ccc'}`,
    backgroundColor: 'background.secondary',
  },
  tourDialogContent: {
    padding: '3rem',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: 'background.default',
  },
  tourContentWrapper: {
    display: 'flex',
    flexDirection: 'column',
    justifyContent: 'center',
    alignItems: 'center',
    width: '100%',
    maxWidth: '80rem',
    height: '100%',
    boxSizing: 'border-box' as const,
    gap: '2rem',
  },
};

const tourDialogSlotProps = {
  paper: {
    sx: styles.tourDialogPaper,
  },
};

/** @public Shared dialog-paper styling consumed by the full-screen overlay. */
export { tourDialogSlotProps };

export default OnboardingTour;
