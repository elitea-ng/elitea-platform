import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { typeScale } from '@/shared/brand/typography';

/**
 * Small local stand-in for the baseline's `ComponentsLib/CircularProgress.jsx`
 * `StyledCircleProgress` — that file (and its sibling `ComponentsLib/
 * Tooltip.jsx`, substituted with a plain MUI `Tooltip` in `UserInput.tsx`)
 * is a generic, non-chat-specific MUI wrapper not owned by any Wave-2 unit
 * (absent from `parity/wave2-partition.json`). Kept local and this small
 * rather than inventing a new `shared/ui` component for a two-branch
 * presentational leaf with one consumer.
 */
export function UploadProgressIndicator({ progress }: { readonly progress?: number | undefined }): ReactNode {
  if (typeof progress === 'number') {
    return (
      <Box
        component="span"
        sx={wrapperSx}
      >
        <CircularProgress
          variant="determinate"
          value={progress}
          sx={circleSx}
        />
        <Typography sx={labelSx}>{Math.round(progress)}%</Typography>
      </Box>
    );
  }
  return <CircularProgress sx={circleSx} />;
}

const wrapperSx: SxProps<Theme> = {
  position: 'absolute',
  zIndex: 999,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
};

const circleSx: SxProps<Theme> = { position: 'absolute', zIndex: 999 };

const labelSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  ...typeScale(theme.typography.labelSmall),
  fontWeight: 600,
  color: theme.vars.palette.primary.main,
  lineHeight: 1,
  backgroundColor: theme.vars.palette.background.aiAnswerBkg,
  borderRadius: theme.vars.shape.radiusPill,
  width: '2rem',
  height: '2rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  top: '50%',
  left: '50%',
  transform: 'translate(-50%, -50%)',
});
